package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/extensions/auditdue"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := filepath.Join(home, ".config", "stack")
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cfg, "audit-due.toml"),
		[]byte("merges = 1\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	kit := filepath.Join(t.TempDir(), "demo")
	if err := os.Mkdir(kit, 0o750); err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(map[string]any{
		"stage": "merged",
		"kits":  []map[string]string{{"slug": "demo", "dir": kit}},
		"merge": map[string]any{"pr": 1, "head": "h", "oid": "o"},
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, stdin, status, context string
	}{
		// K = 1 read from HOME: due on the first merge.
		{
			"due", string(req), auditdue.StatusPass,
			"audit due (1 merges since the last due signal): run steward on kit demo",
		},
		{"invalid request", "{", auditdue.StatusWarn, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(strings.NewReader(c.stdin), &out, &errOut); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errOut.String())
			}
			var resp auditdue.Response
			if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
				t.Fatalf("stdout %q: %v", out.String(), err)
			}
			if resp.Status != c.status || resp.Context != c.context || resp.Findings == nil {
				t.Fatalf("response %+v", resp)
			}
		})
	}

	t.Run("stdout closed", func(t *testing.T) {
		var errOut bytes.Buffer
		if code := run(strings.NewReader("{}"), failWriter{}, &errOut); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "write response") {
			t.Fatalf("stderr %q", errOut.String())
		}
	})
}
