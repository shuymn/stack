// Command jev-calibrate records live Jev answers into fixtures/jev/<ext> and refits that
// extension's cutoffs (docs/design/phase4-jev.md §3.5); --ext picks semantic (the default) or
// classify. Run it from the repository
// root with the key (TYPESAFE_API_KEY or the key file); it is not a stage binary. Each planned
// body is sent three times.
package main

import (
	"context"
	"errors"
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

var errNoKey = errors.New("no key: set " + jev.KeyEnv + " or create " + jev.KeyDisplay)

func main() {
	home, _ := os.UserHomeDir() // "" when unknown; run reports it
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, home, os.Stdout, os.Stderr))
}

// run calibrates with the key getenv or the key file under home gives; home also holds the
// usage ledger.
func run(
	ctx context.Context, args []string, getenv func(string) string, home string,
	stdout, stderr io.Writer,
) int {
	fs := flag.NewFlagSet("jev-calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ext := fs.String("ext", "semantic", "the extension to calibrate: semantic or classify")
	fixtures := fs.String("fixtures", "", "the fixture directory (default fixtures/jev/<ext>)")
	table := fs.String(
		"table",
		"",
		"the cutoff table to write (default extensions/jev/<ext>/cutoffs.json)",
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *ext != "semantic" && *ext != "classify" {
		_, _ = fmt.Fprintf(stderr, "jev-calibrate: unknown extension %q\n", *ext)
		return exitUsage
	}
	if *fixtures == "" {
		*fixtures = "fixtures/jev/" + *ext
	}
	if *table == "" {
		*table = "extensions/jev/" + *ext + "/cutoffs.json"
	}
	if home == "" {
		_, _ = fmt.Fprintln(stderr, "jev-calibrate: home directory unknown")
		return exitUsage
	}
	key, err := jev.Key(getenv, home)
	if err == nil && key == "" {
		err = errNoKey
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jev-calibrate: %v\n", err)
		return exitUsage
	}
	live := jev.Live{Endpoint: jev.Endpoint(), Key: key, Ledger: &jev.Ledger{
		Path: jev.LedgerPath(home), Run: jev.RunID(), Extension: "jev-calibrate", Stderr: stderr,
	}}
	if *ext == "classify" {
		err = calibrate.RunClassify(ctx, calibrate.ClassifyOptions{
			Fixtures: *fixtures, Table: *table, Transport: live, Runner: proc.Exec{},
			Environ: os.Environ(), Stdout: stdout,
		})
	} else {
		err = calibrate.Run(ctx, calibrate.Options{
			Fixtures: *fixtures, Table: *table, Transport: live, Runner: proc.Exec{},
			Environ: os.Environ(), Stdout: stdout,
		})
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jev-calibrate: %v\n", err)
		return 1
	}
	return 0
}
