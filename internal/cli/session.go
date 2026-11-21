package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/session"
)

// runSessionStart is `stack hook claude session-start`. It fails open: an undecodable payload or
// any error prints nothing, and a broken config falls back to the defaults.
func runSessionStart(ctx context.Context, env Env, _ invocation) error {
	start := time.Now()
	debug := env.Getenv != nil && hookio.Debug(env.Getenv)
	hookio.Run(env.Stdin, env.Stdout, env.Stderr, debug, func(in hookio.Input) ([]byte, error) {
		cfg := config.Defaults()
		if env.Home != "" {
			if loaded, _, err := config.Load(paths.ConfigFile(env.Home)); err == nil {
				cfg = loaded
			} else if debug {
				_, _ = fmt.Fprintf(env.Stderr, "stack hook: %v; using defaults\n", err)
			}
		}
		out, errs := session.Start(ctx, session.Params{
			Input:  in,
			Home:   env.Home,
			Getenv: env.Getenv,
			Config: cfg,
			Runner: proc.Exec{},
			Stderr: env.Stderr,
			Start:  start,
		})
		if debug {
			for _, err := range errs {
				_, _ = fmt.Fprintf(env.Stderr, "stack hook: %v\n", err)
			}
		}
		return out, nil
	})
	return nil
}
