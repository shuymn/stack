package observe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

//nolint:gosec // G101: hook output fixtures, no credentials.
const (
	denyJSONSecrets = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",` +
		`"permissionDecisionReason":"stack guard (secrets): Read targets the secret \"x\"."}}` + "\n"
	denyJSONGuard = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",` +
		`"permissionDecisionReason":"stack-guard (secrets): the command mentions the secret \"x\"."}}` + "\n"
	denyJSONRawMerge = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",` +
		`"permissionDecisionReason":"stack guard (raw-merge): gh pr merge is not run directly."}}` + "\n"
	guardDenySecrets    = "PreToolUse:Bash hook error: stack guard (secrets): the command mentions the secret \"x\"."
	guardDenySecretsV11 = "PreToolUse:Bash hook error: stack-guard (secrets): the command mentions the secret \"x\"."
	guardDenyRawMerge   = "PreToolUse:Bash hook error: stack guard (raw-merge): gh pr merge is not run directly."
)

func TestRecordedDecision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	path := f.fromTestdata("decisions.jsonl", ".claude/projects/-p/d.jsonl", f.cwd)
	s := f.state(t.Context())
	if err := s.scanTranscript(path, false, &Source{}); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(s.calls))
	for i, c := range s.calls {
		got[i] = c.tool + " " + c.recorded
	}
	want := []string{
		"Read deny:secrets", // stack guard deny of a Read
		"Bash deny:secrets", // stack guard deny of a Bash call
		"Bash other-deny",   // legacy hook deny
		"Bash other-deny",   // errors of several hooks
		"Bash unknown",      // user permission refusal
		"Bash unknown",      // tool_use with no result
		"Bash no-deny",      // a normal result
	}
	if !slices.Equal(got, want) {
		t.Fatalf("recorded = %q\nwant %q", got, want)
	}

	for _, tc := range []struct{ tool, text, want string }{
		{"Bash", "PreToolUse:Bash hook error: [stack hook claude pre-tool-use]: stack guard (rmroot): rm", "deny:rmroot"},
		// stack-guard's reasons (v11) name the same rules as the core hook's did (v10).
		{"Bash", "PreToolUse:Bash hook error: stack-guard (secrets): x", "deny:secrets"},
		{"Read", "PreToolUse:Read hook error: [stack-guard hook claude pre-tool-use]: stack-guard (secrets): x", "deny:secrets"},
		{"Bash", "PreToolUse:Bash hook error: stack-guard (Secrets): x", decOtherDeny},
		{"Bash", "PreToolUse:Bash hook error: stack-guard secrets", decOtherDeny},
		// Not a rule id: never copied into a finding.
		{"Bash", "PreToolUse:Bash hook error: stack guard (" + probePath + "): x", decOtherDeny},
		{"Bash", "PreToolUse:Bash hook error: stack guard (Secrets): x", decOtherDeny},
		{"Read", "PreToolUse:Bash hook error: stack guard (secrets): x", decOtherDeny}, // another tool's prefix
		{"Bash", "PreToolUse:Bash hook error: stack guard secrets", decOtherDeny},
		{"Bash", "Exit code 1\nPreToolUse:Bash hook error: stack guard (secrets): x", decNoDeny},
		{"Bash", "", decNoDeny},
	} {
		if got := recordedDecision(tc.tool, tc.text, record{}); got != tc.want {
			t.Errorf("recordedDecision(%q, %q) = %q, want %q", tc.tool, tc.text, got, tc.want)
		}
	}
	for _, r := range []record{
		{ToolDenialKind: "user-rejected"},
		{PermissionDecision: &struct {
			Source string `json:"source"`
		}{Source: "user_reject"}},
	} {
		if got := recordedDecision(
			"Bash",
			"Permission to use Bash has been denied.",
			r,
		); got != decUnknown {
			t.Errorf("refusal %+v = %q", r, got)
		}
	}
}

// bashCall is an in-window Bash call in cwd with its recorded result; result "" leaves it unanswered.
func bashCall(at time.Time, cwd string, input map[string]any, resultText string) []map[string]any {
	a := assistant(at, cwd, tool{"Bash", input})
	if resultText == "" {
		return []map[string]any{a}
	}
	return []map[string]any{a, result(a, resultText)}
}

func cmd(c string) map[string]any { return map[string]any{"command": c} }

func (f *fixture) calls(name string, recs ...[]map[string]any) {
	f.t.Helper()
	var all []map[string]any
	for _, r := range recs {
		all = append(all, r...)
	}
	f.transcript(name, all...)
}

func replayFindings(resp Response) []string {
	fds := findings(resp, kindReplayChange)
	out := make([]string, 0, len(fds))
	for _, fd := range fds {
		out = append(out, fd.Detail)
	}
	slices.Sort(out)
	return out
}

func TestReplay(t *testing.T) {
	t.Parallel()
	t.Run("changes, duplicates and unrecoverable", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		at := hourAgo
		edit := assistant(at, f.cwd, tool{"Edit", map[string]any{"file_path": "x"}})
		f.calls(
			"r",
			bashCall(at, f.cwd, cmd("a"), "ok"),
			bashCall(
				at.Add(time.Second),
				f.cwd,
				cmd("a"),
				"ok",
			), // same payload: run once, compared twice
			bashCall(at, f.cwd, cmd("b"), guardDenySecrets),
			bashCall(at, f.cwd, cmd("c"), guardDenyRawMerge),
			bashCall(
				at,
				f.cwd,
				cmd("d"),
				"PreToolUse:Bash hook error: Use trash",
			), // other-deny: not compared
			bashCall(
				at,
				filepath.Join(f.cwd, "gone"),
				cmd("e"),
				"ok",
			), // cwd gone: unrecoverable
			bashCall(at, f.cwd, map[string]any{"description": "no command"}, "ok"), // unrecoverable
			bashCall(at, f.cwd, map[string]any{"command": 7}, "ok"),                // unrecoverable
			[]map[string]any{
				edit,
				result(edit, "ok"),
			}, // not guard-wired
		)
		stack := f.fake("stack", stackModel, proctest.Response{
			Args: []string{"hook", "claude", "pre-tool-use"}, Stdout: denyJSONSecrets,
		})
		resp, l := f.run()
		want := ReplayTotals{
			Compared: 4, Changed: 3, Unrecoverable: 3,
			ChangedByKind: ChangeKinds{NewDeny: 2, RuleChanged: 1},
		}
		if l.Replay != want {
			t.Errorf("replay = %+v, want %+v", l.Replay, want)
		}
		wantF := []string{
			"Bash: deny:raw-merge -> deny:secrets", "Bash: no-deny -> deny:secrets",
			"Bash: no-deny -> deny:secrets",
		}
		if got := replayFindings(resp); !slices.Equal(got, wantF) {
			t.Errorf("findings = %q", got)
		}
		var hooks []proctest.Call
		for _, c := range proctest.Calls(t, stack) {
			if slices.Equal(c.Args, []string{"hook", "claude", "pre-tool-use"}) {
				hooks = append(hooks, c)
			}
		}
		if len(hooks) != 3 {
			t.Fatalf("hook runs = %d, want 3 (a once, b, c)", len(hooks))
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(hooks[0].Stdin), &payload); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(payload))
		for k := range payload {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{
			"cwd", "hook_event_name", "session_id", "tool_input", "tool_name",
			"transcript_path",
		}) || payload["hook_event_name"] != "PreToolUse" || payload["cwd"] != f.cwd {
			t.Errorf("payload = %v", payload)
		}
		for _, fd := range findings(resp, kindReplayChange) {
			if !strings.Contains(fd.Where, "r.jsonl:") {
				t.Errorf("where = %q, want transcript:line", fd.Where)
			}
		}
		if resp.Status != StatusWarn || l.Sources["replay"].Status != srcOK {
			t.Errorf("status %q replay %+v", resp.Status, l.Sources["replay"])
		}
	})
	t.Run("lost deny", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.calls(
			"r",
			bashCall(hourAgo, f.cwd, cmd("b"), guardDenySecrets),
			bashCall(hourAgo, f.cwd, cmd("n"), "ok"),
		)
		f.fake(
			"stack",
			stackModel,
			proctest.Response{Args: []string{"hook", "claude", "pre-tool-use"}},
		)
		resp, l := f.run()
		if got := replayFindings(
			resp,
		); !slices.Equal(
			got,
			[]string{"Bash: deny:secrets -> no-deny"},
		) {
			t.Errorf("findings = %q", got)
		}
		if l.Replay != (ReplayTotals{Compared: 2, Changed: 1, ChangedByKind: ChangeKinds{LostDeny: 1}}) {
			t.Errorf("replay = %+v", l.Replay)
		}
	})
	notComparable := map[string]proctest.Response{
		"non-zero exit": {Exit: 1, Stdout: denyJSONSecrets},
		"bad json":      {Stdout: "{not json"},
		"other deny": {
			Stdout: strings.Replace(denyJSONSecrets, "stack guard (secrets)", "other", 1),
		},
		"failed open": {Stderr: "stack hook: decode hook input: boom\n"},
		// The hook's diagnostic lies past the stderr cap: what was kept cannot say it is absent.
		"stderr past its cap": {
			Stderr: strings.Repeat("x", maxStderr) + "\nstack hook: decode hook input: boom\n",
		},
		"rule id not a rule": {
			Stdout: strings.Replace(denyJSONSecrets, "(secrets)", "("+probePath+")", 1),
		},
		"timeout": {Sleep: 10 * time.Second},
	}
	for name, r := range notComparable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.opts.replayTimeout = 500 * time.Millisecond
			f.calls("r", bashCall(hourAgo, f.cwd, cmd("b"), guardDenySecrets))
			r.Args = []string{"hook", "claude", "pre-tool-use"}
			f.fake("stack", stackModel, r)
			resp, l := f.run()
			if l.Replay != (ReplayTotals{NotComparable: 1}) ||
				len(findings(resp, kindReplayChange)) != 0 {
				t.Errorf("replay = %+v, findings %+v", l.Replay, resp.Findings)
			}
		})
	}
	t.Run("start failure", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.calls("r", bashCall(hourAgo, f.cwd, cmd("b"), guardDenySecrets))
		f.tools["stack"] = f.write("not-a-program", "plain text")
		_, l := f.run()
		if l.Replay != (ReplayTotals{NotComparable: 1}) {
			t.Errorf("replay = %+v", l.Replay)
		}
	})
	t.Run("stack missing: off", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.calls("r", bashCall(hourAgo, f.cwd, cmd("b"), guardDenySecrets))
		_, l := f.run()
		if l.Sources["replay"].Status != srcOff || l.Replay != (ReplayTotals{}) {
			t.Errorf("replay %+v %+v", l.Sources["replay"], l.Replay)
		}
	})
}

// TestReplayHooks covers the replay through both installed hooks (core-stages.md §4.5, §4.10).
func TestReplayHooks(t *testing.T) {
	t.Parallel()
	t.Run("both hooks: v10 and v11 reasons name the same rule", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.calls(
			"r",
			// v10: the core hook made the secrets deny; v11 stack-guard makes it.
			bashCall(hourAgo, f.cwd, cmd("b"), guardDenySecrets),
			bashCall(hourAgo, f.cwd, cmd("g"), guardDenySecretsV11),
		)
		f.fake("stack-guard", proctest.Response{Args: hookArgs, Stdout: denyJSONGuard})
		f.fake("stack", stackModel, proctest.Response{Args: hookArgs})
		resp, l := f.run()
		if l.Replay != (ReplayTotals{Compared: 2}) || len(findings(resp, kindReplayChange)) != 0 {
			t.Errorf("replay = %+v, findings %+v", l.Replay, resp.Findings)
		}
	})
	t.Run("both hooks: the core deny decides first", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.calls("r", bashCall(hourAgo, f.cwd, cmd("m"), guardDenyRawMerge))
		guard := f.fake("stack-guard", proctest.Response{Args: hookArgs, Stdout: denyJSONGuard})
		f.fake("stack", stackModel, proctest.Response{Args: hookArgs, Stdout: denyJSONRawMerge})
		resp, l := f.run()
		if l.Replay != (ReplayTotals{Compared: 1}) || len(findings(resp, kindReplayChange)) != 0 {
			t.Errorf("replay = %+v, findings %+v", l.Replay, resp.Findings)
		}
		if calls := proctest.Calls(t, guard); len(calls) != 0 {
			t.Errorf("stack-guard ran %d times after the core deny", len(calls))
		}
	})
	t.Run("stack-guard not comparable", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.calls("r", bashCall(hourAgo, f.cwd, cmd("n"), "ok"))
		f.fake("stack-guard", proctest.Response{Args: hookArgs, Exit: 1})
		f.fake("stack", stackModel, proctest.Response{Args: hookArgs})
		_, l := f.run()
		if l.Replay != (ReplayTotals{NotComparable: 1}) {
			t.Errorf("replay = %+v", l.Replay)
		}
	})
	t.Run(
		"stack-guard missing: warn, a deny stack no longer makes not comparable",
		func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.calls(
				"r",
				bashCall(hourAgo, f.cwd, cmd("b"), guardDenySecrets),
				bashCall(hourAgo, f.cwd, cmd("n"), "ok"),
			)
			f.fake("stack", stackModel, proctest.Response{Args: hookArgs})
			delete(f.tools, "stack-guard")
			resp, l := f.run()
			if l.Replay != (ReplayTotals{Compared: 1, NotComparable: 1}) ||
				l.Sources["replay"].Status != srcOK || len(findings(resp, kindReplayChange)) != 0 {
				t.Errorf(
					"replay = %+v %+v, findings %+v",
					l.Replay,
					l.Sources["replay"],
					resp.Findings,
				)
			}
			if got := findings(resp, kindReplayHookMissing); len(got) != 1 ||
				got[0].Where != "stack-guard" || resp.Status != StatusWarn {
				t.Errorf("status %q, hook-missing findings %+v", resp.Status, got)
			}
		},
	)
}

// TestReplayInputs covers what decides whether a record is replayed and in which order.
func TestReplayInputs(t *testing.T) {
	t.Parallel()
	t.Run("input the guard cannot judge: unrecoverable, not run", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		denied := "PreToolUse:%s hook error: stack guard (secrets): x"
		tools := []tool{
			{"Read", map[string]any{}},
			{"Read", map[string]any{"file_path": 7}},
			{"NotebookRead", map[string]any{"cell_id": "c"}},
			{"Grep", map[string]any{"pattern": "x", "path": 7}},
			{"Glob", map[string]any{"path": "."}},
			{"mcp__pencil__execute", map[string]any{"input": 7}},
		}
		recs := make([][]map[string]any, 0, len(tools))
		for _, tl := range tools {
			a := assistant(hourAgo, f.cwd, tl)
			recs = append(recs, []map[string]any{a, result(a, fmt.Sprintf(denied, tl.name))})
		}
		f.calls("r", recs...)
		stack := f.fake("stack", stackModel, proctest.Response{
			Args: []string{"hook", "claude", "pre-tool-use"},
		})
		resp, l := f.run()
		if l.Replay != (ReplayTotals{Unrecoverable: 6}) ||
			len(findings(resp, kindReplayChange)) != 0 {
			t.Errorf("replay = %+v, findings %+v", l.Replay, resp.Findings)
		}
		for _, c := range proctest.Calls(t, stack) {
			if c.Stdin != "" {
				t.Errorf("the hook ran for %s", c.Stdin)
			}
		}
	})
	t.Run("missing or null result: unknown, not compared", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		contents := []any{nil, noContent, map[string]any{"text": "x"}}
		recs := make([][]map[string]any, 0, len(contents))
		for _, content := range contents {
			a := assistant(hourAgo, f.cwd, tool{"Bash", cmd("b")})
			recs = append(recs, []map[string]any{a, result(a, content)})
		}
		f.calls("r", recs...)
		f.fake("stack", stackModel, proctest.Response{
			Args: []string{"hook", "claude", "pre-tool-use"}, Stdout: denyJSONSecrets,
		})
		resp, l := f.run()
		if l.Replay != (ReplayTotals{}) || len(findings(resp, kindReplayChange)) != 0 {
			t.Errorf("replay = %+v, findings %+v", l.Replay, resp.Findings)
		}
	})
	t.Run("cap", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.opts.replayCap = 1
		f.calls("r", bashCall(hourAgo, f.cwd, cmd("older"), "ok"),
			bashCall(hourAgo.Add(time.Minute), f.cwd, cmd("newer"), "ok"))
		stack := f.fake(
			"stack",
			stackModel,
			proctest.Response{Args: []string{"hook", "claude", "pre-tool-use"}},
		)
		_, l := f.run()
		if l.Replay != (ReplayTotals{Compared: 1, TruncatedCap: 1}) {
			t.Errorf("replay = %+v", l.Replay)
		}
		var stdin []string
		for _, c := range proctest.Calls(t, stack) {
			if c.Stdin != "" {
				stdin = append(stdin, c.Stdin)
			}
		}
		if len(stdin) != 1 || !strings.Contains(stdin[0], `"newer"`) {
			t.Errorf("replayed %q, want only the newest call", stdin)
		}
	})
	t.Run("cap keeps the newest by full timestamp", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.opts.replayCap = 1
		f.calls("r", bashCall(hourAgo.Add(100*time.Millisecond), f.cwd, cmd("older"), "ok"),
			bashCall(hourAgo.Add(900*time.Millisecond), f.cwd, cmd("newer"), "ok"))
		stack := f.fake(
			"stack",
			stackModel,
			proctest.Response{Args: []string{"hook", "claude", "pre-tool-use"}},
		)
		f.run()
		var stdin []string
		for _, c := range proctest.Calls(t, stack) {
			if c.Stdin != "" {
				stdin = append(stdin, c.Stdin)
			}
		}
		if len(stdin) != 1 || !strings.Contains(stdin[0], `"newer"`) {
			t.Errorf("replayed %q, want only the newest call", stdin)
		}
	})
}

// TestReplayRealHook replays real-format records through the stack and stack-guard built in
// TestMain (phase3-cycle.md P3 (a)).
func TestReplayRealHook(t *testing.T) {
	t.Parallel()
	needBuilt(t)
	// A recorded no-deny the current rules deny, with a local path in its input.
	local := "/home/" + "someone" + "/run.log"
	setup := func(t *testing.T) *fixture {
		t.Helper()
		f := newFixture(t)
		f.tools["stack"] = built.stack
		f.tools["stack-guard"] = built.guard
		f.fromTestdata("decisions.jsonl", ".claude/projects/-p/d.jsonl", f.cwd)
		f.calls(
			"new",
			bashCall(hourAgo, f.cwd, cmd(`gh pr merge 1 --body "log at `+local+`"`), "merged"),
		)
		return f
	}
	check := func(t *testing.T, f *fixture, resp Response, want ReplayTotals, l Latest) {
		t.Helper()
		if l.Replay != want {
			t.Errorf("replay = %+v, findings %+v", l.Replay, resp.Findings)
		}
		if got := replayFindings(
			resp,
		); !slices.Equal(
			got,
			[]string{"Bash: no-deny -> deny:raw-merge"},
		) {
			t.Fatalf("findings = %q", got)
		}
		data := mustJSON(t, resp)
		if strings.Contains(string(data), "someone") {
			t.Errorf("the response quotes the input path: %s", data)
		}
		latest, err := os.ReadFile(latestPath(f.home))
		if err != nil || strings.Contains(string(latest), "someone") {
			t.Errorf("latest.json quotes the input path (err %v)", err)
		}
	}
	t.Run("both hooks", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		resp, l := f.run()
		// Compared: the two recorded v10 `stack guard (secrets)` denies, which stack-guard makes
		// now (unchanged), the no-deny ls (unchanged) and the gh call (raw-merge, new).
		check(
			t,
			f,
			resp,
			ReplayTotals{Compared: 4, Changed: 1, ChangedByKind: ChangeKinds{NewDeny: 1}},
			l,
		)
		if got := findings(resp, kindReplayHookMissing); len(got) != 0 {
			t.Errorf("hook-missing findings %+v", got)
		}
	})
	t.Run("stack-guard missing", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		delete(f.tools, "stack-guard")
		resp, l := f.run()
		// The recorded secrets denies are not comparable, not lost.
		check(t, f, resp, ReplayTotals{
			Compared: 2, Changed: 1, NotComparable: 2, ChangedByKind: ChangeKinds{NewDeny: 1},
		}, l)
		if got := findings(resp, kindReplayHookMissing); len(got) != 1 {
			t.Errorf("hook-missing findings %+v", resp.Findings)
		}
	})
}
