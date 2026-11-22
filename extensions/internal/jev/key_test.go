package jev

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keySentinel is file content no diagnostic may show.
const keySentinel = "sentinel-key-content"

// writeKey creates the key file under home with content and mode, and returns its path.
func writeKey(t *testing.T, home, content string, mode os.FileMode) string {
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
	return p
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
	if KeyDisplay != "~/.config/stack/jev/key" {
		t.Fatalf("KeyDisplay = %q", KeyDisplay)
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
