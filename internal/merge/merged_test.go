package merge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

const mergedLine = "MERGED " + head + " " + mergeOID + "\n"

// orderLog is one ordered log of the run's stdout writes and the merged stage's extension starts.
type orderLog struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	text string
	at   time.Time
}

// Write logs each stdout write as "stdout <bytes>".
func (l *orderLog) Write(b []byte) (int, error) {
	l.add("stdout " + string(b))
	return len(b), nil
}

func (l *orderLog) add(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, logEntry{text, time.Now()})
}

func (l *orderLog) texts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e.text)
	}
	return out
}

// startRunner logs "start <name>" for each extension before it starts it, then runs onStart.
type startRunner struct {
	proc.Exec

	log     *orderLog
	onStart func()
}

func (r startRunner) Run(ctx context.Context, c proc.Cmd) (proc.Result, error) {
	name := strings.TrimSuffix(filepath.Base(c.Name), ".exe")
	r.log.add("start " + strings.TrimPrefix(name, ext.Prefix))
	if r.onStart != nil {
		r.onStart()
	}
	return r.Exec.Run(ctx, c)
}

// stageFixture is a merged stage over the fakes in bin.
type stageFixture struct {
	bin   string
	log   *orderLog
	kits  atomic.Int32
	fakes map[string]string
}

var testKits = []ext.Kit{{Slug: "demo", Dir: filepath.Join(os.TempDir(), "kits", "demo")}}

// newStage installs a fake `stack-<name>` for each response in a dir of its own; its stages look
// names up there only.
func newStage(t *testing.T, fakes map[string]proctest.Response) *stageFixture {
	t.Helper()
	f := &stageFixture{bin: t.TempDir(), log: &orderLog{}, fakes: map[string]string{}}
	for name, resp := range fakes {
		resp.LogEnv = []string{"STACK_STAGE", "STACK_SESSION_ID"}
		f.fakes[name] = proctest.InstallAt(t, f.bin, ext.Prefix+name,
			proctest.Scenario{Responses: []proctest.Response{resp}})
	}
	return f
}

func (f *stageFixture) stage(names []string, timeout time.Duration, onStart func()) MergedStage {
	return MergedStage{
		Names: names,
		Kits: func(context.Context) *[]ext.Kit {
			f.kits.Add(1)
			return &testKits
		},
		Ext: ext.Options{
			Runner:    startRunner{log: f.log, onStart: onStart},
			SessionID: "s1",
		},
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

func (f *stageFixture) calls(t *testing.T, name string) []proctest.Call {
	t.Helper()
	return proctest.Calls(t, f.fakes[name])
}

// started reports the extensions the stage started, in order.
func (f *stageFixture) started() []string {
	var out []string
	for _, e := range f.log.texts() {
		if name, ok := strings.CutPrefix(e, "start "); ok {
			out = append(out, name)
		}
	}
	return out
}

// TestMergedStage covers C5: after a verified merge the MERGED line is written before the first
// extension starts, each result to show follows it, and the run succeeds whatever they answered.
func TestMergedStage(t *testing.T) {
	t.Parallel()
	fakes := map[string]proctest.Response{
		"demo":  {Stdout: `{"status":"warn","context":"  due: run steward\n"}`},
		"gate":  {Stdout: `{"status":"block","context":"STOP"}`},
		"quiet": {Stdout: `{"status":"pass"}`},
	}
	names := []string{"demo", "gate", "quiet"}
	shown := []string{
		"[extension demo: warn: warn]\ndue: run steward\n",
		"[extension gate: warn: block is not allowed at stage merged]\nSTOP\n",
	}
	results := strings.Join(shown, "")
	// The stdout writes and the starts, in order: the line before any start, the results after
	// the last, a passing result without context not at all.
	order := []string{
		"stdout " + mergedLine, "start demo", "start gate", "start quiet",
		"stdout " + shown[0], "stdout " + shown[1],
	}

	t.Run("direct", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		out := run(t, okResponses(), setup{
			fakeGit: true, stdout: f.log,
			opts: func(o *Options) { o.Merged = f.stage(names, time.Minute, nil) },
		})
		if out.err != nil || out.stdout != mergedLine+results {
			t.Fatalf("err = %v, stdout = %q", out.err, out.stdout)
		}
		if got := f.log.texts(); !slices.Equal(got, order) {
			t.Fatalf("order = %q, want %q", got, order)
		}
		checkMergedRequest(t, f, out)
	})

	t.Run("auto while polling", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		out := runAuto(t, autoCase{
			polls:  []proctest.Response{openAutoResponse(), rb("MERGED", head, mergeOID, false)},
			stdout: f.log,
			opts:   func(o *Options) { o.Merged = f.stage(names, time.Minute, nil) },
		})
		if out.err != nil || out.stdout != mergedLine+results ||
			!slices.Equal(f.log.texts(), order) {
			t.Fatalf("err = %v, stdout = %q, order %q", out.err, out.stdout, f.log.texts())
		}
	})

	t.Run("auto final read-back after the wait deadline", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		// After the first OPEN answer the clock reaches the deadline: one poll, then the disable
		// and the final read-back, which finds the merge.
		out := runAuto(t, autoCase{
			polls:      []proctest.Response{openAutoResponse()},
			final:      rb("MERGED", head, mergeOID, false),
			deadlineAt: 1,
			stdout:     f.log,
			opts:       func(o *Options) { o.Merged = f.stage(names, time.Minute, nil) },
		})
		want := seq(autoMergeCall, autoRead, autoDisable, autoRead)
		if out.err != nil || !slices.EqualFunc(out.gh, want, slices.Equal) {
			t.Fatalf("err = %v, gh = %q; want %q", out.err, out.gh, want)
		}
		if out.stdout != mergedLine+results || !slices.Equal(f.log.texts(), order) {
			t.Fatalf("stdout = %q, order %q", out.stdout, f.log.texts())
		}
	})

	t.Run("closed stdout", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		out := run(t, okResponses(), setup{
			brokenStdout: true,
			opts:         func(o *Options) { o.Merged = f.stage(names, time.Minute, nil) },
		})
		if out.err != nil || !strings.Contains(out.stderr, "MERGED "+head) ||
			!slices.Equal(f.started(), []string{"demo", "gate", "quiet"}) {
			t.Fatalf("err = %v, stderr = %q, started %q", out.err, out.stderr, f.started())
		}
	})
}

// TestMergedStageDeadline covers C5's deadline: nothing starts after the runner's deadline, and a
// pipe a grandchild holds open is reaped within the stage's window (20 s in that case).
func TestMergedStageDeadline(t *testing.T) {
	t.Parallel()
	t.Run("a slow first extension", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, map[string]proctest.Response{
			"slow": {Stdout: `{"status":"pass"}`, Sleep: time.Minute},
			"next": {Stdout: `{"status":"pass","context":"NEXT"}`},
		})
		// A 3 s window: the runner's deadline is 1 s after the stage starts.
		out := run(t, okResponses(), setup{opts: func(o *Options) {
			o.Merged = f.stage([]string{"slow", "next"}, 3*time.Second, nil)
		}})
		want := mergedLine + "[extension slow: warn: timed out: stage deadline of 3s exceeded]\n" +
			"[extension next: warn: not started: stage deadline of 3s exceeded]\n"
		if out.err != nil || out.stdout != want {
			t.Fatalf("err = %v, stdout = %q, want %q", out.err, out.stdout, want)
		}
		if calls := f.calls(
			t,
			"next",
		); len(calls) != 0 ||
			!slices.Equal(f.started(), []string{"slow"}) {
			t.Fatalf("next started after the deadline: %+v, %q", calls, f.started())
		}
	})

	t.Run("a pipe left open by a grandchild", func(t *testing.T) {
		t.Parallel()
		// Twice the stage's 10 s: under a full suite's load the first start of a fresh fake has
		// taken over the 8 s before the runner's deadline to spawn and log. The grandchild
		// outlives the window, so a stage that waited for it would end long after.
		const window = 2 * ext.MergedTimeout
		f := newStage(t, map[string]proctest.Response{
			"first": {
				Stdout:     `{"status":"pass"}`,
				Sleep:      time.Minute,
				Grandchild: window + 10*time.Second,
			},
			"next": {Stdout: `{"status":"pass","context":"NEXT"}`},
		})
		out := run(t, okResponses(), setup{stdout: f.log, opts: func(o *Options) {
			o.Merged = f.stage([]string{"first", "next"}, window, nil)
		}})
		if out.err != nil ||
			!strings.HasPrefix(out.stdout, mergedLine+"[extension first: warn: ") ||
			!strings.Contains(out.stdout, "[extension next: warn: not started") {
			t.Fatalf("err = %v, stdout = %q", out.err, out.stdout)
		}
		if calls := f.calls(t, "first"); len(calls) != 1 || calls[0].GrandchildPID == 0 {
			t.Fatalf("first calls = %+v, want one that spawned a grandchild", calls)
		}
		if calls := f.calls(t, "next"); len(calls) != 0 {
			t.Fatalf("next started after the deadline: %+v", calls)
		}
		// The stage starts after the MERGED write and its results are printed once it returned:
		// the reaping fits in the window. A runner deadline at the window's end rather than a
		// pipe wait before it would return no earlier than a pipe wait after it, so that is the
		// limit, and the kill and the scheduler have the pipe wait's 2 s.
		f.log.mu.Lock()
		entries := slices.Clone(f.log.entries)
		f.log.mu.Unlock()
		if n := len(entries); n != 4 || entries[0].text != "stdout "+mergedLine {
			t.Fatalf("log = %+v", entries)
		}
		limit := window + proc.DefaultWaitDelay
		if took := entries[2].at.Sub(entries[0].at); took >= limit {
			t.Fatalf("stage returned %s after the MERGED line, want within %s, under %s", took,
				window, limit)
		}
	})
}

// deadlineRunner records the deadline and the time of its one call instead of running anything.
type deadlineRunner struct {
	proc.Exec

	deadline, called *time.Time
}

func (r deadlineRunner) Run(ctx context.Context, _ proc.Cmd) (proc.Result, error) {
	*r.called = time.Now()
	*r.deadline, _ = ctx.Deadline()
	return proc.Result{Stdout: []byte(`{"status":"pass"}`)}, nil
}

// TestMergedStageRunnerDeadline covers C5's pipe allowance on the clock: the runner's deadline is
// the stage's end, 10 s after the window opens, less the runner's pipe wait. The window opens
// between the two readings around it, so the check holds however slow the scheduler; a deadline
// at the end itself is past the later bound unless the stage took the whole pipe wait to call the
// runner.
func TestMergedStageRunnerDeadline(t *testing.T) {
	t.Parallel()
	var deadline, called time.Time
	s := MergedStage{
		Names: []string{"demo"},
		Ext:   ext.Options{Runner: deadlineRunner{deadline: &deadline, called: &called}},
		resolve: func([]string) []ext.Plan {
			return []ext.Plan{{Name: "demo", Path: "demo"}}
		},
	}
	before := time.Now()
	runMerged(t.Context(), s, io.Discard, "o/r", ext.Merge{})
	runnerWindow := ext.MergedTimeout - proc.DefaultWaitDelay
	lo, hi := before.Add(runnerWindow), called.Add(runnerWindow)
	if deadline.Before(lo) || deadline.After(hi) {
		t.Fatalf("runner deadline %s after the stage began, want %s less the pipe wait %s",
			deadline.Sub(before), ext.MergedTimeout, proc.DefaultWaitDelay)
	}
}

// checkMergedRequest checks demo's one request of a direct merge: it is what the run verified, and
// the stage made no git or gh call of its own (the one git call found the top level, and gh saw
// only the merge's calls).
func checkMergedRequest(t *testing.T, f *stageFixture, out outcome) {
	t.Helper()
	calls := f.calls(t, "demo")
	var req map[string]json.RawMessage
	if len(calls) != 1 || json.Unmarshal([]byte(calls[0].Stdin), &req) != nil {
		t.Fatalf("demo calls = %+v", calls)
	}
	kits, err := json.Marshal(testKits)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"stage": `"merged"`, "repo": `"` + filepath.ToSlash(out.dir) + `"`,
		"range": `""`, "prompt": `""`, "kits": string(kits),
		"merge": `{"pr":7,"head":"` + head + `","oid":"` + mergeOID + `"}`,
	}
	for k, v := range want {
		if string(req[k]) != v {
			t.Errorf("request %s = %s, want %s", k, req[k], v)
		}
	}
	if len(req) != len(want) {
		t.Errorf("request = %s, want only %v", calls[0].Stdin, want)
	}
	if env := calls[0].Env; env["STACK_STAGE"] != "merged" || env["STACK_SESSION_ID"] != "s1" {
		t.Errorf("extension env = %v", env)
	}
	if len(out.git) != 1 || len(out.gh) != len(okResponses()) || f.kits.Load() != 1 {
		t.Errorf("git calls %+v, gh calls %d, kits reads %d", out.git, len(out.gh), f.kits.Load())
	}
}

// TestMergedStageNotRun covers C6: refusals, unknown results and a cancel before the success point
// never run merged; a cancel after it started leaves the merge reported.
func TestMergedStageNotRun(t *testing.T) {
	t.Parallel()
	fakes := map[string]proctest.Response{"demo": {Stdout: `{"status":"pass","context":"RAN"}`}}
	names := []string{"demo"}
	notRun := func(t *testing.T, f *stageFixture) {
		t.Helper()
		if calls := f.calls(t, "demo"); len(calls) != 0 || len(f.started()) != 0 ||
			f.kits.Load() != 0 {
			t.Fatalf("merged ran: calls %+v, started %q", calls, f.started())
		}
	}

	t.Run("refusal", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		s := with(okResponses(), kChecks, proctest.Response{Stdout: checksPage(head, 1, false, "",
			`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"FAILURE"}`)})
		out := run(
			t,
			s,
			setup{opts: func(o *Options) { o.Merged = f.stage(names, time.Minute, nil) }},
		)
		if !errors.Is(out.err, ErrRefused) || out.stdout != "" {
			t.Fatalf("err = %v, stdout = %q; want a refusal", out.err, out.stdout)
		}
		notRun(t, f)
	})

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		s := with(okResponses(), kReadBack, proctest.Response{Stderr: "HTTP 502\n", Exit: 1})
		out := run(
			t,
			s,
			setup{opts: func(o *Options) { o.Merged = f.stage(names, time.Minute, nil) }},
		)
		if !errors.Is(out.err, ErrUnknown) || out.stdout != "" {
			t.Fatalf("err = %v, stdout = %q; want result unknown", out.err, out.stdout)
		}
		notRun(t, f)
	})

	t.Run("cancel during the merge call", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		s := with(okResponses(), kMerge, proctest.Response{Sleep: 20 * time.Second})
		out := run(t, s, setup{stop: true, opts: func(o *Options) {
			o.Merged = f.stage(names, time.Minute, nil)
		}})
		if out.err != nil || out.stdout != mergedLine {
			t.Fatalf("err = %v, stdout = %q; want the merge reported", out.err, out.stdout)
		}
		notRun(t, f)
	})

	t.Run("cancel then success found by the final read-back", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, fakes)
		out := runAuto(t, autoCase{
			disable:  proctest.Response{Stderr: "Pull request is already merged\n", Exit: 1},
			polls:    []proctest.Response{openAutoResponse()},
			final:    rb("MERGED", head, mergeOID, false),
			cancelAt: 1,
			opts:     func(o *Options) { o.Merged = f.stage(names, time.Minute, nil) },
		})
		if out.err != nil || out.stdout != mergedLine {
			t.Fatalf("err = %v, stdout = %q; want the merge reported", out.err, out.stdout)
		}
		notRun(t, f)
	})

	t.Run("cancel after merged started", func(t *testing.T) {
		t.Parallel()
		f := newStage(t, map[string]proctest.Response{
			"demo": {Stdout: `{"status":"pass"}`, Sleep: time.Minute},
		})
		var cancel context.CancelFunc
		start := time.Now()
		out := run(t, okResponses(), setup{
			withCancel: func(c context.CancelFunc) { cancel = c },
			opts: func(o *Options) {
				o.Merged = f.stage(names, time.Minute, func() { cancel() })
			},
		})
		if out.err != nil || !strings.HasPrefix(out.stdout, mergedLine+"[extension demo: warn: ") {
			t.Fatalf("err = %v, stdout = %q; want the merge reported", out.err, out.stdout)
		}
		if took := time.Since(start); took > 30*time.Second {
			t.Fatalf("the cancel did not stop the extension (%s)", took)
		}
	})
}

// TestMergedNoExtension covers C7: with nothing to start, pr merge's output is today's, and the
// session's kits are never read.
func TestMergedNoExtension(t *testing.T) {
	t.Parallel()
	today := run(t, okResponses(), setup{})
	if today.err != nil || today.stdout != mergedLine {
		t.Fatalf("baseline = %+v", today)
	}
	for _, names := range [][]string{nil, {}, {"absent", "gone"}} {
		f := newStage(t, nil)
		out := run(t, okResponses(), setup{opts: func(o *Options) {
			o.Merged = f.stage(names, time.Minute, nil)
		}})
		if out.err != nil || out.stdout != today.stdout || out.stderr != today.stderr ||
			f.kits.Load() != 0 || len(f.started()) != 0 {
			t.Errorf("names %q: err = %v, stdout = %q, stderr = %q, kits reads %d", names,
				out.err, out.stdout, out.stderr, f.kits.Load())
		}
	}

	// Found but not runnable, and nothing runnable: its warn marker after the line, no request.
	f := newStage(t, nil)
	odd := filepath.Join(f.bin, ext.Prefix+"odd")
	if runtime.GOOS == "windows" {
		odd += ".cmd"
	}
	if err := os.WriteFile(odd, []byte("not a program\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, okResponses(), setup{opts: func(o *Options) {
		o.Merged = f.stage([]string{"absent", "odd"}, time.Minute, nil)
	}})
	if out.err != nil || !strings.HasPrefix(out.stdout, mergedLine+"[extension odd: warn: ") ||
		!strings.Contains(out.stdout, "not runnable") || strings.Count(out.stdout, "\n") != 2 ||
		out.stderr != today.stderr || f.kits.Load() != 0 || len(f.started()) != 0 {
		t.Fatalf("not runnable: err = %v, stdout = %q, stderr = %q, kits reads %d", out.err,
			out.stdout, out.stderr, f.kits.Load())
	}
}

func openAutoResponse() proctest.Response {
	return rb("OPEN", head, "", true)
}
