package proc

import (
	"errors"
	"os"
	"syscall"
)

// exited reports whether p, a started child, has exited. It probes the
// original process handle, so neither a reused PID nor an exit status of 259
// (STILL_ACTIVE) misleads it. A failed probe reports false.
func exited(p *os.Process) bool {
	signaled := false
	err := p.WithHandle(func(h uintptr) {
		ev, err := syscall.WaitForSingleObject(syscall.Handle(h), 0)
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
