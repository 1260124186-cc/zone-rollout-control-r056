// Package zonefile parses traditional master-format zone files entirely in
// memory so they can be imported as zone revisions. All normalization and
// validation delegates to the domain package, keeping the rules identical to
// the JSON revision endpoint.
package zonefile

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"solo-0014-zone-rollout-control/internal/domain"
)

// maxFileBytes bounds the zone files accepted for import. A revision holds at
// most 500 entries, so anything larger is almost certainly the wrong file.
const maxFileBytes = 4 << 20

// SyntaxError locates a zone file problem. Line is 1-based; a zero line marks
// a file-level problem such as an unsupported encoding.
type SyntaxError struct {
	Line    int
	Message string
}

func (e *SyntaxError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

// Record is one parsed master file record before domain normalization. Owner
// and CNAME values are already resolved against the origin.
type Record struct {
	Owner string
	Type  string
	Value string
	Line  int
}

// Decode validates the raw file bytes and returns the text content. The file
// must be UTF-8; an optional UTF-8 byte order mark is stripped and UTF-16
// content is rejected explicitly.
func Decode(data []byte) (string, error) {
	if len(data) > maxFileBytes {
		return "", &SyntaxError{Message: fmt.Sprintf("file exceeds the %d byte limit", maxFileBytes)}
	}
	if len(data) >= 2 && (data[0] == 0xFF && data[1] == 0xFE || data[0] == 0xFE && data[1] == 0xFF) {
		return "", &SyntaxError{Message: "file must be UTF-8 encoded; UTF-16 content is not supported"}
	}
	text := strings.TrimPrefix(string(data), "\uFEFF")
	if !utf8.ValidString(text) {
		return "", &SyntaxError{Message: "file is not valid UTF-8 text"}
	}
	return text, nil
}

// Parse turns zone file text into records. defaultOrigin supplies the initial
// origin (usually the plan zone); a $ORIGIN directive overrides it. Comments
// start with ';', parentheses group a record across lines, '@' denotes the
// origin, relative names are resolved against the origin, and a record
// indented with whitespace inherits the previous record's owner.
func Parse(text string, defaultOrigin string) ([]Record, error) {
	lines, err := splitLines(text)
	if err != nil {
		return nil, err
	}
	origin := ""
	if defaultOrigin != "" {
		canonical, err := domain.CanonicalName(defaultOrigin)
		if err != nil {
			return nil, &SyntaxError{Message: fmt.Sprintf("default origin is invalid: %s", err)}
		}
		origin = canonical
	}
	records := make([]Record, 0, len(lines))
	lastOwner := ""
	for _, line := range lines {
		if strings.HasPrefix(line.tokens[0].text, "$") {
			if err := applyDirective(line, &origin); err != nil {
				return nil, err
			}
			continue
		}
		record, err := parseRecord(line, origin, lastOwner)
		if err != nil {
			return nil, err
		}
		lastOwner = record.Owner
		records = append(records, record)
	}
	return records, nil
}

// Compile normalizes records with the same domain rules as the JSON revision
// endpoint, rejects entries that are duplicated after normalization, and
// enforces the revision entry capacity. Every failure carries the source line.
func Compile(records []Record) ([]domain.ZoneEntry, error) {
	entries := make([]domain.ZoneEntry, 0, len(records))
	seen := make(map[domain.ZoneEntry]int, len(records))
	for _, record := range records {
		entry, err := domain.NormalizeEntry(record.Owner, record.Type, record.Value)
		if err != nil {
			return nil, &SyntaxError{Line: record.Line, Message: domain.Describe(err)}
		}
		if firstLine, exists := seen[entry]; exists {
			return nil, &SyntaxError{
				Line:    record.Line,
				Message: fmt.Sprintf("entry %s %s %s duplicates the entry from line %d", entry.Owner, entry.Type, entry.Value, firstLine),
			}
		}
		seen[entry] = record.Line
		entries = append(entries, entry)
		if len(entries) > domain.MaxEntries {
			return nil, &SyntaxError{
				Line:    record.Line,
				Message: fmt.Sprintf("entry capacity exceeded: a revision holds at most %d entries", domain.MaxEntries),
			}
		}
	}
	return entries, nil
}

type token struct {
	text string
	line int
}

type logicalLine struct {
	tokens   []token
	indented bool
}

// splitLines tokenizes the text into logical lines, joining parenthesized
// groups and stripping comments. The indented flag records whether the line
// began with whitespace, which signals owner inheritance in master files.
func splitLines(text string) ([]logicalLine, error) {
	var lines []logicalLine
	var current logicalLine
	depth := 0
	line := 1
	index := 0
	for index < len(text) {
		char := text[index]
		switch char {
		case '\r':
			index++
		case '\n':
			line++
			index++
			if depth == 0 {
				if len(current.tokens) > 0 {
					lines = append(lines, current)
				}
				current = logicalLine{}
			}
		case ' ', '\t':
			if len(current.tokens) == 0 && depth == 0 {
				current.indented = true
			}
			index++
		case ';':
			for index < len(text) && text[index] != '\n' {
				index++
			}
		case '(':
			if depth > 0 {
				return nil, &SyntaxError{Line: line, Message: "nested parentheses are not supported"}
			}
			depth++
			index++
		case ')':
			if depth == 0 {
				return nil, &SyntaxError{Line: line, Message: "closing parenthesis without an opening one"}
			}
			depth--
			index++
		default:
			start := index
			for index < len(text) && !isDelimiter(text[index]) {
				index++
			}
			current.tokens = append(current.tokens, token{text: text[start:index], line: line})
		}
	}
	if depth > 0 {
		return nil, &SyntaxError{Line: line, Message: "opening parenthesis is never closed"}
	}
	if len(current.tokens) > 0 {
		lines = append(lines, current)
	}
	return lines, nil
}

func isDelimiter(char byte) bool {
	switch char {
	case ' ', '\t', '\r', '\n', ';', '(', ')':
		return true
	}
	return false
}

func applyDirective(line logicalLine, origin *string) error {
	head := line.tokens[0]
	switch strings.ToUpper(head.text) {
	case "$ORIGIN":
		if len(line.tokens) != 2 {
			return &SyntaxError{Line: head.line, Message: "$ORIGIN expects exactly one name"}
		}
		name, err := resolveName(line.tokens[1].text, *origin)
		if err != nil {
			return &SyntaxError{Line: line.tokens[1].line, Message: err.Error()}
		}
		canonical, err := domain.CanonicalName(name)
		if err != nil {
			return &SyntaxError{Line: line.tokens[1].line, Message: fmt.Sprintf("$ORIGIN name is invalid: %s", err)}
		}
		*origin = canonical
		return nil
	case "$TTL":
		if len(line.tokens) != 2 {
			return &SyntaxError{Line: head.line, Message: "$TTL expects exactly one value"}
		}
		if _, err := parseTTL(line.tokens[1].text); err != nil {
			return &SyntaxError{Line: line.tokens[1].line, Message: err.Error()}
		}
		return nil
	default:
		return &SyntaxError{Line: head.line, Message: fmt.Sprintf("unsupported directive %q", head.text)}
	}
}

func parseRecord(line logicalLine, origin string, inherited string) (Record, error) {
	tokens := line.tokens
	index := 0
	owner := ""
	if line.indented {
		if inherited == "" {
			return Record{}, &SyntaxError{Line: tokens[0].line, Message: "record omits the owner but no previous owner exists"}
		}
		owner = inherited
	} else {
		resolved, err := resolveName(tokens[0].text, origin)
		if err != nil {
			return Record{}, &SyntaxError{Line: tokens[0].line, Message: err.Error()}
		}
		owner = resolved
		index = 1
	}
	recordType := ""
	typeLine := tokens[0].line
	for ; index < len(tokens); index++ {
		text := tokens[index].text
		if isDigits(text) {
			if _, err := parseTTL(text); err != nil {
				return Record{}, &SyntaxError{Line: tokens[index].line, Message: err.Error()}
			}
			continue
		}
		if strings.EqualFold(text, "IN") {
			continue
		}
		recordType = text
		typeLine = tokens[index].line
		index++
		break
	}
	if recordType == "" {
		return Record{}, &SyntaxError{Line: tokens[0].line, Message: "record is missing a type (expected A, AAAA, or CNAME)"}
	}
	switch strings.ToUpper(recordType) {
	case "A", "AAAA", "CNAME":
	default:
		return Record{}, &SyntaxError{Line: typeLine, Message: fmt.Sprintf("unsupported record type %q (expected A, AAAA, or CNAME)", recordType)}
	}
	if index >= len(tokens) {
		return Record{}, &SyntaxError{Line: typeLine, Message: fmt.Sprintf("%s record is missing a value", strings.ToUpper(recordType))}
	}
	valueToken := tokens[index]
	index++
	if index < len(tokens) {
		return Record{}, &SyntaxError{Line: tokens[index].line, Message: fmt.Sprintf("unexpected token %q after the record value", tokens[index].text)}
	}
	value := valueToken.text
	if strings.EqualFold(recordType, "CNAME") {
		resolved, err := resolveName(value, origin)
		if err != nil {
			return Record{}, &SyntaxError{Line: valueToken.line, Message: err.Error()}
		}
		value = resolved
	}
	return Record{Owner: owner, Type: recordType, Value: value, Line: tokens[0].line}, nil
}

// resolveName applies master file name rules: '@' is the origin, a trailing
// dot marks an absolute name, and anything else is relative to the origin.
func resolveName(name string, origin string) (string, error) {
	if name == "@" {
		if origin == "" {
			return "", fmt.Errorf("'@' requires an origin (set $ORIGIN or pass --origin)")
		}
		return origin, nil
	}
	if strings.HasSuffix(name, ".") {
		return name, nil
	}
	if origin == "" {
		return "", fmt.Errorf("relative name %q requires an origin (set $ORIGIN or pass --origin)", name)
	}
	return name + "." + origin, nil
}

func parseTTL(text string) (uint32, error) {
	value, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("TTL %q is not a valid non-negative integer", text)
	}
	return uint32(value), nil
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for index := 0; index < len(text); index++ {
		if text[index] < '0' || text[index] > '9' {
			return false
		}
	}
	return true
}
