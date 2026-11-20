package cli

import (
	"os"
	"testing"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// TestMain serves the lock-holder re-exec of the kit tests and the fake executables, then runs the
// tests under the gittest guard of the real repository.
func TestMain(m *testing.M) {
	if path := os.Getenv(lockHolderEnv); path != "" {
		os.Exit(holdLock(path))
	}
	proctest.Main()
	os.Exit(gittest.Main(m))
}
