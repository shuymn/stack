package proc

import (
	"os"
	"strings"
	"testing"
)

// Exited is the platform probe of the kill seam.
var Exited = exited

// SetKillHooks replaces the runner's kill and exit probe until tb ends. Tests
// that call it must not run in parallel.
func SetKillHooks(tb testing.TB, kill func(*os.Process) error, exitedHook func(*os.Process) bool) {
	tb.Helper()
	oldKill, oldExited := killProcess, processExited
	killProcess, processExited = kill, exitedHook
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
