package proc

import (
	"errors"
	"math"
	"os"
	"syscall"
	"time"
)

// waitForObject is syscall.WaitForSingleObject, swapped only by non-parallel
// tests.
var waitForObject = syscall.WaitForSingleObject

// exited reports whether p, a started child, has exited, waiting up to wait
// for an exit in progress. It waits on the original process handle, so
// neither a reused PID nor an exit status of 259 (STILL_ACTIVE) misleads it. A
// failed probe or a wait that times out reports false.
func exited(p *os.Process, wait time.Duration) bool {
	// Clamped below INFINITE, so a long wait stays bounded.
	millis := uint32(min(max(wait.Milliseconds(), 0), math.MaxUint32-1))
	signaled := false
	err := p.WithHandle(func(h uintptr) {
		ev, err := waitForObject(syscall.Handle(h), millis)
		signaled = err == nil && ev == syscall.WAIT_OBJECT_0
	})
	switch {
	case err == nil:
		return signaled
	case errors.Is(err, os.ErrNoHandle):
		return false
	default:
		// The handle is released, which for a child of Exec only a successful
		// Process.Wait does.
		return true
	}
}

// exitInProgress reports whether a kill error can come from a child that is
// already exiting: TerminateProcess was denied access, likely because the
// process is terminating and its handle is not yet signaled. An access denied
// from Kill's DuplicateHandle does not qualify.
func exitInProgress(err error) bool {
	return syscallFailed(err, "TerminateProcess", syscall.ERROR_ACCESS_DENIED)
}
