package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/kit"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// pathDir puts a new directory first on PATH and returns it.
func pathDir(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// fakeEntry installs `stack-<name>` into bin answering resp.
func fakeEntry(t *testing.T, bin, name string, resp proctest.Response) string {
	t.Helper()
	return proctest.InstallAt(t, bin, ext.Prefix+name,
		proctest.Scenario{Responses: []proctest.Response{resp}})
}

func passWith(context string) proctest.Response {
	return proctest.Response{Stdout: `{"status":"pass","context":` + strconv.Quote(context) + `}`}
}

// board installs a fake board into the kit dir answering resp and returns its path.
func board(t *testing.T, kitDir string, resp proctest.Response) string {
	t.Helper()
	dir := filepath.Join(kitDir, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return proctest.InstallAt(t, dir, "board",
		proctest.Scenario{Responses: []proctest.Response{resp}})
}

// cutoffIn makes the hook's cut-off fall d after now on the real clock.
func cutoffIn(f *fixture, d time.Duration) time.Time {
	start := time.Now().Add(d - cutoff)
	f.params = func(p *Params) { p.Start = start }
	return start.Add(cutoff)
}

// C4: compaction runs the entry stage once after the restore, within the hook's time and the
// budget, without changing the restore. Not parallel: it changes PATH.
func TestCompactEntry(t *testing.T) {
	t.Run("once with the restored kits and the payload's id, no git", func(t *testing.T) {
		f := newFixture(t)
		bin := pathDir(t)
		git := proctest.InstallAt(t, bin, "git",
			proctest.Scenario{Responses: []proctest.Response{{Sleep: time.Minute}}})
		resp := passWith("ENTRY CTX")
		resp.LogEnv = []string{IDVar, "STACK_STAGE"}
		demo := fakeEntry(t, bin, "demo", resp)
		t.Setenv(IDVar, "other")
		f.cfg.Stages.Entry = []string{"demo", "absent"}
		f.open("b", map[string]string{"state.md": "B STATE"})
		f.open("a", map[string]string{"state.md": "A STATE"})
		f.open("gone", nil)
		if err := os.RemoveAll(f.kitDir("gone")); err != nil {
			t.Fatal(err)
		}
		// The board rewrites .kits after the restore read it; entry must not read it again.
		if err := os.MkdirAll(f.kitDir("late"), 0o700); err != nil {
			t.Fatal(err)
		}
		board(t, f.kitDir("a"), proctest.Response{
			Stdout:     "A BOARD\n",
			WriteFiles: map[string]string{paths.SessionKitsFile(f.home, testID): "late\n"},
		})
		write(t, filepath.Join(f.home, ".config", "stack", "handover", "t3.md"), "HANDOVER")
		f.env["T3CODE_THREAD_ID"] = "thread"

		ctx, _ := f.start(compactIn(testID))
		mustContain(t, ctx, "B STATE", "A STATE", "A BOARD",
			"\n\n<extension-context path="+strconv.Quote(demo)+">\nENTRY CTX\n</extension-context>"+
				"\n\n<orchestration-handover", "HANDOVER")
		calls := proctest.Calls(t, demo)
		want := `{"stage":"entry","repo":"","range":"","prompt":"","kits":[` +
			`{"slug":"b","dir":` + strconv.Quote(f.kitDir("b")) + `},` +
			`{"slug":"a","dir":` + strconv.Quote(f.kitDir("a")) + `}]}`
		if len(calls) != 1 || calls[0].Stdin != want ||
			calls[0].Env[IDVar] != testID || calls[0].Env["STACK_STAGE"] != ext.StageEntry {
			t.Fatalf("calls = %+v, want one with %s and %s=%s", calls, want, IDVar, testID)
		}
		if calls := proctest.Calls(t, git); len(calls) != 0 {
			t.Fatalf("git called: %+v", calls)
		}
	})

	t.Run("no restored kit, not invoked", func(t *testing.T) {
		bin := pathDir(t)
		demo := fakeEntry(t, bin, "demo", passWith("ENTRY CTX"))

		f := newFixture(t)
		f.cfg.Stages.Entry = []string{"demo"}
		f.rules("RULES")
		in := compactIn(testID)
		in.AgentType = "opus-worker"
		ctx, _ := f.start(in)
		mustContain(t, ctx, "RULES")

		// A listed kit that the lock keeps out is not restored either.
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		lock := flock.New(paths.SessionLockFile(f.home, testID))
		if err := lock.Lock(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Close() }()
		ctx, _ = f.start(in)
		mustContain(t, ctx, "RULES", "[kits omitted: .kits locked]")
		mustLack(t, ctx, "ENTRY CTX")
		if calls := proctest.Calls(t, demo); len(calls) != 0 {
			t.Fatalf("extension called: %+v", calls)
		}
	})
}

// C4: the entry stage at compaction runs only with time left and ends by the hook's cut-off,
// leaving the restore whole. Not parallel: it changes PATH.
func TestCompactEntryDeadline(t *testing.T) {
	t.Run("the deadline set at the check also bounds discovery", func(t *testing.T) {
		for _, tt := range []struct {
			name   string
			lookup time.Duration // how long discovery takes
			start  bool          // whether the extension is started at all
		}{
			{"window shrinks", 1500 * time.Millisecond, true},
			{"deadline passed", 2500 * time.Millisecond, false},
		} {
			t.Run(tt.name, func(t *testing.T) {
				bin := pathDir(t)
				demo := fakeEntry(t, bin, "demo",
					proctest.Response{Stdout: `{"status":"pass"}`, Sleep: time.Minute})
				f := newFixture(t)
				f.cfg.Stages.Entry = []string{"demo"}
				f.open("demo", map[string]string{"state.md": "DEMO STATE"})
				// A fixed clock that leaves the stage 2 s at its check.
				start := time.Now()
				now := start.Add(cutoff - proc.DefaultWaitDelay - 2*time.Second)
				f.params = func(p *Params) {
					p.Start = start
					p.Now = func() time.Time { return now }
					p.resolve = func(names []string) []ext.Plan {
						time.Sleep(tt.lookup)
						return ext.Resolve(names)
					}
				}
				begin := time.Now()
				ctx, _ := f.start(compactIn(testID))
				// The stage ends 2 s after its check, or when a longer discovery returns; a fresh
				// timer after discovery would add 2 s to the lookup. The slack covers the kill.
				if took, limit := time.Since(begin),
					max(tt.lookup, 2*time.Second)+750*time.Millisecond; took >= limit {
					t.Fatalf("stage took %s, want under %s: its deadline is 2 s after the check",
						took, limit)
				}
				if tt.start {
					mustContain(t, ctx, "[extension demo: warn: timed out")
					return
				}
				mustContain(t, ctx, "[extension demo: warn: not started: stage deadline")
				if calls := proctest.Calls(t, demo); len(calls) != 0 {
					t.Fatalf("extension started after the deadline: %+v", calls)
				}
			})
		}
	})

	t.Run("runs only with at least 1 s to its deadline", func(t *testing.T) {
		for _, tt := range []struct {
			left time.Duration
			run  bool
		}{
			{time.Second + 100*time.Millisecond, true},
			{time.Second - 100*time.Millisecond, false},
		} {
			t.Run(tt.left.String(), func(t *testing.T) {
				bin := pathDir(t)
				demo := fakeEntry(t, bin, "demo", passWith("ENTRY CTX"))
				f := newFixture(t)
				f.cfg.Stages.Entry = []string{"demo"}
				f.open("demo", map[string]string{"state.md": "DEMO STATE"})
				// A fixed clock that far before the cut-off less the allowance.
				start := time.Now()
				now := start.Add(cutoff - proc.DefaultWaitDelay - tt.left)
				f.params = func(p *Params) {
					p.Start = start
					p.Now = func() time.Time { return now }
				}
				ctx, _ := f.start(compactIn(testID))
				calls := proctest.Calls(t, demo)
				if tt.run {
					// Started: it answered, or a slow start under -race ran into the 1.1 s
					// deadline, which only a started extension reports as a timeout.
					if !strings.Contains(ctx, "ENTRY CTX") &&
						!strings.Contains(ctx, "[extension demo: warn: timed out") {
						t.Fatalf("entry did not run:\n%s", squash(ctx))
					}
					mustLack(t, ctx, "[entry skipped")
					return
				}
				mustContain(t, ctx,
					"DEMO STATE\n</orchestration-state>\n\n[entry skipped: deadline]\n")
				if len(calls) != 0 {
					t.Fatalf("extension called with too little time: %+v", calls)
				}
			})
		}
	})

	t.Run("after the cut-off nothing is looked up or started", func(t *testing.T) {
		bin := pathDir(t)
		demo := fakeEntry(t, bin, "demo", passWith("ENTRY CTX"))
		f := newFixture(t)
		// An invalid name would be reported if names were checked before the clock.
		f.cfg.Stages.Entry = []string{"../bad", "demo"}
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		start := time.Now().Add(-56 * time.Second)
		f.params = func(p *Params) { p.Start = start }
		ctx, _ := f.start(compactIn(testID))
		mustContain(t, ctx, "DEMO STATE\n</orchestration-state>\n\n[entry skipped: deadline]\n")
		mustLack(t, ctx, "entry extensions skipped")
		if calls := proctest.Calls(t, demo); len(calls) != 0 {
			t.Fatalf("extension started after the cut-off: %+v", calls)
		}
	})

	t.Run("bounded by the time the boards left", func(t *testing.T) {
		bin := pathDir(t)
		// Its timeout proves it started; a loaded machine may kill it before it logs the call.
		fakeEntry(t, bin, "first",
			proctest.Response{Stdout: `{"status":"pass"}`, Sleep: time.Minute})
		second := fakeEntry(t, bin, "second", passWith("SECOND"))
		f := newFixture(t)
		f.cfg.Stages.Entry = []string{"first", "second"}
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		// As if the boards had run to their 40 s deadline after a slow restore: 5 s are left.
		// No real board runs, since a fake's start under load would eat into that time.
		cut := cutoffIn(f, 5*time.Second)

		ctx, _ := f.start(compactIn(testID))
		// Entry gets what is left less the 2 s allowance, not its own 10 s: first is killed at
		// that deadline, well before the cut-off, and second never starts.
		if returned := time.Now(); !returned.Before(cut) {
			t.Fatalf("hook returned %s after the cut-off", returned.Sub(cut))
		}
		mustContain(t, ctx, "DEMO STATE", "[extension first: warn: timed out",
			"[extension second: warn: not started: stage deadline")
		if calls := proctest.Calls(t, second); len(calls) != 0 {
			t.Fatalf("second extension started after the deadline: %+v", calls)
		}
	})

	t.Run("a pipe left open is reaped by the cut-off", func(t *testing.T) {
		bin := pathDir(t)
		first := fakeEntry(t, bin, "first", proctest.Response{
			Stdout: `{"status":"pass"}`, Sleep: time.Minute, Grandchild: 30 * time.Second,
		})
		second := fakeEntry(t, bin, "second", passWith("SECOND"))
		f := newFixture(t)
		f.cfg.Stages.Entry = []string{"first", "second"}
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		// The cut-off as far as the 10 s cap: the extension gets the whole 8 s, since a fake's
		// start under a full suite's load can take seconds before it spawns and logs.
		cut := cutoffIn(f, ext.EntryTimeout)

		ctx, _ := f.start(compactIn(testID))
		// Killed at the deadline, the extension's pipes are reaped within the allowance, so the
		// stage ends at the cut-off; the slack covers the kill and the scheduler.
		if late := time.Since(cut); late > 500*time.Millisecond {
			t.Fatalf("hook returned %s after the cut-off", late)
		}
		mustContain(t, ctx, "DEMO STATE", "[extension first: warn: timed out",
			"[extension second: warn: not started")
		if calls := proctest.Calls(t, second); len(calls) != 0 {
			t.Fatalf("second extension started after the deadline: %+v", calls)
		}
		if calls := proctest.Calls(t, first); len(calls) != 1 || calls[0].GrandchildPID == 0 {
			t.Fatalf("first calls = %+v, want one that spawned a grandchild", calls)
		}
	})

	t.Run("a hanging extension leaves the restore whole", func(t *testing.T) {
		bin := pathDir(t)
		fakeEntry(t, bin, "hang", proctest.Response{Sleep: time.Minute})
		f := newFixture(t)
		f.rules("RULES")
		f.open("demo", map[string]string{"state.md": strings.Repeat("s", 5000)})
		in := compactIn(testID)
		in.TranscriptPath = f.transcript(userLine(marker + "\nBRIEF"))
		restore, _ := f.start(in)

		f.cfg.Stages.Entry = []string{"hang"}
		cutoffIn(f, 4*time.Second)
		ctx, _ := f.start(in)
		want := strings.TrimSuffix(restore, "\n") + "\n\n[extension hang: warn: timed out"
		if !strings.HasPrefix(ctx, want) {
			t.Fatalf("context =\n%s\nwant prefix\n%s", squash(ctx), squash(want))
		}
	})
}

// C4: entry contexts take only the budget the restore left. Not parallel: it changes PATH.
func TestCompactEntryBudget(t *testing.T) {
	t.Run("a long context is cut on a code-point boundary", func(t *testing.T) {
		bin := pathDir(t)
		demo := fakeEntry(t, bin, "demo", passWith(strings.Repeat("😀", 3000)))
		f := newFixture(t)
		f.cfg.Stages.Entry = []string{"demo"}
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		ctx, _ := f.start(compactIn(testID))
		// 6,000 UTF-16 units capped at 4,500: 2,250 whole emoji.
		mustContain(t, ctx, "DEMO STATE", "<extension-context path="+strconv.Quote(demo)+">\n"+
			strings.Repeat("😀", 2250)+"\n[...cut 1500 chars]\n</extension-context>\n")
		if !utf8.ValidString(ctx) {
			t.Fatal("context is not valid UTF-8")
		}
	})

	t.Run("no room left, no text and a note", func(t *testing.T) {
		bin := pathDir(t)
		fakeEntry(t, bin, "demo", passWith("ENTRY CTX"))
		f := newFixture(t)
		f.cfg.Stages.Entry = []string{"demo"}
		f.rules(strings.Repeat("r", 12_000))
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		in := compactIn(testID)
		in.AgentType = "opus-worker"
		ctx, _ := f.start(in)
		mustContain(t, ctx, "[state omitted: context limit: "+
			filepath.Join(f.kitDir("demo"), "state.md")+"]\n")
		mustLack(t, ctx, "ENTRY CTX", "entry context omitted")
		if got, want := f.stderr.String(),
			"stack: entry context of demo dropped: context limit\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})
}

// TestEntryOmittedMarker: an entry piece whose cut does not fit is replaced by its own marker.
func TestEntryOmittedMarker(t *testing.T) {
	t.Parallel()
	pc := tagged("extension-context", filepath.Join(t.TempDir(), "stack-demo"),
		strings.Repeat("x", 100), "")
	pc.omit = "[entry context omitted: context limit: demo]"
	a := &allocator{room: cost(pc.omit) + 10}
	a.place(pc, blockLimit)
	if pc.out != pc.omit || a.room != 10 {
		t.Fatalf("out = %q, room %d; want the marker and 10 left", pc.out, a.room)
	}
}

// TestEntryBareCap: a bare entry result (a warn line without context) is capped and cut like a
// tagged one, so a very long extension name cannot take the room of later results.
func TestEntryBareCap(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("x", 6000)
	r := ext.Result{Name: name, Status: ext.StatusWarn, Error: "not started: stage deadline"}
	var stderr strings.Builder
	a := newAllocator()
	out := placeEntry(a, &stderr, []ext.Result{r})
	body := "[extension " + name + ": warn: not started: stage deadline]"
	want := body[:blockLimit] + cutMarker(chars(body)-blockLimit)
	if len(out) != 1 || out[0].out != want || stderr.Len() != 0 {
		t.Fatalf("out = %q, stderr %q; want the body cut at %d", squash(out[0].out),
			stderr.String(), blockLimit)
	}
}

// TestCutExactFit: a cut whose actual marker fits the room exactly is used, not the omitted
// marker.
func TestCutExactFit(t *testing.T) {
	t.Parallel()
	pc := tagged("orchestration-state", filepath.Join(t.TempDir(), "state.md"),
		strings.Repeat("x", 100), kit.KindState)
	want := pc.head + "x" + cutMarker(99) + pc.tail
	a := &allocator{room: cost(want)}
	a.place(pc, 0)
	if pc.out != want || a.room != 0 {
		t.Fatalf("out = %q, room %d; want %q", pc.out, a.room, want)
	}
}
