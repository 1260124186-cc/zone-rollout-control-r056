package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/plan"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
	"solo-0014-zone-rollout-control/internal/zonefile"
)

// maxZoneFileBytes bounds an offline import payload. It matches the JSON
// request limit so a malformed or huge file cannot exhaust the process.
const maxZoneFileBytes = 1 << 20

func runImportZone(args []string) error {
	fs := flag.NewFlagSet("import-zone", flag.ContinueOnError)
	var (
		snapshotPath string
		planID       string
		serialValue  uint
		authority    string
		origin       string
		confirm      bool
		showHelp     bool
	)
	fs.StringVar(&snapshotPath, "state", "data/zrc-state.json", "state snapshot path")
	fs.StringVar(&planID, "plan", "", "target draft plan id (required)")
	fs.UintVar(&serialValue, "serial", 0, "revision serial, unsigned 32-bit and strictly increasing (required)")
	fs.StringVar(&authority, "authority", "", "revision authority nameserver (required)")
	fs.StringVar(&origin, "origin", "", "default $ORIGIN for relative names; defaults to the plan zone")
	fs.BoolVar(&confirm, "yes", false, "confirm creation without an interactive prompt")
	fs.BoolVar(&showHelp, "help", false, "show import-zone usage")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s import-zone --plan <id> --serial <n> --authority <name> [flags] <zonefile>\n", os.Args[0])
		fmt.Fprintln(fs.Output(), "\nparses a traditional zone file and adds one zone revision to a draft plan.")
		fmt.Fprintln(fs.Output(), "supported syntax: owner/type/value lines, $ORIGIN/$TTL, IN class, leading")
		fmt.Fprintln(fs.Output(), "whitespace owner inheritance, '@' and ';' or '#' comments. types: A, AAAA, CNAME.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if showHelp {
		fs.Usage()
		return nil
	}
	positionals := fs.Args()
	if len(positionals) != 1 || planID == "" || serialValue == 0 || authority == "" {
		fs.Usage()
		return errors.New("import-zone requires <zonefile>, --plan, --serial and --authority")
	}
	if serialValue > uint(^uint32(0)) {
		return fmt.Errorf("serial %d does not fit in an unsigned 32-bit integer", serialValue)
	}
	filePath := positionals[0]

	// 1) Read the zone file BEFORE touching the snapshot. The bytes are kept
	// for the content fingerprint; parsing is re-run once the default origin
	// (the plan zone) is known. Any encoding or syntax failure aborts here, so
	// the existing state file is never opened for writing and the original
	// snapshot is preserved untouched.
	raw, err := readZoneFile(filePath)
	if err != nil {
		return err
	}

	// 2) Open the state snapshot under the same file lock the server uses.
	lock, err := platform.LockFile(snapshotPath + ".lock")
	if err != nil {
		if errors.Is(err, platform.ErrAlreadyLocked) {
			return fmt.Errorf("snapshot %s is locked by another process; retry after it exits", snapshotPath)
		}
		return fmt.Errorf("lock snapshot: %w", err)
	}
	defer lock.Unlock()

	stateStore, err := store.Open(snapshotPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	ctx := context.Background()
	planService := plan.New(stateStore, platform.SystemClock{}, platform.CryptoIDGenerator{})

	target, err := planService.Get(ctx, planID)
	if err != nil {
		return err
	}
	defaultOrigin := origin
	if defaultOrigin == "" {
		defaultOrigin = target.Zone
	}

	// 3) Re-parse with the resolved default origin, then normalize, validate,
	// deduplicate and capacity-check entirely in memory. Nothing is written.
	parsed, err := zonefile.Parse(raw, defaultOrigin)
	if err != nil {
		return describeParseError(err)
	}
	prepared, err := zonefile.Prepare(parsed, uint32(serialValue), authority, raw)
	if err != nil {
		return describeImportError(err)
	}

	// 4) Run the same plan-level checks the JSON revision endpoint enforces:
	// plan state, strictly increasing serial, and exact content identity.
	preview, err := planService.PreviewRevision(ctx, planID, prepared.Input)
	if err != nil {
		return err
	}
	if preview.DuplicateRevisionID != "" {
		printDuplicateSummary(os.Stdout, filePath, raw, prepared, preview)
		return nil
	}
	if preview.PlanState != domain.PlanDraft {
		return fmt.Errorf("plan %s is %s; revisions can be added only while it is draft (snapshot left unchanged)", planID, preview.PlanState)
	}
	printRevisionSummary(os.Stdout, filePath, raw, prepared, preview, target)

	// 5) Confirmation gate. Nothing above mutated state; only an explicit
	// confirmation reaches the formal model write.
	if !confirm {
		approved, err := askConfirmation(os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
		if !approved {
			fmt.Fprintln(os.Stdout, "import cancelled; snapshot left unchanged")
			return nil
		}
	}

	revision, err := planService.ImportRevision(ctx, planID, prepared.Input, plan.ImportSource{
		FileName:        filepath.Base(filePath),
		FileFingerprint: prepared.FileFingerprint,
		ContentDigest:   prepared.ContentDigest,
		ExpectedVersion: preview.PlanVersion,
	})
	if err != nil {
		var domainErr *domain.DomainError
		if errors.As(err, &domainErr) && domainErr.Code == domain.CodeVersionConflict {
			return fmt.Errorf("%s\n(target plan changed between preview and write; rerun the import to review the new state)", err.Error())
		}
		return err
	}
	fmt.Fprintf(os.Stdout, "\nrevision created: %s (plan %s, serial %d, %d entries); snapshot %s now at revision %d\n",
		revision.ID, revision.PlanID, revision.Serial, revision.EntryCount, snapshotPath, stateStore.Revision())
	return nil
}

func readZoneFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open zone file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxZoneFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read zone file: %w", err)
	}
	if len(raw) > maxZoneFileBytes {
		return nil, fmt.Errorf("zone file exceeds the %d byte import limit", maxZoneFileBytes)
	}
	return raw, nil
}

func describeParseError(err error) error {
	var parseErr *zonefile.ParseError
	if errors.As(err, &parseErr) {
		return fmt.Errorf("zone file syntax error: %w", err)
	}
	return err
}

func describeImportError(err error) error {
	var importErr *zonefile.ImportError
	if !errors.As(err, &importErr) {
		return err
	}
	var details strings.Builder
	for key, value := range importErr.Details {
		if details.Len() > 0 {
			details.WriteString(", ")
		}
		fmt.Fprintf(&details, "%s=%v", key, value)
	}
	if importErr.Line > 0 {
		if details.Len() > 0 {
			return fmt.Errorf("zone file error at %s: %s (%s)", importErrorLocation(importErr.Line), importErr.Message, details.String())
		}
		return fmt.Errorf("zone file error at %s: %s", importErrorLocation(importErr.Line), importErr.Message)
	}
	if details.Len() > 0 {
		return fmt.Errorf("%s (%s)", importErr.Message, details.String())
	}
	return errors.New(importErr.Message)
}

func importErrorLocation(line int) string {
	return "line " + strconv.Itoa(line)
}

func askConfirmation(stdin *os.File, stdout *os.File) (bool, error) {
	info, err := stdin.Stat()
	if err != nil || (info.Mode()&os.ModeCharDevice) == 0 {
		return false, errConfirmedRequired
	}
	fmt.Fprint(stdout, "type 'yes' to create this revision: ")
	reader := bufio.NewReader(stdin)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	return strings.TrimSpace(strings.ToLower(answer)) == "yes", nil
}

func printRevisionSummary(out io.Writer, filePath string, raw []byte, prepared *zonefile.PreparedImport, preview plan.RevisionPreview, target domain.RolloutPlan) {
	fmt.Fprintf(out, "zone file import preview\n")
	fmt.Fprintf(out, "  source file:        %s\n", filePath)
	fmt.Fprintf(out, "  file size:          %d bytes\n", len(raw))
	fmt.Fprintf(out, "  file fingerprint:   sha256:%s\n", prepared.FileFingerprint)
	fmt.Fprintf(out, "  content identity:   sha256:%s\n", prepared.ContentDigest)
	fmt.Fprintf(out, "  target plan:        %s (zone %s, state %s, version %d)\n", target.ID, target.Zone, preview.PlanState, preview.PlanVersion)
	fmt.Fprintf(out, "  revision serial:    %d", prepared.Input.Serial)
	if preview.LatestSerial > 0 {
		fmt.Fprintf(out, " (latest in plan: %d)", preview.LatestSerial)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  authority:          %s\n", prepared.Input.Authority)
	fmt.Fprintf(out, "  entries:            %d (comments %d, directives %d, blank %d)\n",
		len(prepared.Input.Entries), prepared.CommentLines, prepared.DirectiveLines, prepared.BlankLines)
	fmt.Fprintln(out, "  normalized entries:")
	for index, entry := range prepared.Input.Entries {
		fmt.Fprintf(out, "    %-3d line %-4d  %-30s  %-5s  %s\n",
			index+1, prepared.SourceLines[index], entry.Owner, entry.Type, entry.Value)
	}
}

func printDuplicateSummary(out io.Writer, filePath string, raw []byte, prepared *zonefile.PreparedImport, preview plan.RevisionPreview) {
	if preview.DuplicateSameSerial {
		fmt.Fprintln(out, "revision already exists with this serial and content")
	} else {
		fmt.Fprintln(out, "this file content was already imported under a different serial")
	}
	fmt.Fprintf(out, "  source file:        %s (%d bytes)\n", filePath, len(raw))
	fmt.Fprintf(out, "  file fingerprint:   sha256:%s\n", prepared.FileFingerprint)
	fmt.Fprintf(out, "  content identity:   sha256:%s\n", prepared.ContentDigest)
	fmt.Fprintf(out, "  existing revision:  %s (serial %d, %d entries)\n",
		preview.DuplicateRevisionID, preview.DuplicateSerial, preview.EntryCount)
	fmt.Fprintln(out, "no new revision was created; snapshot left unchanged")
}
