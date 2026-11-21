package merge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// checksFixture installs a fake `stack-<name>` per response in a dir of its own; its message
// checks look names up there only.
type checksFixture struct {
	bin   string
	fakes map[string]string
}

func newChecks(t *testing.T, fakes map[string]proctest.Response) *checksFixture {
	t.Helper()
	f := &checksFixture{bin: t.TempDir(), fakes: map[string]string{}}
	for name, resp := range fakes {
		resp.LogEnv = []string{"STACK_STAGE"}
		f.fakes[name] = proctest.InstallAt(t, f.bin, ext.Prefix+name,
			proctest.Scenario{Responses: []proctest.Response{resp}})
	}
	return f
}

func (f *checksFixture) checks(names []string, timeout time.Duration) MessageChecks {
	return MessageChecks{
		Names: names,
		Ext:   ext.Options{Runner: proc.Exec{}},
		resolve: func(names []string) []ext.Plan {
			plans := make([]ext.Plan, 0, len(names))
			for _, n := range names {
				path, err := proc.ResolveExecutable(f.bin, ext.Prefix+n)
				plans = append(plans, ext.Plan{Name: n, Path: path, Err: err})
			}
			return plans
		},
		timeout: timeout,
	}
}

func (f *checksFixture) calls(t *testing.T, name string) []proctest.Call {
	t.Helper()
	return proctest.Calls(t, f.fakes[name])
}

// TestOutboundMessage is C19 in the merge package: with --subject or --body and a non-empty
// [merge].message_checks, the listed extensions run as one outbound stage run before the first gh
// call, and the merge is refused unless each answered without a block or an error.
func TestOutboundMessage(t *testing.T) {
	t.Parallel()
	fakes := messageFakes()
	body := func(names []string, b *string) func(*checksFixture) func(*Options) {
		return func(f *checksFixture) func(*Options) {
			return func(o *Options) {
				o.Merge.Body = b
				o.MessageChecks = f.checks(names, time.Minute)
			}
		}
	}
	notRunnable := func(t *testing.T, f *checksFixture) {
		t.Helper()
		odd := filepath.Join(f.bin, ext.Prefix+"odd")
		if runtime.GOOS == "windows" {
			odd += ".cmd"
		}
		if err := os.WriteFile(odd, []byte("not a program\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		opts  func(*checksFixture) func(*Options)
		setup func(*testing.T, *checksFixture)
		// lines are the refusal's stderr lines after `stack pr merge: `; nil means merged.
		lines  []string
		stderr string
		ran    []string
	}{
		{name: "no message flag", opts: body([]string{"gate"}, nil)},
		{name: "empty list", opts: body([]string{}, new("see x"))},
		{name: "clean", opts: body([]string{"clean"}, new("ok")), ran: []string{"clean"}},
		{
			name: "valid warn continues", opts: body([]string{"note", "clean"}, new("ok")),
			stderr: "[extension note: warn: warn]\n", ran: []string{"note", "clean"},
		},
		{
			name: "block", opts: body([]string{"gate", "clean"}, new("")),
			lines: []string{"outbound blocked: gate: check public: --body: hit"},
			ran:   []string{"gate", "clean"},
		},
		{
			name: "missing", opts: body([]string{"absent"}, new("ok")),
			lines: []string{"outbound could not check the message: absent: not installed " +
				"(stack-absent not found on PATH)"},
		},
		{
			name: "not runnable", opts: body([]string{"odd"}, new("ok")), setup: notRunnable,
			lines: []string{"outbound could not check the message: odd: "},
		},
		{
			name: "invalid output", opts: body([]string{"bad"}, new("ok")),
			lines: []string{"outbound could not check the message: bad: invalid response: "},
			ran:   []string{"bad"},
		},
		{
			name: "block and missing", opts: body([]string{"absent", "note", "gate"}, new("ok")),
			lines: []string{
				"outbound could not check the message: absent: not installed " +
					"(stack-absent not found on PATH)",
				"outbound blocked: gate: check public: --body: hit",
			},
			stderr: "[extension note: warn: warn]\n", ran: []string{"note", "gate"},
		},
		{
			name: "subject only",
			opts: func(f *checksFixture) func(*Options) {
				return func(o *Options) {
					o.Merge.Subject = new("feat: x")
					o.MessageChecks = f.checks([]string{"gate"}, time.Minute)
				}
			},
			lines: []string{"outbound blocked: gate: check public: --body: hit"},
			ran:   []string{"gate"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newChecks(t, fakes)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			out := run(t, okResponses(), setup{fakeGit: true, opts: tt.opts(f)})
			for name := range fakes {
				if n, want := len(
					f.calls(t, name),
				), slices.Contains(
					tt.ran,
					name,
				); (n == 1) != want ||
					n > 1 {
					t.Errorf("%s ran %d times, want it run: %v", name, n, want)
				}
			}
			assertMessageOutcome(t, out, tt.lines, tt.stderr)
		})
	}
}

// messageFakes are the message checks of the tests, by name.
func messageFakes() map[string]proctest.Response {
	return map[string]proctest.Response{
		"clean": {Stdout: `{"status":"pass"}`},
		"note":  {Stdout: `{"status":"warn","context":"maybe"}`},
		"gate":  {Stdout: `{"status":"block","context":"  check public: --body: hit\n"}`},
		"bad":   {Stdout: `not json`},
		"hang":  {Stdout: `{"status":"pass"}`, Sleep: time.Minute},
	}
}

// assertMessageOutcome checks a run against the refusal lines after `stack pr merge: ` (each a
// prefix; nil means merged) and the stderr written before gh.
func assertMessageOutcome(t *testing.T, out outcome, lines []string, stderr string) {
	t.Helper()
	if lines == nil {
		stderr += "merged\n" // gh pr merge's output
	}
	if out.stderr != stderr {
		t.Errorf("stderr = %q, want %q", out.stderr, stderr)
	}
	if lines == nil {
		if out.err != nil || !out.merged() {
			t.Errorf("err = %v, merged %v; want merged", out.err, out.merged())
		}
		return
	}
	if !errors.Is(out.err, ErrRefused) || errors.Is(out.err, ErrUnknown) {
		t.Fatalf("err = %v, want a refusal", out.err)
	}
	got := strings.Split(out.err.Error(), "\nstack pr merge: ")
	if len(got) != len(lines) {
		t.Fatalf("lines = %q, want %q", got, lines)
	}
	for i, want := range lines {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("line %d = %q, want prefix %q", i, got[i], want)
		}
	}
	if len(out.gh) != 0 {
		t.Errorf("gh calls = %+v, want none", out.gh)
	}
}

// TestOutboundMessageRequest: the message checks get the outbound request of §4.1 with repo the
// top level, after the one git call that found it.
func TestOutboundMessageRequest(t *testing.T) {
	t.Parallel()
	f := newChecks(t, messageFakes())
	out := run(t, okResponses(), setup{fakeGit: true, opts: func(o *Options) {
		o.Merge.Subject, o.Merge.Body = new("feat: x (#7)"), new("")
		o.MessageChecks = f.checks([]string{"clean"}, time.Minute)
	}})
	if out.err != nil || !out.merged() {
		t.Fatalf("err = %v, merged %v", out.err, out.merged())
	}
	calls := f.calls(t, "clean")
	var req map[string]json.RawMessage
	if len(calls) != 1 || json.Unmarshal([]byte(calls[0].Stdin), &req) != nil {
		t.Fatalf("calls = %+v", calls)
	}
	want := map[string]string{
		"stage": `"outbound"`, "repo": strconv.Quote(filepath.ToSlash(out.dir)),
		"range": `""`, "prompt": `""`, "title": `"feat: x (#7)"`, "body": `""`, "branch": `""`,
	}
	for k, v := range want {
		if string(req[k]) != v {
			t.Errorf("request %s = %s, want %s", k, req[k], v)
		}
	}
	if len(req) != len(want) {
		t.Errorf("request = %s, want only %v", calls[0].Stdin, want)
	}
	if calls[0].Env["STACK_STAGE"] != "outbound" {
		t.Errorf("extension env = %v", calls[0].Env)
	}
	// The checks run after the top level is found: git first, then the checks, then gh.
	if len(out.git) != 1 {
		t.Errorf("git calls = %+v, want the one top-level lookup", out.git)
	}
}

// TestOutboundMessageDeadline: a check that hangs past the stage deadline refuses the merge before
// gh, and the next check is left unstarted.
func TestOutboundMessageDeadline(t *testing.T) {
	t.Parallel()
	f := newChecks(t, messageFakes())
	start := time.Now()
	out := run(t, okResponses(), setup{fakeGit: true, opts: func(o *Options) {
		o.Merge.Body = new("ok")
		o.MessageChecks = f.checks([]string{"hang", "clean"}, 300*time.Millisecond)
	}})
	if !errors.Is(out.err, ErrRefused) || len(out.gh) != 0 ||
		!strings.Contains(out.err.Error(),
			"outbound could not check the message: hang: timed out") ||
		!strings.Contains(out.err.Error(),
			"outbound could not check the message: clean: not started") {
		t.Fatalf("err = %v, gh calls %+v", out.err, out.gh)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("took %s", d)
	}
}
