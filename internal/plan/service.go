package plan

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

func (s *Service) Create(ctx context.Context, input domain.PlanCreateInput) (domain.RolloutPlan, error) {
	if err := domain.ValidatePlanInput(input); err != nil {
		return domain.RolloutPlan{}, err
	}
	input = domain.NormalizePlanInput(input)
	now := s.clock.Now().UTC()
	plan := domain.RolloutPlan{
		ID:                  s.ids.New("pln"),
		Zone:                input.Zone,
		State:               domain.PlanDraft,
		MinimumSuccessRatio: input.MinimumSuccessRatio,
		MaximumLatencyMS:    input.MaximumLatencyMS,
		ExpectedNodes:       input.ExpectedNodes,
		Version:             1,
		CreatedAt:           now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		state.Plans[plan.ID] = plan
		store.AddAudit(state, s.ids, "plan", plan.ID, "created", now, map[string]any{
			"zone": plan.Zone,
		})
		return nil
	})
	return plan, err
}

func (s *Service) AddRevision(
	ctx context.Context,
	planID string,
	input domain.RevisionCreateInput,
) (domain.ZoneRevision, error) {
	return s.addRevision(ctx, planID, input, nil)
}

// ImportRevision writes a revision prepared from an offline zone file. It goes
// through the same normalization and validation as AddRevision; the source
// attributes are attached to the audit event so imported revisions remain
// traceable. It is the single "formal model" write path for imports.
func (s *Service) ImportRevision(
	ctx context.Context,
	planID string,
	input domain.RevisionCreateInput,
	source ImportSource,
) (domain.ZoneRevision, error) {
	return s.addRevision(ctx, planID, input, &source)
}

// ImportSource records where an imported revision came from.
type ImportSource struct {
	FileName        string
	FileFingerprint string
	ContentDigest   string
	ExpectedVersion uint64
}

// RevisionPreview is the dry-run view shown before a revision is written.
type RevisionPreview struct {
	PlanID        string
	PlanState     domain.PlanState
	PlanVersion   uint64
	Serial        uint32
	Authority     string
	Entries       []domain.ZoneEntry
	EntryCount    int
	ContentDigest string
	// DuplicateRevisionID is set when identical content already exists.
	DuplicateRevisionID string
	// DuplicateSerial is the serial carried by the existing content-matched
	// revision (equal to Serial for a same-serial replay).
	DuplicateSerial uint32
	// DuplicateSameSerial distinguishes a same-serial exact replay (the hard
	// duplicate_revision rule) from a cross-serial content match (a file that
	// was imported before under another serial).
	DuplicateSameSerial bool
	LatestSerial        uint32
}

// PreviewRevision performs every revision check in memory without modifying
// state. Offline imports use it to render the "what would be created" summary
// and to recognize repeated imports: an exact content match returns the
// existing revision id instead of proposing a new one.
func (s *Service) PreviewRevision(
	ctx context.Context,
	planID string,
	input domain.RevisionCreateInput,
) (RevisionPreview, error) {
	normalized, err := domain.NormalizeRevisionInput(input)
	if err != nil {
		return RevisionPreview{}, err
	}
	if err := domain.ValidateRevisionInput(normalized); err != nil {
		return RevisionPreview{}, err
	}
	preview := RevisionPreview{
		PlanID:        planID,
		Serial:        normalized.Serial,
		Authority:     normalized.Authority,
		Entries:       append([]domain.ZoneEntry(nil), normalized.Entries...),
		EntryCount:    len(normalized.Entries),
		ContentDigest: RevisionDigestInput(normalized),
	}
	err = s.store.Read(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		preview.PlanState = plan.State
		preview.PlanVersion = plan.Version
		check, checkErr := checkRevisionCandidates(state, planID, normalized)
		if checkErr != nil {
			return checkErr
		}
		if check.sameSerialMatchID != "" {
			preview.DuplicateRevisionID = check.sameSerialMatchID
			preview.DuplicateSameSerial = true
			preview.DuplicateSerial = normalized.Serial
		} else if check.contentMatchID != "" {
			preview.DuplicateRevisionID = check.contentMatchID
			if existing, ok := state.Revisions[check.contentMatchID]; ok {
				preview.DuplicateSerial = existing.Serial
			}
		}
		preview.LatestSerial = check.latestSerial
		return nil
	})
	return preview, err
}

type revisionCheck struct {
	// sameSerialMatchID is an existing revision with the same serial AND exact
	// content: the idempotent replay case shared by JSON and file imports.
	sameSerialMatchID string
	// contentMatchID is an existing revision with different serial but exact
	// normalized content. Offline imports use it to recognize a file that was
	// already imported; the JSON endpoint treats another serial as new.
	contentMatchID string
	latestSerial   uint32
}

func (s *Service) addRevision(
	ctx context.Context,
	planID string,
	input domain.RevisionCreateInput,
	source *ImportSource,
) (domain.ZoneRevision, error) {
	normalized, err := domain.NormalizeRevisionInput(input)
	if err != nil {
		return domain.ZoneRevision{}, err
	}
	if err := domain.ValidateRevisionInput(normalized); err != nil {
		return domain.ZoneRevision{}, err
	}
	now := s.clock.Now().UTC()
	var created domain.ZoneRevision
	err = s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if source != nil && source.ExpectedVersion != 0 && plan.Version != source.ExpectedVersion {
			return versionConflict(planID, source.ExpectedVersion, plan.Version)
		}
		if plan.State != domain.PlanDraft {
			return domain.InvalidState("revisions can be added only while plan is draft", map[string]any{
				"plan_id": planID,
				"state":   plan.State,
			})
		}
		check, err := checkRevisionCandidates(state, planID, normalized)
		if err != nil {
			return err
		}
		if check.sameSerialMatchID != "" {
			return domain.NewError(domain.CodeDuplicateRevision, "revision content already exists for this plan", map[string]any{
				"plan_id":     planID,
				"serial":      normalized.Serial,
				"revision_id": check.sameSerialMatchID,
			})
		}
		created = domain.ZoneRevision{
			ID:         s.ids.New("rev"),
			PlanID:     planID,
			Serial:     normalized.Serial,
			Authority:  normalized.Authority,
			Entries:    append([]domain.ZoneEntry(nil), normalized.Entries...),
			EntryCount: len(normalized.Entries),
			CreatedAt:  now,
		}
		state.Revisions[created.ID] = created
		plan.LatestRevisionID = created.ID
		plan.Version++
		state.Plans[planID] = plan
		attributes := map[string]any{
			"plan_id": planID,
			"serial":  created.Serial,
		}
		if source != nil {
			attributes["source"] = "zone_file"
			attributes["file_name"] = source.FileName
			attributes["file_fingerprint"] = source.FileFingerprint
			attributes["content_digest"] = source.ContentDigest
		}
		store.AddAudit(state, s.ids, "revision", created.ID, "added", now, attributes)
		return nil
	})
	return created, err
}

// checkRevisionCandidates enforces the plan's serial rules and computes
// content identity. Same serial with different content rejects the replay;
// same serial with identical content is the idempotent duplicate shared by
// both entry points. Different serial with identical content is recorded
// separately so offline imports can recognize a re-imported file without
// changing the JSON endpoint's semantics (a new serial is a new revision).
func checkRevisionCandidates(state *domain.State, planID string, normalized domain.RevisionCreateInput) (revisionCheck, error) {
	result := revisionCheck{}
	signature := contentSignature(normalized.Authority, normalized.Entries)
	for _, revision := range state.Revisions {
		if revision.PlanID != planID {
			continue
		}
		if revision.Serial > result.latestSerial {
			result.latestSerial = revision.Serial
		}
		sameContent := contentSignature(revision.Authority, revision.Entries) == signature
		if revision.Serial == normalized.Serial {
			if sameContent {
				result.sameSerialMatchID = revision.ID
				continue
			}
			return result, domain.NewError(domain.CodeDuplicateRevision, "plan already contains this serial with different content", map[string]any{
				"plan_id": planID,
				"serial":  normalized.Serial,
			})
		}
		if revision.Serial > normalized.Serial {
			return result, domain.Validation("revision serial must increase", map[string]any{
				"serial": normalized.Serial,
				"latest": revision.Serial,
			})
		}
		if sameContent && result.contentMatchID == "" {
			result.contentMatchID = revision.ID
		}
	}
	return result, nil
}

func (s *Service) Activate(
	ctx context.Context,
	planID string,
	expectedVersion uint64,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if current.Version != expectedVersion {
			return versionConflict(planID, expectedVersion, current.Version)
		}
		now := s.clock.Now().UTC()
		if err := domain.ActivatePlan(&current, now); err != nil {
			return err
		}
		state.Plans[planID] = current
		store.AddAudit(state, s.ids, "plan", planID, "activated", now, map[string]any{
			"version": current.Version,
		})
		plan = current
		return nil
	})
	return plan, err
}

func (s *Service) Complete(
	ctx context.Context,
	planID string,
	expectedVersion uint64,
) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if current.Version != expectedVersion {
			return versionConflict(planID, expectedVersion, current.Version)
		}
		now := s.clock.Now().UTC()
		if err := domain.CompletePlan(&current, now); err != nil {
			return err
		}
		state.Plans[planID] = current
		store.AddAudit(state, s.ids, "plan", planID, "completed", now, map[string]any{
			"version": current.Version,
		})
		plan = current
		return nil
	})
	return plan, err
}

func (s *Service) Get(ctx context.Context, planID string) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := s.store.Read(ctx, func(state *domain.State) error {
		found, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		plan = found
		return nil
	})
	return plan, err
}

func (s *Service) LatestRevision(
	ctx context.Context,
	planID string,
) (domain.ZoneRevision, error) {
	var revision domain.ZoneRevision
	err := s.store.Read(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if plan.LatestRevisionID == "" {
			return domain.NotFound("revision", planID)
		}
		found, ok := state.Revisions[plan.LatestRevisionID]
		if !ok {
			return domain.NewError(domain.CodeStorageFailure, "plan references a missing revision", map[string]any{
				"revision_id": plan.LatestRevisionID,
			})
		}
		revision = found
		return nil
	})
	return revision, err
}

func (s *Service) List(
	ctx context.Context,
	stateFilter domain.PlanState,
	limit int,
	cursor string,
) (domain.PlanPage, error) {
	if limit < 1 || limit > 100 {
		return domain.PlanPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.PlanPage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.PlanPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.RolloutPlan, 0, len(state.Plans))
		for _, plan := range state.Plans {
			if stateFilter != "" && plan.State != stateFilter {
				continue
			}
			if after != "" && plan.ID <= after {
				continue
			}
			items = append(items, plan)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

func (s *Service) RevisionExists(
	ctx context.Context,
	planID string,
	serial uint32,
	authority string,
	entries []domain.ZoneEntry,
) (bool, error) {
	exists := false
	err := s.store.Read(ctx, func(state *domain.State) error {
		for _, revision := range state.Revisions {
			if revision.PlanID != planID || revision.Serial != serial || revision.Authority != authority {
				continue
			}
			if reflect.DeepEqual(revision.Entries, entries) {
				exists = true
				return nil
			}
		}
		return nil
	})
	return exists, err
}

func RevisionDigest(revision domain.ZoneRevision) string {
	payload, _ := json.Marshal(struct {
		Serial    uint32
		Authority string
		Entries   []domain.ZoneEntry
	}{
		Serial:    revision.Serial,
		Authority: revision.Authority,
		Entries:   revision.Entries,
	})
	return string(payload)
}

// RevisionDigestInput computes the same identity payload for a normalized
// revision candidate so preview and commit compare identical bytes.
func RevisionDigestInput(input domain.RevisionCreateInput) string {
	payload, _ := json.Marshal(struct {
		Serial    uint32
		Authority string
		Entries   []domain.ZoneEntry
	}{
		Serial:    input.Serial,
		Authority: input.Authority,
		Entries:   input.Entries,
	})
	return string(payload)
}

// contentSignature identifies revision content independently of serial:
// authority plus the normalized entries. It recognizes a zone file imported
// again under a new serial.
func contentSignature(authority string, entries []domain.ZoneEntry) string {
	payload, _ := json.Marshal(struct {
		Authority string
		Entries   []domain.ZoneEntry
	}{
		Authority: authority,
		Entries:   entries,
	})
	return string(payload)
}

func versionConflict(id string, expected uint64, actual uint64) *domain.DomainError {
	return domain.NewError(domain.CodeVersionConflict, "plan version does not match", map[string]any{
		"plan_id":  id,
		"expected": expected,
		"actual":   actual,
	})
}
