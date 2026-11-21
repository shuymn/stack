package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/shuymn/stack/internal/checkpub"
	"github.com/shuymn/stack/internal/input"
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
	terms, err := privateTerms(env)
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
		text, err := input.ReadFile(env.Home, inv.flags.Lookup(name).Value.String())
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

// privateTerms loads the config, printing its warnings, and reads the configured private terms.
func privateTerms(env Env) ([]string, error) {
	cfg, err := loadConfig(env)
	if err != nil {
		return nil, err
	}
	return checkpub.ReadTerms(paths.Expand(env.Home, cfg.Safety.PrivateTerms))
}
