package jev

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/extensions/internal/safety"
)

// keySentinel is file content no diagnostic may show.
const keySentinel = "sentinel-key-content"

// writeKey creates the key file under home with content and mode.
func writeKey(t *testing.T, home, content string, mode os.FileMode) {
	t.Helper()
	p := KeyPath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func envOf(v string) func(string) string {
	return func(k string) string {
		if k == KeyEnv {
			return v
		}
		return ""
	}
}

// wantKeyError checks that err is a KeyError with cause, naming the file and never its content.
func wantKeyError(t *testing.T, key string, err error, cause string) {
	t.Helper()
	ke, ok := errors.AsType[*KeyError](err)
	if !ok || key != "" {
		t.Fatalf("Key = %q, %v; want a KeyError %q", key, err, cause)
	}
	if ke.Cause != cause {
		t.Fatalf("cause %q, want %q", ke.Cause, cause)
	}
	if !strings.Contains(err.Error(), KeyDisplay) || strings.Contains(err.Error(), keySentinel) {
		t.Fatalf("diagnostic %q: want the file named and no content", err)
	}
}

func TestKeyPath(t *testing.T) {
	home := t.TempDir()
	want := filepath.Join(home, filepath.FromSlash(".config/stack/jev/key"))
	if got := KeyPath(home); got != want {
		t.Fatalf("KeyPath = %q, want %q", got, want)
	}
	// stack-guard protects the same file.
	if KeyDisplay != "~/.config/stack/jev/key" || KeyDisplay != safety.JevKey {
		t.Fatalf("KeyDisplay = %q, safety.JevKey = %q", KeyDisplay, safety.JevKey)
	}
}

// TestKeyEnvFirst: a non-empty variable is the key and the file is never read, even when it
// would fail.
func TestKeyEnvFirst(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(KeyPath(home), 0o700); err != nil { // a directory: unusable as a file
		t.Fatal(err)
	}
	key, err := Key(envOf(" env-key\n"), home)
	if err != nil || key != "env-key" {
		t.Fatalf("Key = %q, %v; want the trimmed variable", key, err)
	}
}

// TestKeyAbsent: no variable and no file is the opt-out.
func TestKeyAbsent(t *testing.T) {
	for name, getenv := range map[string]func(string) string{
		"unset": envOf(""), "nil getenv": nil, "whitespace": envOf(" \n"),
	} {
		key, err := Key(getenv, t.TempDir())
		if key != "" || err != nil {
			t.Errorf("%s: Key = %q, %v; want no key", name, key, err)
		}
	}
	if key, err := Key(envOf(""), ""); key != "" || err != nil {
		t.Errorf("unknown home: Key = %q, %v; want no key", key, err)
	}
}

func TestKeyFile(t *testing.T) {
	home := t.TempDir()
	writeKey(t, home, "  file-key \r\n", 0o600)
	key, err := Key(envOf(""), home)
	if err != nil || key != "file-key" {
		t.Fatalf("Key = %q, %v; want the trimmed file content", key, err)
	}
}

func TestKeyFileBlank(t *testing.T) {
	home := t.TempDir()
	writeKey(t, home, " \n\t\n", 0o600)
	key, err := Key(envOf(""), home)
	wantKeyError(t, key, err, KeyBlank)
}

func TestKeyFileNotRegular(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(KeyPath(home), 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := Key(envOf(""), home)
	wantKeyError(t, key, err, KeyNotRegular)
}

// TestKeyFileSize: a file of MaxKeyFile bytes is accepted and a longer one refused.
func TestKeyFileSize(t *testing.T) {
	home := t.TempDir()
	writeKey(t, home, strings.Repeat("k", MaxKeyFile), 0o600)
	if key, err := Key(envOf(""), home); err != nil || len(key) != MaxKeyFile {
		t.Fatalf("at the cap: Key = %d bytes, %v", len(key), err)
	}
	writeKey(t, home, keySentinel+strings.Repeat("k", MaxKeyFile), 0o600)
	key, err := Key(envOf(""), home)
	wantKeyError(t, key, err, KeyTooLarge)
}

// symlinkKey makes the key path a symlink to target, skipping where links cannot be made.
func symlinkKey(t *testing.T, home, target string) {
	t.Helper()
	p := KeyPath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
}

// TestKeyFileSymlink: a link is followed and its target checked; a link to nothing is a present
// but unusable key file, not the opt-out.
func TestKeyFileSymlink(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(target, []byte("linked-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkKey(t, home, target)
	if key, err := Key(envOf(""), home); err != nil || key != "linked-key" {
		t.Fatalf("Key = %q, %v; want the target's key", key, err)
	}

	home = t.TempDir()
	symlinkKey(t, home, filepath.Join(t.TempDir(), "absent"))
	key, err := Key(envOf(""), home)
	wantKeyError(t, key, err, KeyDangling)
}

// TestKeyFixes: the causes not about mode or owner never suggest chmod, and an unreadable file
// asks for searchable parent directories.
func TestKeyFixes(t *testing.T) {
	if msg := (&KeyError{Cause: KeyUnreadable}).Error(); !strings.Contains(msg,
		"parent directories searchable") {
		t.Errorf("unreadable: %q does not ask for searchable parent directories", msg)
	}
	for _, cause := range []string{KeyUnreadable, KeyNotRegular, KeyBlank, KeyTooLarge, KeyDangling} {
		if msg := (&KeyError{Cause: cause}).Error(); strings.Contains(msg, "chmod") {
			t.Errorf("%s: %q names chmod", cause, msg)
		}
	}
}

// countingReader yields n bytes of 'k' and counts what it handed out.
type countingReader struct{ n, read int }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.read >= c.n {
		return 0, io.EOF
	}
	m := min(len(p), c.n-c.read)
	for i := range m {
		p[i] = 'k'
	}
	c.read += m
	return m, nil
}

// TestReadKeyBounded: a key file far over the cap is read no further than one byte past it.
func TestReadKeyBounded(t *testing.T) {
	r := &countingReader{n: 1 << 20}
	data, err := readKey(r)
	if err != nil || len(data) != MaxKeyFile+1 || r.read > MaxKeyFile+1 {
		t.Fatalf("readKey = %d bytes, %v; consumed %d, want at most %d", len(data), err, r.read,
			MaxKeyFile+1)
	}
}
