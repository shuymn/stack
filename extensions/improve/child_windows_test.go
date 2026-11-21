//go:build windows

package improve

import "testing"

// requireGone is not checked on Windows: only the direct child is killed there (design §3.1),
// and the tests that call it are skipped.
func requireGone(*testing.T, int) {}
