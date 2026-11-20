// Package hookio reads Claude Code hook payloads and writes hook responses.
//
// Hooks fail open: FailOpen turns every decode failure, error and panic into
// "no stdout, exit 0", so a broken hook never breaks a session. Only an
// explicit deny or a session context writes hook JSON.
package hookio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
)

// debugEnv names the variable that, when exactly "1", sends swallowed hook
// errors to stderr.
const debugEnv = "STACK_DEBUG"

var (
	// errNotObject reports a payload that is valid JSON but not an object.
	errNotObject = errors.New("hook input is not a JSON object")
	// errPanic reports a recovered panic in a hook function.
	errPanic = errors.New("hook panicked")
)

// Input holds the hook payload fields core reads. ToolInput stays raw and is
// decoded per tool by the caller.
//
//nolint:tagliatelle // Claude Code defines the payload keys in snake_case.
type Input struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	Source         string          `json:"source"`
	AgentType      string          `json:"agent_type"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// Decode reads one JSON object from r. There is no size cap: a large but
// valid payload must still be judged.
func Decode(r io.Reader) (Input, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Input{}, fmt.Errorf("read hook input: %w", err)
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Input{}, errNotObject
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return Input{}, fmt.Errorf("decode hook input: %w", err)
	}
	return in, nil
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

type output struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

// Deny encodes a PreToolUse deny with reason, newline-terminated.
func Deny(reason string) ([]byte, error) {
	return encode(hookSpecificOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	})
}

// Context encodes a SessionStart additionalContext, newline-terminated.
func Context(text string) ([]byte, error) {
	return encode(hookSpecificOutput{
		HookEventName:     "SessionStart",
		AdditionalContext: text,
	})
}

func encode(h hookSpecificOutput) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(output{HookSpecificOutput: h}); err != nil {
		return nil, fmt.Errorf("encode hook output: %w", err)
	}
	return buf.Bytes(), nil
}

// Debug reports whether getenv enables debug output for swallowed errors.
func Debug(getenv func(string) string) bool {
	return getenv(debugEnv) == "1"
}

// FailOpen runs fn and writes its output to stdout in one write, only when fn
// returns no error. An error or a panic in fn writes nothing to stdout; it is
// reported on stderr only when showErrors is set. FailOpen always returns exit
// code 0.
func FailOpen(stdout, stderr io.Writer, showErrors bool, fn func() ([]byte, error)) int {
	out, err := call(fn)
	if err != nil {
		if showErrors {
			_, _ = fmt.Fprintf(stderr, "stack hook: %v\n", err)
		}
		return 0
	}
	if len(out) > 0 {
		_, _ = stdout.Write(out)
	}
	return 0
}

// Run decodes the hook payload from stdin and passes it to fn under FailOpen.
func Run(
	stdin io.Reader,
	stdout, stderr io.Writer,
	showErrors bool,
	fn func(Input) ([]byte, error),
) int {
	return FailOpen(stdout, stderr, showErrors, func() ([]byte, error) {
		in, err := Decode(stdin)
		if err != nil {
			return nil, err
		}
		return fn(in)
	})
}

// call runs fn, turning a panic into an error. FailOpen drops the output on
// every error, so a panic needs no separate output handling.
//
//nolint:nonamedreturns // recover can set the result only through named returns.
func call(fn func() ([]byte, error)) (out []byte, err error) {
	// Stays set when fn panics and recover returns nil (GODEBUG panicnil=1).
	err = errPanic
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%w: %v\n%s", errPanic, r, debug.Stack())
		}
	}()
	return fn()
}
