package cli

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/shuymn/stack/internal/checkpub"
	"github.com/shuymn/stack/internal/github"
	"github.com/shuymn/stack/internal/merge"
	"github.com/shuymn/stack/internal/proc"
)

// runPrMerge implements `stack pr merge <pr> <head-sha>`. Invalid arguments, a local path or private
// term in --subject/--body and an unusable config exit 2; every later refusal exits 1.
func runPrMerge(ctx context.Context, env Env, inv invocation) error {
	set := map[string]bool{}
	inv.flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	o := merge.Options{
		PR:            inv.args[0],
		Head:          inv.args[1],
		AllowNoChecks: boolFlagValue(inv.flags, "allow-no-checks"),
		Stdout:        env.Stdout,
		Stderr:        env.Stderr,
		Client: github.Client{
			Runner: proc.Exec{},
			// gh and git find the repository from the working directory, never from variables a git
			// hook exported.
			Env: checkpub.DropRepoVars(os.Environ()),
		},
	}
	if env.Getenv != nil {
		o.GHRepo = env.Getenv("GH_REPO")
	}
	for _, m := range merge.Methods {
		if boolFlagValue(inv.flags, m) {
			o.Methods = append(o.Methods, m)
		}
	}
	o.Merge.DeleteBranch = boolFlagValue(inv.flags, "delete-branch")
	for _, name := range []string{"subject", "body"} {
		if !set[name] {
			continue
		}
		v := inv.flags.Lookup(name).Value.String()
		if name == "subject" {
			o.Merge.Subject = &v
		} else {
			o.Merge.Body = &v
		}
	}
	// Arguments first, so a usage error does not depend on the config.
	if err := merge.Validate(o); err != nil {
		return err
	}
	terms, err := privateTerms(env)
	if err != nil {
		return err
	}
	o.Terms = terms

	err = merge.Run(ctx, o)
	if errors.Is(err, merge.ErrRefused) {
		return refusal{err}
	}
	return err
}

func boolFlagValue(fs *flag.FlagSet, name string) bool {
	g, ok := fs.Lookup(name).Value.(flag.Getter)
	if !ok {
		return false
	}
	b, _ := g.Get().(bool)
	return b
}
