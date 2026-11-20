package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/paths"
)

func runHookStdin(t *testing.T, home, stdin string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(t.Context(), []string{"hook", "claude", "pre-tool-use"}, Env{
		Stdin:  strings.NewReader(stdin),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) string { return "" },
		Home:   home,
	})
	return result{code, stdout.String(), stderr.String()}
}

func payload(t *testing.T, cwd, tool string, input any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id":      "0b9c6f7e-3d1a-4c2b-9f5e-8a7d6c5b4a39",
		"cwd":             cwd,
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"tool_input":      input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type denyOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func assertDeny(t *testing.T, name string, r result, rule string) {
	t.Helper()
	var out denyOutput
	if r.code != exitOK || json.Unmarshal([]byte(r.stdout), &out) != nil ||
		out.HookSpecificOutput.HookEventName != "PreToolUse" ||
		out.HookSpecificOutput.PermissionDecision != "deny" ||
		!strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "("+rule+")") {
		t.Errorf("%s = %+v, want a %s deny", name, r, rule)
	}
}

func writeConfig(t *testing.T, home, text string) {
	t.Helper()
	p := paths.ConfigFile(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHookConfigFallback is the CLI half of C2: under a broken config the hook applies the §5
// defaults, so the secrets rule still denies.
func TestHookConfigFallback(t *testing.T) {
	read := func(cwd string) string {
		return payload(t, cwd, "Read", map[string]any{"file_path": ".dev.vars"})
	}
	for name, cfg := range map[string]string{
		"no config":    "",
		"syntax error": "version = 1\n[safety\nsecrets = []\n",
		"version 2":    "version = 2\n[safety]\nsecrets = []\ndeny_pencil_generate = false\n",
		"type error":   "version = 1\n[safety]\nsecrets = \"x\"\ndeny_pencil_generate = false\n",
	} {
		home := t.TempDir()
		if cfg != "" {
			writeConfig(t, home, cfg)
		}
		cwd := t.TempDir()
		assertDeny(t, name+": Read secret", runHookStdin(t, home, read(cwd)), "secrets")
	}
}

func TestPreToolUseMalformedStdin(t *testing.T) {
	for _, stdin := range []string{
		"",
		"x",
		"[]",
		`{"tool_name":`,
		`{"tool_name":"Bash","tool_input":"rm -rf /"}`,
		`{"tool_name":"Read","tool_input":{"file_path":7}}`,
		"\x00\xff",
	} {
		r := runHookStdin(t, t.TempDir(), stdin)
		if r.code != exitOK || r.stdout != "" || r.stderr != "" {
			t.Errorf("stdin %q = %+v, want exit 0 and no output", stdin, r)
		}
	}
}

func TestPreToolUseAllowsOrdinaryCommand(t *testing.T) {
	r := runHookStdin(t, t.TempDir(),
		payload(t, t.TempDir(), "Bash", map[string]any{"command": "git status"}))
	if r.code != exitOK || r.stdout != "" || r.stderr != "" {
		t.Errorf("git status = %+v, want exit 0 and no output", r)
	}
}
