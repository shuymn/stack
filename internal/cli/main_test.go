package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// builtPublic is the real stack-public that TestMain builds, alone in its directory, for the
// [merge].message_checks tests (C19); builtErr is why it could not be built.
var builtPublic, builtErr = "", error(nil)

// TestMain serves the lock-holder re-exec of the kit tests and the fake executables, builds
// stack-public, then runs the tests under the gittest guard of the real repository.
func TestMain(m *testing.M) {
	if path := os.Getenv(lockHolderEnv); path != "" {
		os.Exit(holdLock(path))
	}
	proctest.Main()
	dir, err := os.MkdirTemp("", "stack-cli-test-")
	if err != nil {
		builtErr = err
	} else {
		builtErr = buildPublic(dir)
	}
	code := gittest.Main(m)
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

func buildPublic(dir string) error {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	builtPublic = filepath.Join(dir, "stack-public"+exe)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", builtPublic,
		"../../extensions/public/cmd/stack-public")
	cmd.Env = gittest.CleanEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build stack-public: %w: %s", err, out)
	}
	return nil
}

func needPublic(t *testing.T) {
	t.Helper()
	if builtErr != nil {
		t.Fatalf("building stack-public failed: %v", builtErr)
	}
}
