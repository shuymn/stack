package proc

import (
	"os"
	"strings"
	"testing"
	"time"
)

// SetKillHooks replaces the runner's kill and, unless exitedHook is nil, its
// exit probe until tb ends; the probe answers at once, whatever the wait. Tests
// that call it must not run in parallel.
func SetKillHooks(tb testing.TB, kill func(*os.Process) error, exitedHook func(*os.Process) bool) {
	tb.Helper()
	oldKill, oldExited := killProcess, processExited
	killProcess = kill
	if exitedHook != nil {
		processExited = func(p *os.Process, _ time.Duration) bool { return exitedHook(p) }
	}
	tb.Cleanup(func() { killProcess, processExited = oldKill, oldExited })
}

// QuietExitEnv is the environment with the race detector's exit sleep turned
// off for a child, keeping any other GORACE options.
func QuietExitEnv() []string {
	return append(
		os.Environ(),
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"),
	)
}
