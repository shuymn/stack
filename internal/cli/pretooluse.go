package cli

import (
	"context"

	"github.com/shuymn/stack/internal/guard"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/powershell"
)

// runPreToolUse implements `stack hook claude pre-tool-use`: it prints a deny for a raw merge and
// nothing otherwise. It fails open (hookio.Run) and reads no config.
func runPreToolUse(_ context.Context, env Env, _ invocation) error {
	hookio.Run(env.Stdin, env.Stdout, env.Stderr, hookio.Debug(getenv(env)),
		func(in hookio.Input) ([]byte, error) {
			d, ok := guard.Decide(in, guard.Options{Home: env.Home, OS: powershell.HostOS()})
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
