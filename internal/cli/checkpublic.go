package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/shuymn/stack/internal/checkpub"
	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
)

var errNoHome = errors.New("home directory unknown")

// runCheckPublic implements `stack check public <base>..<head>`. Every input is validated before git
// runs; hits go to stderr and exit 1, anything that cannot be verified exits 2.
func runCheckPublic(ctx context.Context, env Env, inv invocation) error {
	rng := inv.args[0]
	if err := checkpub.ValidRange(rng); err != nil {
		return err
	}
	if env.Home == "" {
		return errNoHome
	}
	cfgPath := paths.ConfigFile(env.Home)
	cfg, warnings, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(env.Stderr, "stack: %s\n", w)
	}
	terms, err := checkpub.ReadTerms(paths.Expand(env.Home, cfg.Safety.PrivateTerms))
	if err != nil {
		return err
	}

	set := map[string]bool{}
	inv.flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var meta []checkpub.Text
	for _, name := range []string{"title-file", "body-file"} {
		if !set[name] {
			continue
		}
		text, err := readInputFile(env.Home, inv.flags.Lookup(name).Value.String())
		if err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
		meta = append(meta, checkpub.Text{Where: "--" + name, Text: text})
	}
	if set["branch"] {
		meta = append(
			meta,
			checkpub.Text{Where: "--branch", Text: inv.flags.Lookup("branch").Value.String()},
		)
	}

	err = checkpub.Check(ctx, checkpub.Options{
		Range:  rng,
		Runner: proc.Exec{},
		Terms:  terms,
		Meta:   meta,
		Out:    env.Stderr,
	})
	if errors.Is(err, checkpub.ErrFound) {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return err
}

// readInputFile reads a --title-file or --body-file argument, resolving `~` and, on Windows, the Git
// Bash `/c/…` forms against the current directory.
func readInputFile(home, arg string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("current directory: %w", err)
	}
	p, ok := paths.Resolve(home, cwd, arg, true)
	if !ok {
		return "", fmt.Errorf("%w %s: cannot resolve the path", checkpub.ErrInput, arg)
	}
	return checkpub.ReadUTF8(p)
}
