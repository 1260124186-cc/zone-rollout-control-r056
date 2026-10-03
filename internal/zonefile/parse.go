// Package zonefile parses traditional offline zone files into zone entries.
//
// The parser intentionally accepts only the common subset used by operations
// teams: owner/type/value records, RFC1035 owner inheritance (a line starting
// with whitespace reuses the previous owner), "$ORIGIN"/"$TTL" directives,
// optional TTL and the IN class, and semicolon or hash comments. Only the
// record types the control plane supports (A, AAAA and CNAME) are accepted.
//
// Parsing is purely syntactic: relative names are expanded against $ORIGIN but
// values are never semantically validated here. Canonicalization, IP
// validation, sorting and dedup happen through the domain package, the same
// code path the JSON API uses.
package zonefile

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"solo-0014-zone-rollout-control/internal/domain"
)

// ParseError describes a malformed zone file. Line is always the 1-based
// source line so operators can fix the file without guessing.
type ParseError struct {
	Line    int
	Message string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

// Record is one parsed owner/type/value line before semantic validation.
type Record struct {
	Line  int
	Entry domain.ZoneEntry
}

// OriginDirective marks one "$ORIGIN" directive by source line. The name is
// already origin-expanded; semantic validation happens during preparation.
type OriginDirective struct {
	Line int
	Name string
}

// File is the parsed view of a zone file.
type File struct {
	// InitialOrigin holds the "$ORIGIN" in effect before the first record, or
	// the supplied default origin when none was set.
	InitialOrigin  string
	Origins        []OriginDirective
	Records        []Record
	CommentLines   int
	DirectiveLines int
	BlankLines     int
}

var supportedTypes = map[domain.EntryType]struct{}{
	domain.EntryA:     {},
	domain.EntryAAAA:  {},
	domain.EntryCNAME: {},
}

// unsupportedTypes names the common DNS types this control plane deliberately
// does not accept. Recognizing them yields a precise, line-located error
// instead of a generic "expected a record type" message.
var unsupportedTypes = map[string]struct{}{
	"NS": {}, "SOA": {}, "MX": {}, "TXT": {}, "PTR": {}, "SRV": {},
	"CAA": {}, "DNSKEY": {}, "DS": {}, "NAPTR": {}, "SPF": {}, "HINFO": {},
}

// Parse turns raw zone-file bytes into records. defaultOrigin qualifies
// relative names when the file does not declare "$ORIGIN" (normally the target
// plan's zone). It verifies the encoding (UTF-8, no NUL bytes, optional BOM)
// before looking at any record so a truncated or mis-encoded file can never be
// partially imported.
func Parse(raw []byte, defaultOrigin string) (*File, error) {
	payload, err := decodeBytes(raw)
	if err != nil {
		return nil, err
	}

	file := &File{}
	origin := defaultOrigin
	file.InitialOrigin = defaultOrigin
	lastOwnerToken := ""
	lines := strings.Split(payload, "\n")
	for index, line := range lines {
		lineNumber := index + 1
		// Normalize CRLF: strings.Split leaves a trailing '\r' on Windows files.
		line = strings.TrimRight(line, "\r")
		code, hadComment := stripComment(line)
		trimmed := strings.TrimSpace(code)
		if trimmed == "" {
			if hadComment {
				file.CommentLines++
			} else if index < len(lines)-1 || line != "" {
				file.BlankLines++
			}
			continue
		}
		if trimmed[0] == '$' {
			file.DirectiveLines++
			newOrigin, err := parseDirective(lineNumber, trimmed, origin)
			if err != nil {
				return nil, err
			}
			if newOrigin != "" {
				file.Origins = append(file.Origins, OriginDirective{Line: lineNumber, Name: newOrigin})
				origin = newOrigin
			}
			continue
		}
		if hadComment {
			file.CommentLines++
		}

		record, ownerToken, err := parseRecordLine(lineNumber, code, trimmed, origin, lastOwnerToken)
		if err != nil {
			return nil, err
		}
		lastOwnerToken = ownerToken
		file.Records = append(file.Records, record)
	}
	return file, nil
}

// decodeBytes rejects encodings that would silently corrupt DNS names. A
// leading UTF-8 BOM is tolerated and stripped; NUL bytes and invalid UTF-8
// sequences are hard errors that must abort before anything is persisted.
func decodeBytes(raw []byte) (string, error) {
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		raw = raw[3:]
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", &ParseError{Message: "file contains a NUL byte; expected UTF-8 text zone data"}
	}
	if !utf8.Valid(raw) {
		return "", &ParseError{Message: "file is not valid UTF-8; convert the zone file to UTF-8 and retry"}
	}
	return string(raw), nil
}

// stripComment removes a trailing comment. Both ';' (the RFC1035 zone-file
// marker) and '#' (common operational convention) start comments. Neither
// character is valid in the accepted names or IP literals, so there is no
// quoting ambiguity for the supported record types.
func stripComment(line string) (string, bool) {
	semicolon := strings.IndexByte(line, ';')
	hash := strings.IndexByte(line, '#')
	position := -1
	switch {
	case semicolon >= 0 && hash >= 0:
		if semicolon < hash {
			position = semicolon
		} else {
			position = hash
		}
	case semicolon >= 0:
		position = semicolon
	case hash >= 0:
		position = hash
	}
	if position < 0 {
		return line, false
	}
	return line[:position], true
}

func parseDirective(line int, text string, currentOrigin string) (string, error) {
	fields := strings.Fields(text)
	switch strings.ToUpper(fields[0]) {
	case "$ORIGIN":
		if len(fields) != 2 {
			return "", &ParseError{Line: line, Message: "$ORIGIN requires exactly one name"}
		}
		if !strings.Contains(fields[1], ".") {
			// "$ORIGIN ." is the root; a bare single label almost always
			// means a mis-converted file and is rejected outright.
			return "", &ParseError{Line: line, Message: "$ORIGIN must be a dotted domain name"}
		}
		return expandName(fields[1], currentOrigin), nil
	case "$TTL":
		if len(fields) != 2 {
			return "", &ParseError{Line: line, Message: "$TTL requires exactly one value"}
		}
		if !isTTL(fields[1]) {
			return "", &ParseError{Line: line, Message: "$TTL must be a non-negative integer number of seconds"}
		}
		return "", nil
	case "$INCLUDE":
		return "", &ParseError{Line: line, Message: "$INCLUDE is not supported for offline import; inline the records"}
	default:
		return "", &ParseError{Line: line, Message: fmt.Sprintf("unsupported directive %q", fields[0])}
	}
}

func parseRecordLine(line int, code string, text string, origin string, lastOwnerToken string) (Record, string, error) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return Record{}, "", &ParseError{Line: line, Message: "record must contain at least a type and a value"}
	}

	// RFC1035: a line beginning with whitespace has no owner field and inherits
	// the previous owner. A line beginning with a non-space token always starts
	// with that owner (or "@"), even when it looks like a TTL or class.
	startsWithOwner := len(code) > 0 && code[0] != ' ' && code[0] != '\t'

	ownerToken := lastOwnerToken
	cursor := 0
	sawTTL := false
	sawClass := false
	if startsWithOwner {
		ownerToken = fields[0]
		cursor = 1
	}

	classify := func(field string) string {
		switch {
		case isTTL(field):
			return "ttl"
		case strings.EqualFold(field, "IN"):
			return "class-in"
		case strings.EqualFold(field, "CH"), strings.EqualFold(field, "HS"):
			return "class-bad"
		default:
			upper := strings.ToUpper(field)
			if _, ok := supportedTypes[domain.EntryType(upper)]; ok {
				return "type"
			}
			if _, ok := unsupportedTypes[upper]; ok {
				return "type-unsupported"
			}
			return "other"
		}
	}

	for cursor < len(fields) {
		kind := classify(fields[cursor])
		switch kind {
		case "ttl":
			if sawTTL {
				return Record{}, "", &ParseError{Line: line, Message: "record contains more than one TTL"}
			}
			sawTTL = true
			cursor++
		case "class-in":
			if sawClass {
				return Record{}, "", &ParseError{Line: line, Message: "record contains more than one class"}
			}
			sawClass = true
			cursor++
		case "class-bad":
			return Record{}, "", &ParseError{Line: line, Message: fmt.Sprintf("record class %q is not supported; only IN is accepted", fields[cursor])}
		case "type-unsupported":
			return Record{}, "", &ParseError{
				Line:    line,
				Message: fmt.Sprintf("record type %s is not supported for import; only A, AAAA and CNAME are accepted", strings.ToUpper(fields[cursor])),
			}
		case "type":
			goto haveType
		case "other":
			return Record{}, "", &ParseError{Line: line, Message: fmt.Sprintf("unexpected field %q before record type", fields[cursor])}
		}
	}
	return Record{}, "", &ParseError{Line: line, Message: "record is missing its type and value"}

haveType:
	typeToken := domain.EntryType(strings.ToUpper(fields[cursor]))
	rdataFields := fields[cursor+1:]
	if len(rdataFields) != 1 {
		return Record{}, "", &ParseError{
			Line:    line,
			Message: fmt.Sprintf("%s record must have exactly one value, found %d", typeToken, len(rdataFields)),
		}
	}
	if ownerToken == "" {
		// First record with no explicit owner and no previous owner targets
		// the current origin.
		ownerToken = "@"
	}
	expandedOwner := expandName(ownerToken, origin)
	value := rdataFields[0]
	if typeToken == domain.EntryCNAME {
		value = expandName(rdataFields[0], origin)
	}
	return Record{
		Line: line,
		Entry: domain.ZoneEntry{
			Owner: expandedOwner,
			Type:  typeToken,
			Value: value,
		},
	}, ownerToken, nil
}

func isTTL(field string) bool {
	if value, err := strconv.Atoi(field); err == nil {
		return value >= 0
	}
	if len(field) > 1 {
		scale := field[len(field)-1]
		switch scale {
		case 's', 'm', 'h', 'd', 'w':
			value, err := strconv.Atoi(field[:len(field)-1])
			return err == nil && value >= 0
		}
	}
	return false
}

// expandName applies RFC1035 $ORIGIN semantics: "@" becomes the origin, a
// trailing dot marks an absolute name, and everything else is qualified with
// the origin. The result is still raw; domain canonicalization happens later.
func expandName(name string, origin string) string {
	if name == "@" {
		return origin
	}
	if strings.HasSuffix(name, ".") {
		return name
	}
	if origin == "" {
		return name
	}
	if origin == "." {
		return name + "."
	}
	return name + "." + origin
}
