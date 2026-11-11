// Command finstore is a self-hosted datastore for one person's bank data.
package main

import (
	"fmt"
	"io"
	"os"
)

// Set at build time with -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		_, _ = fmt.Fprintf(stdout, "finstore %s (commit %s, built %s)\n", version, commit, buildDate)
		return 0
	default:
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: finstore <command>")
	_, _ = fmt.Fprintln(w, "commands: version")
}
