//go:build unix

package guard

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSkipsFIFOBodyFile checks that a body file that is not a regular file is skipped instead of
// opened: opening a FIFO with no writer would block the hook until its timeout.
func TestSkipsFIFOBodyFile(t *testing.T) {
	f := newFixture(t)
	if err := syscall.Mkfifo(filepath.Join(f.cwd, "b.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan result, 1)
	go func() { done <- f.bash("gh pr create -t t --body-file b.md") }()
	select {
	case r := <-done:
		assertAllow(t, "FIFO body file", r)
	case <-time.After(5 * time.Second):
		t.Fatal("the hook blocked on a FIFO body file")
	}
}
