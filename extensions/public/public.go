// Package public is the stack-public extension (docs/design/core-stages.md §4.4): the check for
// local paths and private terms in what a push or a PR would publish. Without arguments it answers
// one outbound request of the extension protocol; `stack-public check` is the CLI mode for direct
// callers, with the flags, output and exit codes of the v10 core's `stack check public`.
package public

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/shuymn/stack/extensions/internal/checkpub"
	"github.com/shuymn/stack/extensions/internal/paths"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/internal/safety"
)

// Exit codes of the CLI mode.
const (
	exitOK      = 0
	exitRefused = 1
	exitUsage   = 2
)

const (
	// diag prefixes the CLI mode's diagnostics and checkSynopsis is its usage line: both are the
	// v10 core command's, so a caller that switches from `stack check public` reads the same output
	// (P9).
	diag          = "stack check public"
	checkSynopsis = diag + " <base>..<head> [--title-file F] [--body-file F] [--branch B]"
	// usage is the top-level usage, which names the stack-public invocations.
	usage = "usage: stack-public check <base>..<head> [--title-file F] [--body-file F] [--branch B]\n" +
		"       stack-public   (no arguments: one outbound request on stdin)\n"
)

var (
	errNoHome = errors.New("home directory unknown")
	errArgs   = errors.New("wrong arguments")
)

// Env is everything stack-public takes from the process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Home is the home directory; "" when unknown.
	Home string
}

// Main runs stack-public with the command-line arguments args (without the program name) and
// returns the exit code.
func Main(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		return stage(ctx, env)
	}
	switch args[0] {
	case "check":
		return runCheck(ctx, args[1:], env)
	case "help", "-h", "--help":
		_, _ = io.WriteString(env.Stdout, usage)
		return exitOK
	default:
		_, _ = fmt.Fprintf(env.Stderr, "stack-public: unknown command %q\n%s", args[0], usage)
		return exitUsage
	}
}

// runCheck is the CLI mode. Every input is validated before git runs; hits go to stderr and exit 1,
// anything that cannot be verified exits 2. Every hit is printed.
func runCheck(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	for _, name := range []string{"title-file", "body-file", "branch"} {
		fs.String(name, "", "")
	}
	positional, err := parseInterspersed(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprintf(env.Stdout, "usage: %s\n", checkSynopsis)
		return exitOK
	}
	if err == nil && len(positional) != 1 {
		err = fmt.Errorf("%w: want 1 positional, got %d", errArgs, len(positional))
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %v\nusage: %s\n", diag, err, checkSynopsis)
		return exitUsage
	}
	err = check(ctx, env, positional[0], fs)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, checkpub.ErrFound):
		_, _ = fmt.Fprintf(env.Stderr, "%s: refused: %v\n", diag, err)
		return exitRefused
	default:
		_, _ = fmt.Fprintf(env.Stderr, "%s: %v\n", diag, err)
		return exitUsage
	}
}

func check(ctx context.Context, env Env, rng string, fs *flag.FlagSet) error {
	if err := checkpub.ValidRange(rng); err != nil {
		return err
	}
	terms, err := privateTerms(env)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var meta []checkpub.Text
	for _, name := range []string{"title-file", "body-file"} {
		if !set[name] {
			continue
		}
		text, err := checkpub.ReadInputFile(env.Home, fs.Lookup(name).Value.String())
		if err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
		meta = append(meta, checkpub.Text{Where: "--" + name, Text: text})
	}
	if set["branch"] {
		meta = append(
			meta,
			checkpub.Text{Where: "--branch", Text: fs.Lookup("branch").Value.String()},
		)
	}
	return checkpub.Check(ctx, checkpub.Options{
		Range:  rng,
		Runner: proc.Exec{},
		Terms:  terms,
		Meta:   meta,
		Out:    env.Stderr,
	})
}

// privateTerms loads safety.toml, printing its warnings, and reads the configured private terms. A
// broken safety.toml is an error: the terms it names cannot be trusted.
func privateTerms(env Env) ([]string, error) {
	if env.Home == "" {
		return nil, errNoHome
	}
	cfg, warnings, err := safety.Load(safety.File(env.Home))
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(env.Stderr, "stack-public: %s\n", w)
	}
	return checkpub.ReadTerms(paths.Expand(env.Home, cfg.PrivateTerms))
}
