package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shuymn/stack/internal/budget"
	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/paths"
)

// runBudgetStatus displays kit sizes and item counts. It is display only and exits 0 (§4.6): a broken
// config falls back to the default kit root, an invalid STACK_SESSION_ID only costs the session
// marks, and every other problem is a warning.
func runBudgetStatus(ctx context.Context, env Env, _ invocation) error {
	warn := func(format string, a ...any) {
		_, _ = fmt.Fprintf(env.Stderr, "stack: "+format+"\n", a...)
	}
	if env.Home == "" {
		warn("%v; no kit root to show", paths.ErrNoHome)
		return nil
	}
	cfg, warnings, err := config.Load(paths.ConfigFile(env.Home))
	if err != nil {
		warn("%v; using the default kit root", err)
		cfg = config.Defaults()
	}
	for _, w := range warnings {
		warn("%s", w)
	}
	root, err := filepath.Abs(paths.FromConfig(env.Home, cfg.Kit.Root))
	if err != nil {
		warn("kit root: %v", err)
		return nil
	}
	id, err := sessionID(env)
	if err != nil {
		warn("%v; session kits are not marked", err)
		id = ""
	}
	if err := budget.Status(ctx, env.Stdout, budget.Options{
		Root:      root,
		Home:      env.Home,
		SessionID: id,
		Stderr:    env.Stderr,
	}); err != nil {
		warn("%v", err)
	}
	return nil
}
