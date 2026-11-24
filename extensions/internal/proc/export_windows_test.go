package proc

import (
	"syscall"
	"testing"
)

// ExitGrace is how long the runner waits for an exit in progress.
const ExitGrace = exitGrace

// SetWaitHook replaces the runner's wait on the child's process handle until
// tb ends. Tests that call it must not run in parallel.
func SetWaitHook(tb testing.TB, wait func(h syscall.Handle, millis uint32) (uint32, error)) {
	tb.Helper()
	old := waitForObject
	waitForObject = wait
	tb.Cleanup(func() { waitForObject = old })
}
