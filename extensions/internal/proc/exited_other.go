//go:build !windows

package proc

import (
	"os"
	"time"
)

// exited reports whether p, a started child, has exited. On Unix killing an
// exited but unreaped child succeeds, so a failed kill never comes from a child
// that exited on its own.
func exited(*os.Process, time.Duration) bool { return false }

// exitInProgress reports whether a kill error can come from a child that is
// already exiting; on Unix none does.
func exitInProgress(error) bool { return false }
