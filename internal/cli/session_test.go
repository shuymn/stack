package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionStart pipes payload into `stack hook claude session-start` with home and env.
func sessionStart(t *testing.T, home string, env map[string]string, payload string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(t.Context(), []string{"hook", "claude", "session-start"}, Env{
		Stdin:  strings.NewReader(payload),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(k string) string { return env[k] },
		Home:   home,
	})
	return result{code, stdout.String(), stderr.String()}
}

func TestSessionStart(t *testing.T) {
	home := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env")
	env := map[string]string{"STACK_SESSION_ID": "s1", "CLAUDE_ENV_FILE": envFile}
	if r := runIn(t, home, env, "kit", "open", "demo"); r.code != exitOK {
		t.Fatalf("kit open = %+v", r)
	}
	statePath := filepath.Join(home, ".local", "state", "stack", "kits", "demo", "state.md")
	writeFile(t, statePath, []byte("DEMO STATE\n"))

	r := sessionStart(t, home, env, `{"session_id":"s1","source":"startup"}`)
	if r.code != exitOK || r.stdout != "" || r.stderr != "" {
		t.Fatalf("startup = %+v", r)
	}
	data, err := os.ReadFile(envFile)
	if err != nil || string(data) != "export STACK_SESSION_ID='s1'\n" {
		t.Fatalf("env file = %q, %v", data, err)
	}

	r = sessionStart(t, home, env, `{"session_id":"s1","source":"compact"}`)
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if r.code != exitOK || json.Unmarshal([]byte(r.stdout), &out) != nil ||
		out.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, "DEMO STATE") {
		t.Fatalf("compact = %+v", r)
	}

	// A broken config falls back to the defaults: the default agent type still marks a worker.
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"), []byte("version = \n"))
	writeFile(t, filepath.Join(home, ".claude", "agents", "opus-worker.md"), []byte("RULES\n"))
	r = sessionStart(
		t,
		home,
		nil,
		`{"session_id":"s2","source":"compact","agent_type":"opus-worker"}`,
	)
	if r.code != exitOK || !strings.Contains(r.stdout, "RULES") || r.stderr != "" {
		t.Fatalf("compact with broken config = %+v", r)
	}

	for _, payload := range []string{"x", "", "[]", `{"session_id":1}`} {
		if r := sessionStart(t, home, env, payload); r.code != exitOK || r.stdout != "" ||
			r.stderr != "" {
			t.Errorf("payload %q = %+v, want exit 0 and no output", payload, r)
		}
	}
}
