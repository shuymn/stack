package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/shuymn/stack/internal/checkpub"
	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/github"
	"github.com/shuymn/stack/internal/merge"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
)

// runPrMerge implements `stack pr merge <pr> <head-sha>`. Invalid arguments, a local path or private
// term in --subject/--body and an unusable config exit 2; every later refusal exits 1. After a
// verified merge it runs [stages].merged, whose results never change the exit code.
//
// SIGINT and SIGTERM cancel the run instead of ending the process, so the running child is killed
// and a merge call that already started is still read back; with --auto, auto-merge is disabled
// first. Only this command catches them; every
// other command keeps the default exit on signal.
func runPrMerge(ctx context.Context, env Env, inv invocation) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A consumer that closed stdout or stderr (e.g. `| head -1`) must not end stack while
	// auto-merge may be enabled: with SIGPIPE caught, writes there just fail and cleanup still runs.
	// It stays caught until the process exits, so the final reason cannot end it by signal either.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
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
			Env: proc.DropRepoVars(os.Environ()),
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
	o.Merge.Auto = boolFlagValue(inv.flags, "auto")
	o.Merge.DeleteBranch = boolFlagValue(inv.flags, "delete-branch")
	// Visit sees only the flags given, so an explicit empty value stays distinct from none.
	inv.flags.Visit(func(f *flag.Flag) {
		v := f.Value.String()
		switch f.Name {
		case "subject":
			o.Merge.Subject = &v
		case "body":
			o.Merge.Body = &v
		}
	})
	// Arguments first, so a usage error does not depend on the config.
	if err := merge.Validate(o); err != nil {
		return err
	}
	// One config load supplies the private terms and [stages].merged.
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	termsFile := paths.Expand(env.Home, cfg.Safety.PrivateTerms)
	if o.Terms, err = checkpub.ReadTerms(termsFile); err != nil {
		return err
	}
	if err := checkStageNames(env, ext.StageMerged, cfg.Stages.Merged); err != nil {
		return err
	}
	// [merge].message_checks is validated with the rest of the config; nothing runs it yet.
	if err := ext.CheckNames(cfg.Merge.MessageChecks); err != nil {
		return fmt.Errorf("%w: %s: [merge].message_checks: %w", config.ErrInvalid,
			paths.ConfigFile(env.Home), err)
	}
	lc := &lazyConfig{env: env, loaded: true, cfg: cfg}
	o.Merged = merge.MergedStage{
		Names: cfg.Stages.Merged,
		Kits:  func(ctx context.Context) *[]ext.Kit { return runKits(ctx, env, lc) },
		Ext:   extOptions(env),
	}

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
