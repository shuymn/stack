package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/safety"
	"github.com/shuymn/stack/internal/gittest"
)

const isWindows = runtime.GOOS == "windows"

// built holds the binaries TestMain builds: stack-guard, and the core stack for the drift and
// two-hook checks.
var built struct {
	stack, guard string
	err          error
}

func TestMain(m *testing.M) {
	cleanup := buildBinaries()
	code := gittest.Main(m)
	cleanup()
	os.Exit(code)
}

func buildBinaries() func() {
	dir, err := os.MkdirTemp("", "stack-guard-test-")
	if err != nil {
		built.err = err
		return func() {}
	}
	exe := ""
	if isWindows {
		exe = ".exe"
	}
	built.stack = filepath.Join(dir, "stack"+exe)
	built.guard = filepath.Join(dir, "stack-guard"+exe)
	env := gittest.CleanEnv(os.Environ())
	for _, c := range [][]string{
		{"build", "-o", built.stack, "../../cmd/stack"},
		{"build", "-o", built.guard, "./cmd/stack-guard"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, "go", c...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			built.err = fmt.Errorf("go %s: %w: %s", strings.Join(c, " "), err, out)
			break
		}
	}
	return func() { _ = os.RemoveAll(dir) }
}

func needBuilt(t *testing.T) {
	t.Helper()
	if built.err != nil {
		t.Fatalf("building the test binaries failed: %v", built.err)
	}
}

type result struct {
	code           int
	stdout, stderr string
}

// runBinary runs bin with args and stdin under an environment holding only the home variables (and
// what Windows needs to start a process), so the user's real home is never read. With home "" the
// home variables are unset.
func runBinary(t *testing.T, bin, home, stdin string, args ...string) result {
	t.Helper()
	needBuilt(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{}
	if home != "" {
		cmd.Env = append(cmd.Env, "HOME="+home, "USERPROFILE="+home)
	}
	if root := os.Getenv("SYSTEMROOT"); root != "" {
		cmd.Env = append(cmd.Env, "SYSTEMROOT="+root)
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", bin, err)
	}
	return result{code, stdout.String(), stderr.String()}
}

// guardBin and coreBin run the two PreToolUse hooks.
func guardBin(t *testing.T, home, stdin string) result {
	t.Helper()
	return runBinary(t, built.guard, home, stdin, "hook", "claude", "pre-tool-use")
}

func coreBin(t *testing.T, home, stdin string) result {
	t.Helper()
	return runBinary(t, built.stack, home, stdin, "hook", "claude", "pre-tool-use")
}

// runHook runs the hook in process with home as Env.Home.
func runHook(t *testing.T, home, stdin string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main([]string{"hook", "claude", "pre-tool-use"}, Env{
		Stdin: strings.NewReader(stdin), Stdout: &stdout, Stderr: &stderr, Home: home,
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

func bashPayload(t *testing.T, cwd, command string) string {
	t.Helper()
	return payload(t, cwd, "Bash", map[string]any{"command": command})
}

type denyOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// deniedBy returns the rule of a deny printed with prefix ("stack-guard" or "stack guard"), or ""
// when r is not such a deny.
func deniedBy(r result, prefix string) string {
	var out denyOutput
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil ||
		out.HookSpecificOutput.HookEventName != "PreToolUse" ||
		out.HookSpecificOutput.PermissionDecision != "deny" {
		return ""
	}
	rest, ok := strings.CutPrefix(out.HookSpecificOutput.PermissionDecisionReason, prefix+" (")
	if !ok {
		return ""
	}
	rule, _, ok := strings.Cut(rest, "): ")
	if !ok {
		return ""
	}
	return rule
}

func assertDeny(t *testing.T, name string, r result, rule string) {
	t.Helper()
	if got := deniedBy(r, "stack-guard"); got != rule {
		t.Errorf("%s = %+v, want a stack-guard (%s) deny", name, r, rule)
	}
}

func assertAllow(t *testing.T, name string, r result) {
	t.Helper()
	if r.code != 0 || r.stdout != "" {
		t.Errorf("%s = %+v, want exit 0 and no output", name, r)
	}
}

// writeSafety writes home's safety.toml.
func writeSafety(t *testing.T, home, text string) {
	t.Helper()
	p := safety.File(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// localPath returns a home path that publictext reports, built at run time so this package holds
// no literal one.
func localPath() string {
	return strings.Join([]string{"", "Users", "dev", "notes.md"}, "/")
}

// t3Secrets is the default T3 secrets entry (`~/…`), and t3Rel its home-relative part.
var (
	t3Secrets = safety.T3Secrets
	t3Rel     = strings.TrimPrefix(safety.T3Secrets, "~/")
)
