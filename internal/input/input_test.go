package input

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadFile covers the resolution of an argument: `~` against home, a relative path against the
// current directory, and the input errors (missing, not UTF-8, a directory).
func TestReadFile(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Chdir(cwd)
	write(t, filepath.Join(home, "title.txt"), []byte("docs: role\n"))
	write(t, filepath.Join(cwd, "body.md"), []byte("body\n"))
	write(t, filepath.Join(cwd, "empty.md"), nil)
	write(t, filepath.Join(cwd, "latin1.md"), []byte("caf\xe9\n"))

	for _, tt := range []struct {
		arg, want string
	}{
		{"~/title.txt", "docs: role\n"},
		{"body.md", "body\n"},
		{filepath.Join(cwd, "body.md"), "body\n"},
		{"empty.md", ""},
	} {
		got, err := ReadFile(home, tt.arg)
		if err != nil || got != tt.want {
			t.Errorf("ReadFile(%q) = %q, %v; want %q", tt.arg, got, err, tt.want)
		}
	}
	for _, tt := range []struct {
		arg, want string
	}{
		{"none.md", "cannot read "},
		{"latin1.md", "not UTF-8"},
		{".", "cannot read "},
	} {
		_, err := ReadFile(home, tt.arg)
		if !errors.Is(err, ErrInput) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ReadFile(%q) = %v, want ErrInput with %q", tt.arg, err, tt.want)
		}
	}
}
