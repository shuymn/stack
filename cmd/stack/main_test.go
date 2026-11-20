package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := run([]string{"--version"}, &stdout, &stderr); got != 0 {
		t.Fatalf("run(--version) = %d, want 0", got)
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("stdout = %q, want %q", stdout.String(), version)
	}

	stdout.Reset()
	stderr.Reset()
	if got := run([]string{"unknown"}, &stdout, &stderr); got != 2 {
		t.Fatalf("run(unknown) = %d, want 2", got)
	}
}
