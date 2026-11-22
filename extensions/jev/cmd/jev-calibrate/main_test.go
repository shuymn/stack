package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestRunWithoutKey(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), nil, noEnv, t.TempDir(), &stdout, &stderr)
	want := "jev-calibrate: no key: set TYPESAFE_API_KEY or create ~/.config/stack/jev/key\n"
	if code != 2 || stderr.String() != want {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

// TestRunUnusableKeyFile: the key file is looked up through the shared resolver, whose
// diagnostic is reported as is.
func TestRunUnusableKeyFile(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".config", "stack", "jev", "key")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), nil, noEnv, home, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "key file ~/.config/stack/jev/key is blank") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestRunWithoutHome(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), nil, func(string) string { return "k" }, "", &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "home directory unknown") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestRunBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-nope"}, func(string) string { return "k" }, t.TempDir(),
		&stdout, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
}
