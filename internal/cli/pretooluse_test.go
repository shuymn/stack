package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
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

// assertRawMergeDeny checks for the core hook's raw-merge deny.
func assertRawMergeDeny(t *testing.T, name string, r result) {
	t.Helper()
	var out denyOutput
	if r.code != exitOK || json.Unmarshal([]byte(r.stdout), &out) != nil ||
		out.HookSpecificOutput.HookEventName != "PreToolUse" ||
		out.HookSpecificOutput.PermissionDecision != "deny" ||
		!strings.HasPrefix(out.HookSpecificOutput.PermissionDecisionReason,
			"stack guard (raw-merge): ") {
		t.Errorf("%s = %+v, want a stack guard (raw-merge) deny", name, r)
	}
}

func assertNoOutput(t *testing.T, name string, r result) {
	t.Helper()
	if r.code != exitOK || r.stdout != "" || r.stderr != "" {
		t.Errorf("%s = %+v, want exit 0 and no output", name, r)
	}
}

// TestPreToolUseCoreOnly is C18: the core hook denies raw merges and allows everything else, also
// the inputs the stack-guard rules deny, under the old [safety] defaults and a config that still
// has them.
func TestPreToolUseCoreOnly(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"), []byte(
		"[safety]\nsecrets = [\".dev.vars\"]\ndeny_pencil_generate = true\n"))
	local := strings.Join([]string{"", "home", "dev"}, "/") // a local path, built at run time
	root := "/"
	if runtime.GOOS == "windows" {
		root = "C:/"
	}
	for _, h := range []string{home, t.TempDir(), ""} {
		cwd := t.TempDir()
		writeFile(t, filepath.Join(cwd, ".dev.vars"), []byte("KEY=x\n"))
		for name, stdin := range map[string]string{
			"Read .dev.vars": payload(t, cwd, "Read", map[string]any{"file_path": ".dev.vars"}),
			"Grep .dev.vars": payload(t, cwd, "Grep",
				map[string]any{"pattern": "KEY", "path": ".dev.vars"}),
			"cat .dev.vars": payload(t, cwd, "Bash", map[string]any{"command": "cat .dev.vars"}),
			"rm -rf root":   payload(t, cwd, "Bash", map[string]any{"command": "rm -rf " + root}),
			"rm -rf ~":      payload(t, cwd, "Bash", map[string]any{"command": "rm -rf ~"}),
			"gh publish": payload(t, cwd, "Bash",
				map[string]any{"command": "gh pr create -t t --body 'see " + local + "/x'"}),
			"Pencil Generate": payload(t, cwd, "mcp__pencil__execute",
				map[string]any{"input": `await Generate({prompt: "icon"})`}),
		} {
			assertNoOutput(t, name+" (home "+strconv.Quote(h)+")", runHookStdin(t, h, stdin))
		}
		merge := payload(t, cwd, "Bash", map[string]any{"command": "gh pr merge 1"})
		assertRawMergeDeny(t, "gh pr merge 1", runHookStdin(t, h, merge))
		// An input matching raw-merge and a moved rule is denied by core for raw-merge (§4.9).
		both := payload(t, cwd, "Bash", map[string]any{"command": "cat .dev.vars && gh pr merge 1"})
		assertRawMergeDeny(t, "secret read and raw merge", runHookStdin(t, h, both))
	}
}

func TestPreToolUseMalformedStdin(t *testing.T) {
	for _, stdin := range []string{
		"",
		"x",
		"[]",
		`{"tool_name":`,
		`{"tool_name":"Bash","tool_input":"gh pr merge 1"}`,
		`{"tool_name":"Bash","tool_input":{"command":7}}`,
		"\x00\xff",
	} {
		assertNoOutput(t, strconv.Quote(stdin), runHookStdin(t, t.TempDir(), stdin))
	}
}

// TestPreToolUseRegressions runs review findings through the CLI entry point.
func TestPreToolUseRegressions(t *testing.T) {
	tests := []struct {
		name, command string
		deny          bool
	}{
		{
			"guarded call in an if body after cd",
			"if cd docs; then gh pr merge -R o/r 1 --squash; fi",
			true,
		},
		{"command -v only looks the name up", "command -v gh pr merge 1", false},
		{"command without -v still runs it", "command -p gh pr merge 1", true},
		{"gh api help merges nothing", "gh api --help -X PUT repos/o/r/pulls/1/merge", false},
	}
	for _, tt := range tests {
		r := runHookStdin(t, t.TempDir(),
			payload(t, t.TempDir(), "Bash", map[string]any{"command": tt.command}))
		if tt.deny {
			assertRawMergeDeny(t, tt.name, r)
		} else {
			assertNoOutput(t, tt.name, r)
		}
	}
}

// TestPreToolUseInheritedStdinCwd checks that `( cd sub && … ) < F` reads F where the shell opens
// it, the outer cwd, not the cwd of the command inside.
func TestPreToolUseInheritedStdinCwd(t *testing.T) {
	mutation := `{"query":"mutation { mergePullRequest(input: {}) { clientMutationId } }"}`
	read := `{"query":"query { viewer { login } }"}`
	const cmd = "(cd docs && gh api graphql --input -) < in.json"
	for name, files := range map[string]map[string]string{
		"merge": {"in.json": mutation, "docs/in.json": read},
		"read":  {"in.json": read, "docs/in.json": mutation},
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
		if name == "merge" {
			assertRawMergeDeny(t, "merge mutation in the outer in.json", r)
		} else {
			assertNoOutput(t, "query in the outer in.json", r)
		}
	}
}

func TestPreToolUseAllowsOrdinaryCommand(t *testing.T) {
	r := runHookStdin(t, t.TempDir(),
		payload(t, t.TempDir(), "Bash", map[string]any{"command": "git status"}))
	assertNoOutput(t, "git status", r)
}
