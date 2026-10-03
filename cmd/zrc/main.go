package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// dispatch keeps the legacy flag-based server invocation working while adding
// offline subcommands. A first positional argument that names a known
// subcommand enters that tool; everything else starts the HTTP server exactly
// as before.
func dispatch(args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "import-zone":
			return runImportZone(args[1:])
		case "-h", "--help", "help":
			printRootUsage(os.Stdout)
			return nil
		}
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		printRootUsage(os.Stdout)
		return nil
	}
	return runServer(args)
}

func printRootUsage(file *os.File) {
	fmt.Fprintf(file, "usage:\n")
	fmt.Fprintf(file, "  %s [server flags]                 start the HTTP control plane\n", os.Args[0])
	fmt.Fprintf(file, "  %s import-zone [flags] <zonefile> offline import of a zone file\n", os.Args[0])
	fmt.Fprintf(file, "\nrun '%[1]s import-zone --help' for import options.\n", os.Args[0])
}

var errConfirmedRequired = errors.New("import requires confirmation; pass --yes to confirm non-interactively")
