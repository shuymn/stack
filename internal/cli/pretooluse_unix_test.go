//go:build unix

package cli

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestPreToolUseSkipsFIFOBodyFile checks that a body file that is not a regular file is skipped
// instead of opened: opening a FIFO with no writer would block the hook until its timeout.
func TestPreToolUseSkipsFIFOBodyFile(t *testing.T) {
	cwd := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(cwd, "b.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan result, 1)
	go func() {
		done <- runHookStdin(t, t.TempDir(), payload(t, cwd, "Bash",
			map[string]any{"command": "gh pr create -t t --body-file b.md"}))
	}()
	select {
	case r := <-done:
		if r.code != exitOK || r.stdout != "" {
			t.Errorf("FIFO body file = %+v, want allow", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook blocked on a FIFO body file")
	}
}
