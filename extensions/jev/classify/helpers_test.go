package classify

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// realDir is a temporary directory reached through no symlink (macOS /var), so a path git
// reports compares equal to the one the test built.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, ok := parseTime(s)
	if !ok {
		t.Fatalf("time %q", s)
	}
	return v
}
