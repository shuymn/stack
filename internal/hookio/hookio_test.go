package hookio

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestFailOpen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		stdin string
		fn    func(Input) ([]byte, error)
	}{
		{
			name:  "undecodable stdin",
			stdin: "x",
			fn:    func(Input) ([]byte, error) { return Deny("unreachable") },
		},
		{
			name:  "non-object stdin",
			stdin: "null",
			fn:    func(Input) ([]byte, error) { return Deny("unreachable") },
		},
		{
			name:  "callback error with output",
			stdin: `{"tool_name":"Read"}`,
			fn: func(Input) ([]byte, error) {
				return []byte("partial"), errors.New("boom")
			},
		},
		{
			name:  "panic",
			stdin: `{"tool_name":"Read"}`,
			fn:    func(Input) ([]byte, error) { panic("boom") },
		},
		{
			name:  "nil panic",
			stdin: `{"tool_name":"Read"}`,
			fn:    func(Input) ([]byte, error) { panic(nil) },
		},
	}
	for _, tc := range cases {
		for _, showErrors := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var stdout, stderr bytes.Buffer
				code := Run(strings.NewReader(tc.stdin), &stdout, &stderr, showErrors, tc.fn)
				if code != 0 {
					t.Fatalf("exit = %d, want 0", code)
				}
				if stdout.Len() != 0 {
					t.Fatalf("stdout = %q, want empty", stdout.String())
				}
				if showErrors == (stderr.Len() == 0) {
					t.Fatalf("showErrors=%v but stderr = %q", showErrors, stderr.String())
				}
			})
		}
	}
}

func TestRunWritesOutputOnce(t *testing.T) {
	t.Parallel()

	payload := `{"session_id":"s1","transcript_path":"t.jsonl","cwd":"/w","hook_event_name":"PreToolUse",` +
		`"source":"compact","agent_type":"worker","tool_name":"Bash","tool_input":{"command":"ls"},"extra":1}`
	var got Input
	var stdout countingWriter
	fn := func(in Input) ([]byte, error) {
		got = in
		return Deny("no <secrets> & more")
	}
	code := Run(strings.NewReader(payload), &stdout, io.Discard, false, fn)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if stdout.writes != 1 {
		t.Fatalf("writes = %d, want 1", stdout.writes)
	}
	want := Input{
		SessionID: "s1", TranscriptPath: "t.jsonl", CWD: "/w", HookEventName: "PreToolUse",
		Source: "compact", AgentType: "worker", ToolName: "Bash",
	}
	if string(got.ToolInput) != `{"command":"ls"}` {
		t.Fatalf("tool_input = %s", got.ToolInput)
	}
	got.ToolInput = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %+v, want %+v", got, want)
	}
	wantOut := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",` +
		`"permissionDecisionReason":"no <secrets> & more"}}` + "\n"
	if stdout.buf.String() != wantOut {
		t.Fatalf("stdout = %q, want %q", stdout.buf.String(), wantOut)
	}
}

func TestRunEmptyOutput(t *testing.T) {
	t.Parallel()

	var stdout countingWriter
	code := Run(strings.NewReader(`{}`), &stdout, io.Discard, true, func(Input) ([]byte, error) {
		return nil, nil
	})
	if code != 0 || stdout.writes != 0 {
		t.Fatalf("exit = %d, writes = %d; want 0, 0", code, stdout.writes)
	}
}

func TestContext(t *testing.T) {
	t.Parallel()

	out, err := Context("line 1\nline 2")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]string
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	h := decoded["hookSpecificOutput"]
	if len(h) != 2 || h["hookEventName"] != "SessionStart" ||
		h["additionalContext"] != "line 1\nline 2" {
		t.Fatalf("output = %s", out)
	}
}

// TestRunLargeInput guards against a size cap: a valid payload padded past
// 8 MiB must still reach the decision.
func TestRunLargeInput(t *testing.T) {
	t.Parallel()

	payload := `{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"a.txt"}}` +
		strings.Repeat(
			" ",
			9<<20,
		)
	var stdout bytes.Buffer
	code := Run(
		strings.NewReader(payload),
		&stdout,
		io.Discard,
		true,
		func(in Input) ([]byte, error) {
			if in.ToolName != "Read" {
				return nil, errors.New("tool_name not decoded")
			}
			return Deny("denied")
		},
	)
	if code != 0 || !strings.Contains(stdout.String(), `"permissionDecision":"deny"`) {
		t.Fatalf("exit = %d, stdout = %q; want the deny", code, stdout.String())
	}
}

func TestDebug(t *testing.T) {
	t.Parallel()

	for v, want := range map[string]bool{"1": true, "": false, "0": false, "true": false} {
		if got := Debug(func(string) string { return v }); got != want {
			t.Errorf("Debug(%q) = %v, want %v", v, got, want)
		}
	}
}

type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	_, _ = w.buf.Write(p)
	return len(p), nil
}
