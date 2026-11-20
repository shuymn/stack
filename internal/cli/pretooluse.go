package cli

import (
	"context"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/guard"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/paths"
)

// runPreToolUse implements `stack hook claude pre-tool-use`: it prints a deny for the hard-boundary
// hazards and nothing otherwise. It fails open (hookio.Run), and a broken config falls back to the
// defaults, so the secrets rule never turns off because of a typo.
func runPreToolUse(_ context.Context, env Env, _ invocation) error {
	hookio.Run(env.Stdin, env.Stdout, env.Stderr, hookio.Debug(getenv(env)),
		func(in hookio.Input) ([]byte, error) {
			cfg := config.Defaults()
			if env.Home != "" {
				cfg = config.LoadForHook(paths.ConfigFile(env.Home))
			}
			d, ok := guard.Decide(in, guard.Options{Config: cfg, Home: env.Home})
			if !ok {
				return nil, nil
			}
			return hookio.Deny(d.Reason)
		})
	return nil
}

func getenv(env Env) func(string) string {
	if env.Getenv == nil {
		return func(string) string { return "" }
	}
	return env.Getenv
}
