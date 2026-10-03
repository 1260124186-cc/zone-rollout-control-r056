package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	return s.addRevision(ctx, planID, 0, false, input)
}

// ImportRevision appends a revision on behalf of the offline zone file
// import. The write is rejected when the plan version moved past
// expectedVersion, so a stale preview can never overwrite a changed plan.
func (s *Service) ImportRevision(
	ctx context.Context,
	planID string,
	expectedVersion uint64,
	input domain.RevisionCreateInput,
) (domain.ZoneRevision, error) {
	return s.addRevision(ctx, planID, expectedVersion, true, input)
}

func (s *Service) addRevision(
	ctx context.Context,
	planID string,
	expectedVersion uint64,
	checkVersion bool,
	input domain.RevisionCreateInput,
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
		if checkVersion && plan.Version != expectedVersion {
			return versionConflict(planID, expectedVersion, plan.Version)
		}
		if plan.State != domain.PlanDraft {
			return domain.InvalidState("revisions can be added only while plan is draft", map[string]any{
				"plan_id": planID,
				"state":   plan.State,
			})
		}
		for _, revision := range state.Revisions {
			if revision.PlanID != planID {
				continue
			}
			if revision.Serial == normalized.Serial &&
				revision.Authority == normalized.Authority &&
				reflect.DeepEqual(revision.Entries, normalized.Entries) {
				return domain.NewError(domain.CodeDuplicateRevision, "plan already contains an identical revision", map[string]any{
					"plan_id":     planID,
					"revision_id": revision.ID,
					"serial":      normalized.Serial,
				})
			}
		}
		for _, revision := range state.Revisions {
			if revision.PlanID != planID {
				continue
			}
			if revision.Serial == normalized.Serial {
				return domain.NewError(domain.CodeDuplicateRevision, "plan already contains this serial", map[string]any{
					"plan_id": planID,
					"serial":  normalized.Serial,
				})
			}
			if revision.Serial > normalized.Serial {
				return domain.Validation("revision serial must increase", map[string]any{
					"serial": normalized.Serial,
					"latest": revision.Serial,
				})
			}
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
		store.AddAudit(state, s.ids, "revision", created.ID, "added", now, map[string]any{
			"plan_id": planID,
			"serial":  created.Serial,
		})
		return nil
	})
	return created, err
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

// FindRevision returns the stored revision whose content matches the given
// serial, authority, and entries, so a repeated import of the same zone file
// is recognized before anything is written.
func (s *Service) FindRevision(
	ctx context.Context,
	planID string,
	serial uint32,
	authority string,
	entries []domain.ZoneEntry,
) (domain.ZoneRevision, bool, error) {
	var found domain.ZoneRevision
	exists := false
	err := s.store.Read(ctx, func(state *domain.State) error {
		for _, revision := range state.Revisions {
			if revision.PlanID != planID || revision.Serial != serial || revision.Authority != authority {
				continue
			}
			if reflect.DeepEqual(revision.Entries, entries) {
				found = revision
				exists = true
				return nil
			}
		}
		return nil
	})
	return found, exists, err
}

// RevisionContentDigest identifies normalized revision content with a stable
// fingerprint, so import previews can show exactly what would be written.
func RevisionContentDigest(serial uint32, authority string, entries []domain.ZoneEntry) string {
	payload, _ := json.Marshal(struct {
		Serial    uint32             `json:"serial"`
		Authority string             `json:"authority"`
		Entries   []domain.ZoneEntry `json:"entries"`
	}{
		Serial:    serial,
		Authority: authority,
		Entries:   entries,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
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

func versionConflict(id string, expected uint64, actual uint64) *domain.DomainError {
	return domain.NewError(domain.CodeVersionConflict, "plan version does not match", map[string]any{
		"plan_id":  id,
		"expected": expected,
		"actual":   actual,
	})
}
