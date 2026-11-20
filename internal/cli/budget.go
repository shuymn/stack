package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shuymn/stack/internal/budget"
	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/paths"
)

// runBudgetStatus displays kit sizes and item counts. Only a config or kit root that cannot be read
// fails it; an invalid STACK_SESSION_ID only costs the session marks.
func runBudgetStatus(ctx context.Context, env Env, _ invocation) error {
	if env.Home == "" {
		return paths.ErrNoHome
	}
	cfg, warnings, err := config.Load(paths.ConfigFile(env.Home))
	if err != nil {
		return err
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(env.Stderr, "stack: %s\n", w)
	}
	root, err := filepath.Abs(paths.FromConfig(env.Home, cfg.Kit.Root))
	if err != nil {
		return fmt.Errorf("kit root: %w", err)
	}
	id, err := sessionID(env)
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "stack: %v; session kits are not marked\n", err)
		id = ""
	}
	return budget.Status(ctx, env.Stdout, budget.Options{
		Root:      root,
		Home:      env.Home,
		SessionID: id,
		Stderr:    env.Stderr,
	})
}
