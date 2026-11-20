package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

const request = `{"stage":"outbound","repo":"","range":"","prompt":""}`

// install puts a fake `stack-<name>` answering resp into a new dir and returns its runnable plan.
func install(t *testing.T, name string, resp proctest.Response) Plan {
	t.Helper()
	path := proctest.Install(t, Prefix+name,
		proctest.Scenario{Responses: []proctest.Response{resp}})
	return Plan{Name: name, Path: path}
}

func findings(t *testing.T, r Result) []map[string]string {
	t.Helper()
	var f []map[string]string
	if err := json.Unmarshal(r.Findings, &f); err != nil {
		t.Fatalf("findings %s: %v", r.Findings, err)
	}
	return f
}

// TestRunResponses covers C21's per-response evaluation: JSON and exit per case.
func TestRunResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		resp    proctest.Response
		timeout time.Duration
		max     int64
		want    string
		context string
		reason  string // substring of the synthetic finding's error; empty for a real response
	}{
		{
			name:    "pass with context",
			resp:    proctest.Response{Stdout: `{"status":"pass","context":"CTX","findings":[]}`},
			want:    StatusPass,
			context: "CTX",
		},
		{
			name: "block",
			resp: proctest.Response{Stdout: `{"status":"block","findings":[{"rule":"x"}]}` + "\n"},
			want: StatusBlock,
		},
		{
			name: "warn response",
			resp: proctest.Response{Stdout: `{"status":"warn"}`},
			want: StatusWarn,
		},
		{
			name: "non-zero exit with a valid response keeps its status",
			resp: proctest.Response{Stdout: `{"status":"block"}`, Exit: 3},
			want: StatusBlock,
		},
		{
			name:   "non-JSON",
			resp:   proctest.Response{Stdout: "hello"},
			want:   StatusWarn,
			reason: "invalid response",
		},
		{
			name:   "trailing garbage",
			resp:   proctest.Response{Stdout: `{"status":"pass"} x`},
			want:   StatusWarn,
			reason: "trailing data",
		},
		{
			name:   "second object",
			resp:   proctest.Response{Stdout: `{"status":"pass"}{"status":"block"}`},
			want:   StatusWarn,
			reason: "trailing data",
		},
		{
			name:   "unknown status",
			resp:   proctest.Response{Stdout: `{"status":"ok"}`},
			want:   StatusWarn,
			reason: `unknown status "ok"`,
		},
		{
			name:   "findings not an array",
			resp:   proctest.Response{Stdout: `{"status":"pass","findings":{}}`},
			want:   StatusWarn,
			reason: "findings is not an array",
		},
		{
			name:   "crash without a response",
			resp:   proctest.Response{Stderr: "panic", Exit: 2},
			want:   StatusWarn,
			reason: "exit 2; invalid response: malformed: no output",
		},
		{
			name: "valid JSON then hang",
			resp: proctest.Response{
				Stdout: `{"status":"pass","context":"EARLY"}`,
				Sleep:  time.Minute,
			},
			timeout: 5 * time.Second,
			want:    StatusWarn,
			reason:  "timed out",
		},
		{
			name:   "truncated valid JSON",
			resp:   proctest.Response{Stdout: `{"status":"pass"}` + strings.Repeat(" ", 64)},
			max:    int64(len(`{"status":"pass"}`)),
			want:   StatusWarn,
			reason: "response size limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := install(t, "demo", tt.resp)
			o := Options{Runner: proc.Exec{}, Timeout: tt.timeout, MaxStdout: tt.max}
			agg := Run(context.Background(), o, StageOutbound, []byte(request), []Plan{p})
			if agg.Status != tt.want || len(agg.Results) != 1 {
				t.Fatalf("aggregate = %+v, want %s", agg, tt.want)
			}
			r := agg.Results[0]
			if r.Name != "demo" || r.Status != tt.want || r.Context != tt.context {
				t.Fatalf("result = %+v", r)
			}
			if tt.reason == "" {
				if r.Error != "" {
					t.Fatalf("error = %q, want none", r.Error)
				}
			} else {
				f := findings(t, r)
				if len(f) != 1 || f[0]["extension"] != "demo" ||
					!strings.Contains(f[0]["error"], tt.reason) ||
					!strings.Contains(r.Error, tt.reason) {
					t.Fatalf("synthetic finding = %s / %q, want %q", r.Findings, r.Error, tt.reason)
				}
			}
			if tt.timeout > 0 {
				return // a loaded machine may kill the fake before it logs; TestEvaluateOrder pins it
			}
			calls := proctest.Calls(t, p.Path)
			if len(calls) != 1 || calls[0].Stdin != request || len(calls[0].Args) != 0 {
				t.Fatalf("calls = %+v", calls)
			}
		})
	}
}

// TestEvaluateOrder covers C21's evaluation order: a timeout, a pipe overrun or truncation warns
// even when the stdout received so far is a valid response, and the exit code is ignored for a
// valid one.
func TestEvaluateOrder(t *testing.T) {
	t.Parallel()
	valid := []byte(`{"status":"block","context":"EARLY"}`)
	for _, tt := range []struct {
		res    proc.Result
		want   string
		reason string
	}{
		{proc.Result{Stdout: valid, TimedOut: true, ExitCode: -1}, StatusWarn, "timed out: stage deadline of 1s exceeded"},
		{proc.Result{Stdout: valid, WaitDelayExceeded: true}, StatusWarn, "stayed open"},
		{proc.Result{Stdout: valid, Truncated: true}, StatusWarn, "size limit"},
		{proc.Result{Stdout: valid, ExitCode: 9}, StatusBlock, ""},
	} {
		r := evaluate("demo", tt.res, nil, time.Second)
		if r.Status != tt.want || !strings.Contains(r.Error, tt.reason) ||
			(tt.reason != "" && r.Context != "") {
			t.Errorf("evaluate(%+v) = %+v, want %s %q", tt.res, r, tt.want, tt.reason)
		}
	}
	if r := evaluate("demo", proc.Result{}, proc.ErrStart, time.Second); r.Status != StatusWarn {
		t.Errorf("start error = %+v", r)
	}
}

// TestEvaluateStrictResponse covers C21's response shape: stdout must be UTF-8, context a string
// and findings an array; null is neither, and an absent field takes its empty value.
func TestEvaluateStrictResponse(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		stdout  string
		want    string
		context string
		reason  string
	}{
		{"{\"status\":\"pass\",\"context\":\"\xff\",\"findings\":[]}", StatusWarn, "", "not UTF-8"},
		{`{"status":"pass","context":null,"findings":[]}`, StatusWarn, "", "context is not a string"},
		{`{"status":"pass","context":1}`, StatusWarn, "", "context is not a string"},
		{`{"status":"pass","context":"C","findings":null}`, StatusWarn, "", "findings is not an array"},
		{`{"status":"pass","findings":"x"}`, StatusWarn, "", "findings is not an array"},
		{`{"status":"pass","context":123,"Context":"OK"}`, StatusWarn, "", "context is not a string"},
		{`{"status":"pass","context":null,"context":"OK"}`, StatusWarn, "", "context is not a string"},
		{`{"status":"pass","findings":null,"Findings":[]}`, StatusWarn, "", "findings is not an array"},
		{`{"status":"pass","context":"C\u00e9"}`, StatusPass, "C\u00e9", ""},
		{`{"status":"block"}`, StatusBlock, "", ""},
	} {
		r := evaluate("demo", proc.Result{Stdout: []byte(tt.stdout)}, nil, time.Second)
		if r.Status != tt.want || r.Context != tt.context || !strings.Contains(r.Error, tt.reason) {
			t.Errorf("evaluate(%q) = %+v, want %s %q", tt.stdout, r, tt.want, tt.reason)
			continue
		}
		if tt.reason != "" {
			if f := findings(t, r); len(f) != 1 || f[0]["extension"] != "demo" {
				t.Errorf("evaluate(%q) findings = %s", tt.stdout, r.Findings)
			}
		} else if string(r.Findings) != "[]" {
			t.Errorf("evaluate(%q) findings = %s, want []", tt.stdout, r.Findings)
		}
	}
}

// TestRunStageDeadline covers C21's one deadline per stage: two extensions that hang finish within
// one budget plus one cleanup grace period, and the second never starts.
func TestRunStageDeadline(t *testing.T) {
	t.Parallel()
	const budget, grace = 2 * time.Second, 500 * time.Millisecond
	hang := proctest.Response{Stdout: `{"status":"pass"}`, Sleep: time.Minute}
	first, second := install(t, "first", hang), install(t, "second", hang)
	o := Options{Runner: proc.Exec{WaitDelay: grace}, Timeout: budget}
	start := time.Now()
	agg := Run(context.Background(), o, StageCycle, []byte(request), []Plan{first, second})
	elapsed := time.Since(start)
	// Generous slack for loaded runners; the "not started" assertions below are what fail when
	// each extension gets its own deadline.
	if limit := budget + grace + 5*time.Second; elapsed >= limit {
		t.Fatalf("stage took %s, want under %s", elapsed, limit)
	}
	if agg.Status != StatusWarn || len(agg.Results) != 2 ||
		!strings.Contains(agg.Results[0].Error, "timed out") ||
		!strings.Contains(agg.Results[1].Error, "not started: stage deadline of 2s exceeded") {
		t.Fatalf("aggregate = %+v", agg)
	}
	if f := findings(t, agg.Results[1]); len(f) != 1 || f[0]["extension"] != "second" {
		t.Fatalf("unlaunched finding = %s", agg.Results[1].Findings)
	}
	if calls := proctest.Calls(t, second.Path); len(calls) != 0 {
		t.Fatalf("second extension started after the deadline: %+v", calls)
	}
}

// TestRunWaitDelay covers a valid response whose pipes a grandchild keeps open: warn.
func TestRunWaitDelay(t *testing.T) {
	t.Parallel()
	p := install(t, "demo", proctest.Response{
		Stdout:     `{"status":"pass"}`,
		Grandchild: 30 * time.Second,
	})
	o := Options{Runner: proc.Exec{WaitDelay: 200 * time.Millisecond}}
	agg := Run(context.Background(), o, StageEntry, []byte(request), []Plan{p})
	if agg.Status != StatusWarn || !strings.Contains(agg.Results[0].Error, "stayed open") {
		t.Fatalf("aggregate = %+v", agg)
	}
}

// TestRunStartFailure covers a resolved extension that fails to start: warn.
func TestRunStartFailure(t *testing.T) {
	t.Parallel()
	p := Plan{Name: "gone", Path: filepath.Join(t.TempDir(), "stack-gone")}
	agg := Run(context.Background(), Options{Runner: proc.Exec{}}, StageEntry, nil, []Plan{p})
	if agg.Status != StatusWarn || !strings.Contains(agg.Results[0].Error, "did not start") {
		t.Fatalf("aggregate = %+v", agg)
	}
}

// TestRunAggregate covers C21's aggregation: block > warn > pass, skipped listed, zero run = pass.
func TestRunAggregate(t *testing.T) {
	t.Parallel()
	pass := install(t, "a", proctest.Response{Stdout: `{"status":"pass","context":"A"}`})
	warnP := install(t, "b", proctest.Response{Stdout: `{"status":"warn"}`})
	block := install(t, "c", proctest.Response{Stdout: `{"status":"block"}`})
	skipped := Plan{Name: "missing", Err: proc.ErrNotFound}
	unrunnable := Plan{Name: "odd", Path: "x", Err: proc.ErrNotRunnable}

	tests := []struct {
		name    string
		plans   []Plan
		want    string
		results []string
		skipped []string
	}{
		{"none", nil, StatusPass, []string{}, []string{}},
		{"only skipped", []Plan{skipped}, StatusPass, []string{}, []string{"missing"}},
		{"pass", []Plan{pass, skipped}, StatusPass, []string{"a"}, []string{"missing"}},
		{"pass warn", []Plan{pass, warnP}, StatusWarn, []string{"a", "b"}, []string{}},
		{
			"block first",
			[]Plan{block, pass, warnP},
			StatusBlock,
			[]string{"c", "a", "b"},
			[]string{},
		},
		{"block last", []Plan{warnP, block}, StatusBlock, []string{"b", "c"}, []string{}},
		{"not runnable", []Plan{pass, unrunnable}, StatusWarn, []string{"a", "odd"}, []string{}},
	}
	for _, tt := range tests {
		agg := Run(context.Background(), Options{Runner: proc.Exec{}}, StageCycle,
			[]byte(request), tt.plans)
		var names []string
		for _, r := range agg.Results {
			names = append(names, r.Name)
		}
		if agg.Stage != StageCycle || agg.Status != tt.want ||
			!slices.Equal(append([]string{}, names...), tt.results) ||
			!slices.Equal(agg.Skipped, tt.skipped) {
			t.Errorf("%s: aggregate = %+v, want %s %v skipped %v", tt.name, agg, tt.want,
				tt.results, tt.skipped)
		}
	}

	// The JSON shape: results and skipped are arrays, never null.
	out, err := json.Marshal(Run(context.Background(), Options{}, StageEntry, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"stage":"entry","status":"pass","results":[],"skipped":[]}`; string(out) != want {
		t.Fatalf("empty aggregate = %s, want %s", out, want)
	}
	agg := Run(context.Background(), Options{}, StageEntry, nil, []Plan{unrunnable})
	f := findings(t, agg.Results[0])
	if len(f) != 1 || f[0]["extension"] != "odd" {
		t.Fatalf("not runnable finding = %s", agg.Results[0].Findings)
	}
}

// TestRunStderrAndEnv covers stderr passed through with a name prefix and the child environment.
func TestRunStderrAndEnv(t *testing.T) {
	t.Parallel()
	p := install(t, "demo", proctest.Response{Stdout: `{"status":"pass"}`, Stderr: "one\ntwo"})
	var stderr bytes.Buffer
	o := Options{Runner: proc.Exec{}, Stderr: &stderr}
	Run(context.Background(), o, StageEntry, []byte(request), []Plan{p})
	if got, want := stderr.String(), "[demo] one\n[demo] two\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	o = Options{
		Env:       []string{"A=1", "STACK_STAGE=old", "STACK_SESSION_ID=old"},
		SessionID: "s1",
	}
	if got, want := o.childEnv(StageCycle),
		[]string{"A=1", "STACK_STAGE=cycle", "STACK_SESSION_ID=s1"}; !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}
	o.SessionID = ""
	if got, want := o.childEnv(StageEntry),
		[]string{"A=1", "STACK_SESSION_ID=old", "STACK_STAGE=entry"}; !slices.Equal(got, want) {
		t.Fatalf("env without id = %q, want %q", got, want)
	}
}

// TestValidName covers C21's name rule: one lower-case file-name component.
func TestValidName(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"jev-router", "observe", "a1", "9"} {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false", n)
		}
	}
	for _, n := range []string{"", "../x", "a/b", `a\b`, "C:x", "-a", "Jev", "a.b", "a_b", ".."} {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true", n)
		}
	}
	if err := CheckNames([]string{"ok", "../x"}); !errors.Is(err, ErrInvalidName) ||
		!strings.Contains(err.Error(), `"../x"`) {
		t.Fatalf("CheckNames = %v", err)
	}
}

func TestNames(t *testing.T) {
	t.Parallel()
	s := config.Defaults().Stages
	for stage, want := range map[string][]string{
		StageEntry:    {"jev-router"},
		StageOutbound: {"jev-semantic"},
		StageCycle:    {"observe", "jev-classify", "improve"},
	} {
		if got, err := Names(s, stage); err != nil || !slices.Equal(got, want) {
			t.Errorf("Names(%s) = %v, %v", stage, got, err)
		}
	}
	if _, err := Names(s, "other"); !errors.Is(err, ErrUnknownStage) {
		t.Fatalf("unknown stage err = %v", err)
	}
}

// TestRunDiscovery covers C21's discovery through PATH: missing → skipped, not runnable → warn,
// and the first runnable candidate wins. It changes PATH, so it is not parallel.
func TestRunDiscovery(t *testing.T) {
	early, late := t.TempDir(), t.TempDir()
	ok := `{"status":"pass","context":"RAN"}`
	lateFake := proctest.InstallAt(t, late, Prefix+"x",
		proctest.Scenario{Responses: []proctest.Response{{Stdout: ok}}})
	if runtime.GOOS == "windows" {
		// A .cmd earlier on PATH than stack-x.exe: the .exe runs.
		write(t, filepath.Join(early, "stack-x.cmd"), "@echo {\"status\":\"block\"}\r\n")
		// Only a .cmd on PATH: found but not runnable.
		write(t, filepath.Join(early, "stack-y.cmd"), "@echo {\"status\":\"block\"}\r\n")
	} else {
		// A non-executable file earlier on PATH than an executable one: the executable runs.
		write(t, filepath.Join(early, "stack-x"), "#!/bin/sh\necho '{\"status\":\"block\"}'\n")
		// Only a non-executable file on PATH: found but not runnable.
		write(t, filepath.Join(early, "stack-y"), "#!/bin/sh\necho '{\"status\":\"block\"}'\n")
	}
	t.Setenv(
		"PATH",
		strings.Join([]string{"relative-dir", early, late}, string(os.PathListSeparator)),
	)

	plans := Resolve([]string{"x", "y", "z"})
	if !plans[0].Runnable() || plans[0].Path != lateFake {
		t.Fatalf("x plan = %v, want %s", plans[0], lateFake)
	}
	if plans[1].Runnable() || plans[1].Skipped() || !errors.Is(plans[1].Err, proc.ErrNotRunnable) {
		t.Fatalf("y plan = %v, want not runnable", plans[1])
	}
	if !plans[2].Skipped() || plans[2].String() != "z -> skipped" {
		t.Fatalf("z plan = %v, want skipped", plans[2])
	}

	agg := Run(
		context.Background(),
		Options{Runner: proc.Exec{}},
		StageEntry,
		[]byte(request),
		plans,
	)
	if agg.Status != StatusWarn || len(agg.Results) != 2 ||
		agg.Results[0].Context != "RAN" || agg.Results[1].Status != StatusWarn ||
		!strings.Contains(agg.Results[1].Error, "not runnable") ||
		!slices.Equal(agg.Skipped, []string{"z"}) {
		t.Fatalf("aggregate = %+v", agg)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
