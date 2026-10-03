package zonefile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"solo-0014-zone-rollout-control/internal/domain"
)

// PreparedImport is the fully validated, in-memory result of converting a zone
// file into a revision. Nothing has been persisted at this point: callers show
// a summary, ask for confirmation, and only then hand Input to the same
// revision service that backs the JSON API.
type PreparedImport struct {
	Input domain.RevisionCreateInput
	// SourceLines maps each entry of Input.Entries (sorted) back to the
	// source line it originated from, for diagnostics.
	SourceLines     []int
	ContentDigest   string
	FileFingerprint string
	CommentLines    int
	DirectiveLines  int
	BlankLines      int
}

// ImportError is a preparation failure whose Line points at the zone-file line
// responsible. Line is 0 for errors that belong to the request itself rather
// than to a file line (for example a bad --authority flag).
type ImportError struct {
	Line    int
	Message string
	Details map[string]any
	err     *domain.DomainError
}

func (e *ImportError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

// Unwrap lets callers recover the underlying domain error code so imports and
// JSON requests share the exact same error vocabulary.
func (e *ImportError) Unwrap() error {
	return e.err
}

// DomainError returns the underlying stable domain error.
func (e *ImportError) DomainError() *domain.DomainError {
	return e.err
}

func importError(line int, domainErr *domain.DomainError) *ImportError {
	return &ImportError{
		Line:    line,
		Message: domainErr.Message,
		Details: domainErr.Details,
		err:     domainErr,
	}
}

// indexedEntry keeps a normalized entry beside its source line while sorting.
type indexedEntry struct {
	entry domain.ZoneEntry
	line  int
}

// Prepare converts a parsed file into a normalized revision input entirely in
// memory. It performs the same semantic checks the JSON revision endpoint does
// (serial/authority validation, per-entry canonicalization, capacity, stable
// sorting and duplicate detection) plus in-file duplicate detection reported
// at the offending source line. It never touches storage.
func Prepare(file *File, serial uint32, authority string, raw []byte) (*PreparedImport, error) {
	if file == nil {
		return nil, &ImportError{Message: "no parsed zone file was provided"}
	}
	if len(file.Records) == 0 {
		return nil, importError(0, domain.Validation("revision input is invalid", map[string]any{
			"entries": "must contain between 1 and 500 items",
		}))
	}
	if len(file.Records) > domain.MaxEntries {
		return nil, importError(0, domain.Validation("revision input is invalid", map[string]any{
			"entries": fmt.Sprintf("zone file contains %d records, which exceeds the limit of %d", len(file.Records), domain.MaxEntries),
		}))
	}

	// Validate "$ORIGIN" names with the shared canonical-name rules while
	// keeping the directive's source line.
	for _, directive := range file.Origins {
		if err := domain.ValidateName(domain.CanonicalName(directive.Name)); err != nil {
			var domainErr *domain.DomainError
			errors.As(err, &domainErr)
			domainErr.Message = "$ORIGIN name is invalid"
			return nil, importError(directive.Line, domainErr)
		}
	}

	normalizedAuthority := domain.CanonicalName(authority)
	if err := domain.ValidateName(normalizedAuthority); err != nil {
		var nameErr *domain.DomainError
		if errors.As(err, &nameErr) {
			return nil, importError(0, domain.Validation("revision input is invalid", map[string]any{
				"authority": nameErr.Message,
			}))
		}
		return nil, importError(0, domain.Validation("revision input is invalid", map[string]any{
			"authority": err.Error(),
		}))
	}
	if serial == 0 {
		return nil, importError(0, domain.Validation("revision input is invalid", map[string]any{
			"serial": "must be greater than zero",
		}))
	}

	indexed := make([]indexedEntry, 0, len(file.Records))
	// Track normalized entries already seen in this file, keyed by canonical
	// form, so duplicates are reported at the second occurrence.
	seen := make(map[string]int, len(file.Records))
	for _, record := range file.Records {
		normalized, err := domain.NormalizeZoneEntry(record.Entry)
		if err != nil {
			var domainErr *domain.DomainError
			errors.As(err, &domainErr)
			return nil, importError(record.Line, domainErr)
		}
		key := entryKey(normalized)
		if firstLine, ok := seen[key]; ok {
			return nil, importError(record.Line, domain.NewError(
				domain.CodeValidationFailed,
				"zone entry is duplicated",
				map[string]any{
					"owner":      normalized.Owner,
					"type":       normalized.Type,
					"value":      normalized.Value,
					"first_seen": firstLine,
				},
			))
		}
		seen[key] = record.Line
		indexed = append(indexed, indexedEntry{entry: normalized, line: record.Line})
	}

	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].entry.Owner != indexed[j].entry.Owner {
			return indexed[i].entry.Owner < indexed[j].entry.Owner
		}
		if indexed[i].entry.Type != indexed[j].entry.Type {
			return indexed[i].entry.Type < indexed[j].entry.Type
		}
		return indexed[i].entry.Value < indexed[j].entry.Value
	})

	entries := make([]domain.ZoneEntry, len(indexed))
	sourceLines := make([]int, len(indexed))
	for i, item := range indexed {
		entries[i] = item.entry
		sourceLines[i] = item.line
	}

	input := domain.RevisionCreateInput{
		Serial:    serial,
		Authority: normalizedAuthority,
		Entries:   entries,
	}
	// Final validation through the exported domain entry point guarantees
	// parity with the JSON revision endpoint.
	if err := domain.ValidateRevisionInput(input); err != nil {
		var domainErr *domain.DomainError
		errors.As(err, &domainErr)
		return nil, importError(0, domainErr)
	}

	sum := sha256.Sum256(raw)
	return &PreparedImport{
		Input:           input,
		SourceLines:     sourceLines,
		ContentDigest:   RevisionContentDigest(input.Authority, input.Entries),
		FileFingerprint: hex.EncodeToString(sum[:]),
		CommentLines:    file.CommentLines,
		DirectiveLines:  file.DirectiveLines,
		BlankLines:      file.BlankLines,
	}, nil
}

// RevisionContentDigest is the content identity of a revision candidate. It
// deliberately excludes the serial, which is metadata supplied separately:
// importing the same normalized file content again (even under a new serial)
// produces the same digest, regardless of comments, whitespace or record
// order in the source file.
func RevisionContentDigest(authority string, entries []domain.ZoneEntry) string {
	payload, _ := json.Marshal(struct {
		Authority string
		Entries   []domain.ZoneEntry
	}{
		Authority: authority,
		Entries:   entries,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func entryKey(entry domain.ZoneEntry) string {
	return entry.Owner + "|" + string(entry.Type) + "|" + entry.Value
}
