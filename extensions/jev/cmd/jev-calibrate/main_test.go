package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunWithoutKey(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), nil, func(string) string { return "" }, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "TYPESAFE_API_KEY is not set") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestRunBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-nope"}, func(string) string { return "k" }, &stdout,
		&stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
}
