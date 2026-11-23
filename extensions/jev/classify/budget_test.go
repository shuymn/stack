package classify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// withBudget sets the process budget and the finalisation reserve for one test (§3.1). Tests that
// set them do not run in parallel.
func withBudget(t *testing.T, b, r time.Duration) {
	t.Helper()
	ob, or := budget, reserve
	budget, reserve = b, r
	t.Cleanup(func() { budget, reserve = ob, or })
}

// slowT3 answers every query after blocking until the work deadline.
type slowT3 struct{ replayT3 }

func (s slowT3) Users(ctx context.Context, _, _ time.Time) ([]T3Row, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("slow t3: %w", ctx.Err())
}

// childT3 is a sqlite3 child that hangs until its own deadline, ChildMargin before the work
// deadline, and then fails as SQLite does.
type childT3 struct{ replayT3 }

func (s childT3) Users(ctx context.Context, _, _ time.Time) ([]T3Row, error) {
	cctx, cancel := childContext(ctx)
	defer cancel()
	<-cctx.Done()
	return nil, fmt.Errorf("%w: %w", errSQLite, errSQLiteTimeout)
}

// lateCwdT3 answers the cwd query only once its sqlite3 child deadline has passed: inside the
// margin, before the work deadline; with err set it fails with that ordinary error.
type lateCwdT3 struct {
	replayT3

	err error
}

func (s lateCwdT3) Cwds(ctx context.Context, threads []string) (map[string]string, error) {
	cctx, cancel := childContext(ctx)
	defer cancel()
	<-cctx.Done()
	if s.err != nil {
		return nil, s.err
	}
	return s.replayT3.Cwds(ctx, threads)
}

const bigPad = 15 << 10

// bigWeek is a week of 10 replies of 30 KiB, oldest first, one to a request: fix replies on
// even positions qualify; the memory index names the even ones.
func bigWeek(t *testing.T) (*world, []string) {
	t.Helper()
	w := newWorld(t)
	w.mkdir("work")
	pad := strings.Repeat("p ", bigPad)
	ids := make([]string, 0, 10)
	for i := range 10 {
		first := "fix"
		if i%2 == 1 {
			first = "ok"
		}
		ids = append(
			ids,
			w.turn("p/s.jsonl", "work", "task", "done", fmt.Sprintf("%s w%d %s", first, i, pad)),
		)
	}
	w.write()
	w.memory("work", "- w0 w2 w4 w6 w8 are known\n")
	return w, ids
}

// TestBudget is C4 for classify: the deadline at each stage of the run leaves the newest items
// checked and the oldest unevaluated, with complete false and the report written inside the
// reserve; the report lock bound is inside the process deadline.
//
//nolint:gocyclo,maintidx // one subtest per claim
func TestBudget(t *testing.T) {
	const b, r = 4 * time.Second, 2 * time.Second // work deadline 2 s
	answeredBy := func(t *testing.T, start time.Time) {
		t.Helper()
		if d := time.Since(start); d > b {
			t.Fatalf("answered after %v, over the %v budget", d, b)
		}
	}

	for _, slow := range []struct {
		name string
		t3   func(replayT3) T3
	}{
		{"the work deadline", func(r replayT3) T3 { return slowT3{r} }},
		{"a sqlite3 child deadline", func(r replayT3) T3 { return childT3{r} }},
	} {
		t.Run("a deadline in preparation after one source was read sends nothing/"+slow.name,
			func(t *testing.T) { prepDeadlineCase(t, b, r, slow.t3, answeredBy) })
	}

	t.Run("a query that succeeds inside the child margin is kept", func(t *testing.T) {
		withBudget(t, b, r)
		w := newWorld(t)
		w.mkdir("work")
		rows := w.thread("th", "work", t3Owner("a", "task"), t3Text("done"), t3Owner("b", "fix c"))
		id := w.t3Reply("th", rows[2])
		w.write()
		res := w.run(t, Env{Transport: &fake{p: byPrefix}, T3: lateCwdT3{replayT3: w.t3}},
			w.request())
		if it := res.item(t, id); it.Status != StatusClassified || it.Reason != "" {
			t.Fatalf("%+v", it)
		}
	})

	t.Run("an ordinary error inside the child margin is a cwd unknown", func(t *testing.T) {
		withBudget(t, b, r)
		w := newWorld(t)
		w.mkdir("work")
		rows := w.thread("th", "work", t3Owner("a", "task"), t3Text("done"), t3Owner("b", "fix c"))
		id := w.t3Reply("th", rows[2])
		w.write()
		t3 := lateCwdT3{replayT3: w.t3, err: fmt.Errorf("%w: exit 1", errSQLite)}
		res := w.run(t, Env{Transport: &fake{p: byPrefix}, T3: t3}, w.request())
		if it := res.item(t, id); it.Status != StatusClassified ||
			it.DupReason != ReasonCwdUnknown {
			t.Fatalf("%+v", it)
		}
	})

	t.Run(
		"a deadline during a later kind_ group keeps the newest items checked",
		func(t *testing.T) {
			withBudget(t, b, r)
			w, ids := bigWeek(t)
			// Groups of 4: w9..w6 answered; every request with an older reply hangs.
			f := &fake{p: byPrefix, block: func(bt jev.Batch) bool {
				s := string(bt.Body)
				if !strings.Contains(s, `"kind_r`) {
					return false
				}
				for i := range 6 {
					if strings.Contains(s, fmt.Sprintf("w%d ", i)) {
						return true
					}
				}
				return false
			}}
			start := time.Now()
			res := w.run(t, Env{Transport: f}, w.request())
			answeredBy(t, start)
			for i, id := range ids {
				it := res.item(t, id)
				if i >= 6 {
					if it.Status != StatusClassified {
						t.Fatalf("w%d: %+v", i, it)
					}
					continue
				}
				if it.Status != StatusUnevaluated || it.Reason != ReasonDeadline || it.Done() {
					t.Fatalf("w%d: %+v", i, it)
				}
			}
			// The recent qualified items stay checked and listed.
			if it := res.item(t, ids[8]); it.Duplicate != DupDuplicate || !it.Done() {
				t.Fatalf("w8: %+v", it)
			}
			if it := res.item(t, ids[6]); it.Duplicate != DupDuplicate {
				t.Fatalf("w6: %+v", it)
			}
			if res.report.Complete || res.resp.Status != jev.StatusPass {
				t.Fatalf("complete %v, status %s", res.report.Complete, res.resp.Status)
			}
		},
	)

	t.Run("a deadline during a group's dup_ requests leaves its qualified items unassessed",
		func(t *testing.T) {
			withBudget(t, b, r)
			w, ids := bigWeek(t)
			f := &fake{p: byPrefix, block: func(bt jev.Batch) bool {
				return strings.Contains(string(bt.Body), `"memory_index"`)
			}}
			start := time.Now()
			res := w.run(t, Env{Transport: f}, w.request())
			answeredBy(t, start)
			for _, i := range []int{8, 6} {
				it := res.item(t, ids[i])
				if it.Status != StatusClassified || !it.Qualified ||
					it.Duplicate != DupUnassessed ||
					it.DupReason != ReasonDeadline ||
					it.Done() {
					t.Fatalf("w%d: %+v", i, it)
				}
			}
			// Later groups were never sent.
			if it := res.item(
				t,
				ids[0],
			); it.Status != StatusUnevaluated ||
				it.Reason != ReasonDeadline {
				t.Fatalf("w0: %+v", it)
			}
			if !strings.Contains(res.resp.Context, "(dup unassessed: deadline)") ||
				res.report.Complete {
				t.Fatalf("context %q", res.resp.Context)
			}
		})

	t.Run("a slow git is the work deadline's, and a child holding its pipe does not outlast it",
		func(t *testing.T) {
			withBudget(t, b, r)
			w := newWorld(t)
			w.mkdir("work")
			id := w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
			w.write()
			git := proctest.Install(t, "git", proctest.Scenario{Responses: []proctest.Response{{
				Stdout: "/x", Sleep: time.Minute, Grandchild: time.Minute,
			}}})
			start := time.Now()
			res := w.run(t, Env{
				Transport: &fake{p: byPrefix}, Git: git, Runner: proc.Exec{WaitDelay: jev.PipeWait},
			}, w.request())
			answeredBy(t, start)
			it := res.item(t, id)
			if it.Duplicate != DupUnassessed || it.DupReason != ReasonDeadline || it.Done() {
				t.Fatalf("%+v", it)
			}
		})

	t.Run("the report lock released just before its cutoff publishes before the deadline",
		func(t *testing.T) {
			withBudget(t, b, r) // cutoff: 4 s - 1 s = 3 s from the start
			w := newWorld(t)
			w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
			w.write()
			dir := filepath.Dir(reportPath(w.home))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			held := flock.New(filepath.Join(dir, "latest.lock"))
			if ok, err := held.TryLock(); err != nil || !ok {
				t.Fatalf("lock: %v %v", ok, err)
			}
			time.AfterFunc(2500*time.Millisecond, func() { _ = held.Unlock() })
			start := time.Now()
			res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
			answeredBy(t, start)
			if res.report == nil || res.resp.Status != jev.StatusPass {
				t.Fatalf("not published: %+v", res.resp)
			}
		})

	t.Run("the report lock held past its cutoff is latest-not-written, answered in time",
		func(t *testing.T) {
			withBudget(t, b, r)
			w := newWorld(t)
			w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
			w.write()
			dir := filepath.Dir(reportPath(w.home))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			held := flock.New(filepath.Join(dir, "latest.lock"))
			if ok, err := held.TryLock(); err != nil || !ok {
				t.Fatalf("lock: %v %v", ok, err)
			}
			defer func() { _ = held.Unlock() }()
			start := time.Now()
			res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
			answeredBy(t, start)
			if res.resp.Status != jev.StatusWarn || res.report != nil ||
				!strings.Contains(res.resp.Context, "report not written: report lock busy") {
				t.Fatalf("%+v", res.resp)
			}
			if got := entries(t, dir); len(got) != 1 || got[0] != "latest.lock" {
				t.Fatalf("directory %v: a temp file survived", got)
			}
			var f jev.Finding
			for _, x := range res.resp.Findings {
				if x.Kind == kindNotWritten {
					f = x
				}
			}
			if f.Detail != "report lock busy" {
				t.Fatalf("findings %+v", res.resp.Findings)
			}
		})
}

// prepDeadlineCase is TestBudget's preparation case: a changed transcript reply, a good one and a
// T3 reply whose read hangs until a deadline; nothing is sent.
func prepDeadlineCase(
	t *testing.T,
	b, r time.Duration,
	slow func(replayT3) T3,
	answeredBy func(*testing.T, time.Time),
) {
	t.Helper()
	withBudget(t, b, r)
	w := newWorld(t)
	// Oldest first: a changed transcript record, a good one, then a T3 item whose read hangs.
	line := w.transcript("p/s.jsonl", "work", owner("task"), text("done"), owner("fix a"))[2]
	changed := w.reply("p/s.jsonl", line)
	w.files["p/s.jsonl"][line-1]["message"] = map[string]any{"content": "fix a, edited"}
	good := w.turn("p/s.jsonl", "work", "task", "done", "fix b")
	rows := w.thread("th", "work", t3Owner("a", "task"), t3Text("done"), t3Owner("b", "fix c"))
	pending := w.t3Reply("th", rows[2])
	w.write()
	f := &fake{p: byPrefix}
	start := time.Now()
	res := w.run(t, Env{Transport: f, T3: slow(w.t3)}, w.request())
	answeredBy(t, start)
	if len(f.sent()) != 0 || res.report == nil || res.report.Complete {
		t.Fatalf("sent %d, report %+v", len(f.sent()), res.report)
	}
	if it := res.item(
		t,
		changed,
	); it.Status != StatusUnresolved ||
		it.Reason != ReasonRecordChanged {
		t.Fatalf("changed: %+v", it)
	}
	for _, id := range []string{good, pending} {
		if it := res.item(
			t,
			id,
		); it.Status != StatusUnevaluated ||
			it.Reason != ReasonPrepDeadline {
			t.Fatalf("%s: %+v", id, it)
		}
	}
	if res.resp.Status != jev.StatusPass ||
		!strings.Contains(res.resp.Context, "complete: false") {
		t.Fatalf("%+v", res.resp)
	}
}
