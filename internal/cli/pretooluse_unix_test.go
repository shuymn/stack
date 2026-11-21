//go:build unix

package cli

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestPreToolUseSkipsFIFOInputFile checks that a query file that is not a regular file is skipped
// instead of opened: opening a FIFO with no writer would block the hook until its timeout.
func TestPreToolUseSkipsFIFOInputFile(t *testing.T) {
	cwd := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(cwd, "q.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan result, 1)
	go func() {
		done <- runHookStdin(t, t.TempDir(), payload(t, cwd, "Bash",
			map[string]any{"command": "gh api graphql --input q.json"}))
	}()
	select {
	case r := <-done:
		if r.code != exitOK || r.stdout != "" {
			t.Errorf("FIFO input file = %+v, want allow", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook blocked on a FIFO input file")
	}
}
