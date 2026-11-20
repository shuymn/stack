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
// defaults, so the secrets and Pencil rules still deny.
func TestHookConfigFallback(t *testing.T) {
	read := func(cwd string) string {
		return payload(t, cwd, "Read", map[string]any{"file_path": ".dev.vars"})
	}
	pencil := func(cwd string) string {
		return payload(t, cwd, "mcp__pencil__execute",
			map[string]any{"input": `await Generate({prompt: "icon"})`})
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
		assertDeny(t, name+": Pencil Generate", runHookStdin(t, home, pencil(cwd)), "pencil")
	}

	home := t.TempDir()
	writeConfig(t, home, "version = 1\n[safety]\ndeny_pencil_generate = false\n")
	if r := runHookStdin(t, home, pencil(t.TempDir())); r.code != exitOK || r.stdout != "" {
		t.Errorf("valid config disabling the Pencil rule = %+v, want allow", r)
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

// TestPreToolUseRegressions runs review findings through the CLI entry point. rule "" means allow.
func TestPreToolUseRegressions(t *testing.T) {
	home := strings.Join([]string{"", "home", "dev"}, "/") // a local path, built at run time
	tests := []struct {
		name, tool string
		input      map[string]any
		rule       string
	}{
		{
			"guarded call in an if body after cd", "Bash",
			map[string]any{"command": "if cd docs; then gh pr merge -R o/r 1 --squash; fi"},
			"raw-merge",
		},
		{
			"empty here-string before a secret read", "Bash",
			map[string]any{"command": `: <<< ""; cat .dev.vars`},
			"secrets",
		},
		{
			"heredoc on a compound command reaches its gh call",
			"Bash",
			map[string]any{
				"command": "{ gh issue comment 1 --body-file -; } <<'EOF'\nsee " + home + "/x\nEOF",
			},
			"gh-publish",
		},
		{
			"heredoc on an if clause reaches its gh call",
			"Bash",
			map[string]any{
				"command": "if true; then gh issue comment 1 -F -; fi <<'EOF'\nsee " + home + "/x\nEOF",
			},
			"gh-publish",
		},
		{
			"ANSI-C quoted body", "Bash",
			map[string]any{"command": "gh pr create -t t -b $'see\\n" + home + "/x'"},
			"gh-publish",
		},
		{
			"command -v only looks the name up", "Bash",
			map[string]any{"command": "command -v gh pr merge 1 && command -V rm -rf ~"},
			"",
		},
		{
			"command without -v still runs it", "Bash",
			map[string]any{"command": "command -p gh pr merge 1"},
			"raw-merge",
		},
		{
			"negated Grep glob excludes the secret", "Grep",
			map[string]any{"pattern": "KEY", "glob": "!**/.dev.vars"},
			"",
		},
		{
			"negated Grep glob does not hide a secret path", "Grep",
			map[string]any{"pattern": "KEY", "path": ".dev.vars", "glob": "!*.md"},
			"secrets",
		},
		{
			"gh api help merges nothing", "Bash",
			map[string]any{"command": "gh api --help -X PUT repos/o/r/pulls/1/merge"},
			"",
		},
		{
			"gh pr create help publishes nothing", "Bash",
			map[string]any{"command": "gh pr create -h --body '" + home + "/x'"},
			"",
		},
		{
			"gh pr create dry run publishes nothing", "Bash",
			map[string]any{"command": "gh pr create --dry-run -t t --body '" + home + "/x'"},
			"",
		},
		{
			"gh pr create dry run turned off publishes", "Bash",
			map[string]any{"command": "gh pr create --dry-run=false -t t --body '" + home + "/x'"},
			"gh-publish",
		},
		{
			"Pencil call through a constant index", "mcp__pencil__execute",
			map[string]any{"input": `await api["Generate"]({prompt: "logo"})`},
			"pencil",
		},
	}
	for _, tt := range tests {
		r := runHookStdin(t, t.TempDir(), payload(t, t.TempDir(), tt.tool, tt.input))
		if tt.rule == "" {
			if r.code != exitOK || r.stdout != "" {
				t.Errorf("%s = %+v, want allow", tt.name, r)
			}
			continue
		}
		assertDeny(t, tt.name, r, tt.rule)
	}
}

// TestPreToolUseInheritedStdinCwd checks that `( cd sub && … ) < F` reads F where the shell opens
// it, the outer cwd, not the cwd of the command inside.
func TestPreToolUseInheritedStdinCwd(t *testing.T) {
	home := strings.Join([]string{"", "home", "dev"}, "/") // a local path, built at run time
	const cmd = "(cd docs && gh issue comment 1 -F -) < body.md"
	for name, files := range map[string]map[string]string{
		"gh-publish": {"body.md": "see " + home + "/x\n", "docs/body.md": "clean\n"},
		"":           {"body.md": "clean\n", "docs/body.md": "see " + home + "/x\n"},
	} {
		cwd := t.TempDir()
		for rel, content := range files {
			p := filepath.Join(cwd, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r := runHookStdin(t, t.TempDir(), payload(t, cwd, "Bash", map[string]any{"command": cmd}))
		if name == "" {
			if r.code != exitOK || r.stdout != "" {
				t.Errorf("clean outer body.md = %+v, want allow", r)
			}
			continue
		}
		assertDeny(t, "local path in the outer body.md", r, name)
	}
}

func TestPreToolUseAllowsOrdinaryCommand(t *testing.T) {
	r := runHookStdin(t, t.TempDir(),
		payload(t, t.TempDir(), "Bash", map[string]any{"command": "git status"}))
	if r.code != exitOK || r.stdout != "" || r.stderr != "" {
		t.Errorf("git status = %+v, want exit 0 and no output", r)
	}
}
