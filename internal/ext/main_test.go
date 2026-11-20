package ext

import (
	"os"
	"testing"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// TestMain serves the fake extensions, then runs the tests under the gittest guard of the real
// repository.
func TestMain(m *testing.M) {
	proctest.Main()
	os.Exit(gittest.Main(m))
}
