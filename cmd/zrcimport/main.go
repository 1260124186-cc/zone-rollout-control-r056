// Command zrcimport loads a traditional zone file into a draft rollout plan.
// The file is parsed and validated fully in memory, the revision that would
// be created is printed as a summary, and only an explicit confirmation (or
// --yes) writes it through the plan service. Syntax and encoding failures
// never touch the state snapshot.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/plan"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
	"solo-0014-zone-rollout-control/internal/zonefile"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		var domainErr *domain.DomainError
		if errors.As(err, &domainErr) && len(domainErr.Details) > 0 {
			fmt.Fprintf(os.Stderr, "details: %s\n", domain.Describe(domainErr))
		}
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("zrcimport", flag.ContinueOnError)
	var (
		statePath = flags.String("state", "data/zrc-state.json", "JSON state snapshot path")
		planID    = flags.String("plan", "", "target rollout plan identifier")
		filePath  = flags.String("file", "", "traditional zone file to import")
		serial    = flags.Uint64("serial", 0, "zone serial for the new revision")
		authority = flags.String("authority", "", "primary authority name for the new revision")
		origin    = flags.String("origin", "", "origin for relative names (defaults to the plan zone)")
		assumeYes = flags.Bool("yes", false, "write the revision without interactive confirmation")
	)
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *planID == "" || *filePath == "" || *authority == "" {
		flags.Usage()
		return errors.New("--plan, --file, and --authority are required")
	}
	if *serial == 0 || *serial > 1<<32-1 {
		return errors.New("--serial must be between 1 and 4294967295")
	}

	// Decode and parse before opening the snapshot: encoding and syntax
	// failures must leave the state file untouched.
	raw, err := os.ReadFile(*filePath)
	if err != nil {
		return fmt.Errorf("read zone file: %w", err)
	}
	text, err := zonefile.Decode(raw)
	if err != nil {
		return fmt.Errorf("zone file %s: %w", *filePath, err)
	}

	lock, err := platform.LockFile(*statePath + ".lock")
	if err != nil {
		if errors.Is(err, platform.ErrAlreadyLocked) {
			return fmt.Errorf("state snapshot %s is locked by another process", *statePath)
		}
		return fmt.Errorf("lock state snapshot: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	stateStore, err := store.Open(*statePath)
	if err != nil {
		return fmt.Errorf("open state snapshot: %w", err)
	}
	plans := plan.New(stateStore, platform.SystemClock{}, platform.CryptoIDGenerator{})
	ctx := context.Background()

	rollout, err := plans.Get(ctx, *planID)
	if err != nil {
		return err
	}
	if rollout.State != domain.PlanDraft {
		return fmt.Errorf("plan %s is %s: zone files can be imported only into a draft plan; snapshot left unchanged", rollout.ID, rollout.State)
	}

	defaultOrigin := *origin
	if defaultOrigin == "" {
		defaultOrigin = rollout.Zone
	}
	records, err := zonefile.Parse(text, defaultOrigin)
	if err != nil {
		return fmt.Errorf("zone file %s: %w", *filePath, err)
	}
	entries, err := zonefile.Compile(records)
	if err != nil {
		return fmt.Errorf("zone file %s: %w", *filePath, err)
	}

	// Run the same normalization and validation as the JSON revision
	// endpoint so the import can never create content the API would reject.
	normalized, err := domain.NormalizeRevisionInput(domain.RevisionCreateInput{
		Serial:    uint32(*serial),
		Authority: *authority,
		Entries:   entries,
	})
	if err != nil {
		return err
	}
	if err := domain.ValidateRevisionInput(normalized); err != nil {
		return err
	}

	fingerprint := plan.RevisionContentDigest(normalized.Serial, normalized.Authority, normalized.Entries)
	existing, exists, err := plans.FindRevision(ctx, rollout.ID, normalized.Serial, normalized.Authority, normalized.Entries)
	if err != nil {
		return err
	}
	if exists {
		fmt.Printf("plan %s already contains this exact revision as %s (fingerprint %s); nothing to import\n",
			rollout.ID, existing.ID, fingerprint)
		return nil
	}

	printSummary(*statePath, *filePath, rollout, normalized, len(records), fingerprint)

	if !*assumeYes {
		fmt.Printf("type 'yes' to write this revision to plan %s: ", rollout.ID)
		answer, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read confirmation: %w", readErr)
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "yes" && answer != "y" {
			return errors.New("import aborted; state snapshot left unchanged")
		}
	}

	created, err := plans.ImportRevision(ctx, rollout.ID, rollout.Version, normalized)
	if err != nil {
		return fmt.Errorf("import failed; state snapshot left unchanged: %w", err)
	}
	fmt.Printf("imported revision %s into plan %s (serial %d, %d entries)\n",
		created.ID, created.PlanID, created.Serial, created.EntryCount)
	return nil
}

func printSummary(statePath string, filePath string, rollout domain.RolloutPlan, input domain.RevisionCreateInput, recordCount int, fingerprint string) {
	fmt.Println("zone file import preview")
	fmt.Printf("  state file:  %s\n", statePath)
	fmt.Printf("  plan:        %s (zone %s, state %s, version %d)\n", rollout.ID, rollout.Zone, rollout.State, rollout.Version)
	fmt.Printf("  zone file:   %s (%d records)\n", filePath, recordCount)
	fmt.Printf("  serial:      %d\n", input.Serial)
	fmt.Printf("  authority:   %s\n", input.Authority)
	fmt.Printf("  entries:     %d (%s)\n", len(input.Entries), typeSummary(input.Entries))
	fmt.Printf("  fingerprint: %s\n", fingerprint)
	const shown = 10
	for index, entry := range input.Entries {
		if index == shown {
			fmt.Printf("    ... and %d more\n", len(input.Entries)-shown)
			break
		}
		fmt.Printf("    %-36s %-5s %s\n", entry.Owner, entry.Type, entry.Value)
	}
}

func typeSummary(entries []domain.ZoneEntry) string {
	counts := make(map[domain.EntryType]int, 3)
	for _, entry := range entries {
		counts[entry.Type]++
	}
	parts := make([]string, 0, len(counts))
	for _, entryType := range []domain.EntryType{domain.EntryA, domain.EntryAAAA, domain.EntryCNAME} {
		if counts[entryType] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", entryType, counts[entryType]))
		}
	}
	return strings.Join(parts, ", ")
}
