//go:build !windows

package proc

import "os"

// exited reports whether p, a started child, has exited. On Unix killing an
// exited but unreaped child succeeds, so a failed kill never comes from a child
// that exited on its own.
func exited(*os.Process) bool { return false }
