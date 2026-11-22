// Command jev-calibrate records live Jev answers into fixtures/jev/semantic and refits
// jev-semantic's cutoffs (docs/design/phase4-jev.md §3.5). Run it from the repository
// root with TYPESAFE_API_KEY set; it is not a stage binary. Each planned body is sent three times.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/calibrate"
)

// exitUsage is the exit code of a usage error or a missing key.
const exitUsage = 2

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(
	ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer,
) int {
	fs := flag.NewFlagSet("jev-calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fixtures := fs.String("fixtures", "fixtures/jev/semantic", "the semantic fixture directory")
	table := fs.String("table", "extensions/jev/semantic/cutoffs.json", "the cutoff table to write")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	key, err := calibrate.Key(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jev-calibrate: %v\n", err)
		return exitUsage
	}
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jev-calibrate: %v\n", err)
		return exitUsage
	}
	live := jev.Live{Endpoint: jev.Endpoint(), Key: key, Ledger: &jev.Ledger{
		Path: jev.LedgerPath(home), Run: jev.RunID(), Extension: "jev-calibrate", Stderr: stderr,
	}}
	err = calibrate.Run(ctx, calibrate.Options{
		Fixtures: *fixtures, Table: *table, Transport: live, Runner: proc.Exec{},
		Environ: os.Environ(), Stdout: stdout,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jev-calibrate: %v\n", err)
		return 1
	}
	return 0
}
