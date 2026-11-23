package classify

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// ctxLines splits a context into its lines.
func ctxLines(r result) []string { return strings.Split(r.resp.Context, "\n") }

// TestContext is C9's context and coverage: order, markers, the 10-line bound with every
// candidate's excerpt in the report, and complete with the coverage lines for an empty healthy
// week, an empty week with a failed source, an all-unresolved week and a removed worktree; with
// the duplicate check disabled, every candidate marked and the disabled line with its suffix (a
// week with candidates, an empty one), without it when both decisions are disabled, and a failed
// kind_ request still not done.
//
//nolint:gocyclo,maintidx // one subtest per claim
func TestContext(t *testing.T) {
	t.Run("12 candidates give 10 lines and a marker, all 12 in the report", func(t *testing.T) {
		w := newWorld(t)
		w.mkdir("work")
		var ids []string
		for i := 1; i <= 12; i++ {
			ids = append(
				ids,
				w.turn("p/s.jsonl", "work", "task", "done", fmt.Sprintf("fix c%d", i)),
			)
		}
		w.write()
		w.memory("work", "- nothing relevant\n")
		r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		lines := ctxLines(r)
		if len(lines) != 12 ||
			!strings.HasPrefix(lines[0], name+" ("+jev.Pin+"): 12 owner replies: "+
				"12 qualified (12 new, 0 duplicate, 0 dup unassessed)") ||
			lines[11] != "(+2 more in the report)" {
			t.Fatalf("context:\n%s", r.resp.Context)
		}
		// Newest first: c12 … c3 in the 10 lines, c2 and c1 only in the report.
		for i, line := range lines[1:11] {
			want := fmt.Sprintf("correction p/s.jsonl:%d: fix c%d", 3*(12-i), 12-i)
			if line != want {
				t.Fatalf("line %d %q, want %q", i+1, line, want)
			}
		}
		for i, id := range ids {
			if it := r.item(t, id); it.Excerpt != fmt.Sprintf("fix c%d", i+1) || !it.Qualified {
				t.Fatalf("report item %d: %+v", i, it)
			}
		}
		if !r.report.Complete || r.report.Counts.New != 12 {
			t.Fatalf("%+v", r.report.Counts)
		}
	})

	t.Run(
		"new candidates first, then unassessed ones marked, each newest first",
		func(t *testing.T) {
			w := newWorld(t)
			w.mkdir("work")
			w.turn("p/s.jsonl", "work", "task", "done", "fix n1")
			w.turn("p/s.jsonl", "gone", "task", "done", "fix u1")
			w.turn("p/s.jsonl", "work", "task", "done", "fix n2")
			w.turn("p/s.jsonl", "gone", "task", "done", "fix u2")
			w.write()
			w.memory("work", "- nothing relevant\n")
			r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
			got := ctxLines(r)[1:5]
			want := []string{
				"correction p/s.jsonl:9: fix n2",
				"correction p/s.jsonl:3: fix n1",
				"correction p/s.jsonl:12: fix u2 (dup unassessed: cwd gone)",
				"correction p/s.jsonl:6: fix u1 (dup unassessed: cwd gone)",
			}
			if !slices.Equal(got, want) {
				t.Fatalf("lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			// A removed worktree is coverage, never an incomplete run.
			if !r.report.Complete || r.report.Counts.DupUnassessedLookup != 2 {
				t.Fatalf("%+v complete %v", r.report.Counts, r.report.Complete)
			}
		},
	)

	t.Run("an empty healthy week", func(t *testing.T) {
		w := newWorld(t)
		w.write()
		r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		want := name + " (" + jev.Pin + "): 0 owner replies: 0 qualified (0 new, 0 duplicate, " +
			"0 dup unassessed), 0 unqualified, 0 unresolved, 0 unevaluated; report " + reportDisplay
		if r.resp.Context != want || r.resp.Status != jev.StatusPass || !r.report.Complete ||
			r.report.Counts.Items != 0 || r.report.Decisions[Qualification] != "enabled" {
			t.Fatalf("context %q\nreport %+v", r.resp.Context, r.report)
		}
	})

	t.Run("an empty week with observe's T3 source failed", func(t *testing.T) {
		w := newWorld(t)
		w.sources["t3"] = &ObserveSource{Status: "fail", Error: "sqlite3 failed"}
		w.write()
		r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if !r.report.Complete || r.report.Sources["t3"].Status != "fail" ||
			!strings.Contains(r.resp.Context, "observe source t3 fail: sqlite3 failed") {
			t.Fatalf("context %q report %+v", r.resp.Context, r.report)
		}
	})

	t.Run("an empty week with qualification disabled and a failed source", func(t *testing.T) {
		w := newWorld(t)
		w.sources["transcripts"] = &ObserveSource{Status: "timeout", Error: "deadline passed"}
		w.write()
		r := w.run(t, Env{
			Transport: &fake{p: byPrefix}, Decisions: ParseTestCutoffs("duplicate=0.5"),
		}, w.request())
		if !r.report.Complete || r.report.Decisions[Qualification] != "disabled" ||
			!strings.Contains(r.resp.Context, "observe source transcripts timeout") ||
			!strings.Contains(r.resp.Context, "qualification is disabled") {
			t.Fatalf("context %q report %+v", r.resp.Context, r.report)
		}
	})

	t.Run("an all-unresolved week is complete with its reasons", func(t *testing.T) {
		w := newWorld(t)
		for i := range 3 {
			w.items = append(w.items, ObserveItem{
				ID: fmt.Sprintf("gone%d", i), Signal: signalOwnerReply,
				Where: fmt.Sprintf("p/none%d.jsonl:2", i), Excerpt: "x", TS: iso(testNow),
			})
		}
		w.write()
		f := &fake{p: byPrefix}
		r := w.run(t, Env{Transport: f}, w.request())
		if !r.report.Complete || r.report.Counts.Unresolved != 3 || len(f.sent()) != 0 ||
			!strings.Contains(r.resp.Context, "unresolved: 3 (source gone 3)") ||
			r.resp.Status != jev.StatusPass {
			t.Fatalf("context %q report %+v", r.resp.Context, r.report.Counts)
		}
	})

	const dupOff = "the duplicate check is disabled: disabled in this test build"
	const suffix = "; candidates are not checked against memory"
	noDup := ParseTestCutoffs(Qualification + "=0.5")

	t.Run(
		"with the duplicate check disabled every candidate is listed unchecked",
		func(t *testing.T) {
			w := newWorld(t)
			w.mkdir("work")
			w.turn("p/s.jsonl", "work", "task", "done", "fix known")
			w.turn("p/s.jsonl", "work", "task", "done", "fix new")
			w.write()
			w.memory("work", "- fix known\n")
			f := &fake{p: byPrefix}
			r := w.run(t, Env{Transport: f, Decisions: noDup}, w.request())
			want := []string{
				name + " (" + jev.Pin + "): 2 owner replies: 2 qualified (0 new, 0 duplicate, 2 dup " +
					"unassessed), 0 unqualified, 0 unresolved, 0 unevaluated; report " + reportDisplay,
				"correction p/s.jsonl:6: fix new (dup unassessed: duplicate check disabled)",
				"correction p/s.jsonl:3: fix known (dup unassessed: duplicate check disabled)",
				dupOff + suffix,
			}
			if got := ctxLines(r); !slices.Equal(got, want) {
				t.Fatalf("lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for _, b := range f.sent() {
				if strings.Contains(string(b), `"memory_index"`) {
					t.Fatalf("a dup_ request was sent: %s", b)
				}
			}
			if !r.report.Complete || r.report.Counts.DupUnassessed != 2 {
				t.Fatalf("%+v complete %v", r.report.Counts, r.report.Complete)
			}
		},
	)

	for _, tc := range []struct {
		name string
		ds   map[string]jev.Decision
		want []string
	}{
		{"an empty week with the duplicate check disabled", noDup, []string{dupOff + suffix}},
		{
			"an empty week with both decisions disabled", ParseTestCutoffs("none"),
			[]string{"qualification is disabled: disabled in this test build", dupOff},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.write()
			r := w.run(t, Env{Transport: &fake{p: byPrefix}, Decisions: tc.ds}, w.request())
			if got := ctxLines(r)[1:]; !slices.Equal(got, tc.want) || !r.report.Complete {
				t.Fatalf("lines %q, want %q (complete %v)", got, tc.want, r.report.Complete)
			}
		})
	}

	t.Run(
		"with the duplicate check disabled a failed kind_ request is not done",
		func(t *testing.T) {
			w := newWorld(t)
			w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
			w.write()
			p := byPrefix
			p.drop = func(string) bool { return true }
			r := w.run(t, Env{Transport: &fake{p: p}, Decisions: noDup}, w.request())
			if r.report.Complete ||
				!strings.Contains(
					r.resp.Context,
					dupOff+suffix+"\ncomplete: false: 1 items not done "+
						"(malformed answer 1)",
				) {
				t.Fatalf("context %q", r.resp.Context)
			}
		},
	)

	t.Run("an incomplete run names the items not done and why", func(t *testing.T) {
		w := newWorld(t)
		w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
		w.write()
		p := byPrefix
		p.drop = func(string) bool { return true }
		r := w.run(t, Env{Transport: &fake{p: p}}, w.request())
		if r.report.Complete ||
			!strings.Contains(
				r.resp.Context,
				"complete: false: 1 items not done (malformed answer 1)",
			) {
			t.Fatalf("context %q", r.resp.Context)
		}
	})
}
