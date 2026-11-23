package classify

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// TestDecision is C7 for classify: the summed probability decides at the cutoff, the larger label
// is the kind (correction on a tie), and a disabled decision sends no request but still writes
// the report.
func TestDecision(t *testing.T) {
	mk := func(c, d float64) map[string]float64 {
		return map[string]float64{LabelCorrection: c, LabelDrift: d, LabelApproval: 0.1}
	}
	for _, c := range []struct {
		name string
		p    map[string]float64
		want bool
		kind string
	}{
		{"just below the cutoff", mk(0.25, 0.2499), false, LabelCorrection},
		{"at the cutoff", mk(0.25, 0.25), true, LabelCorrection},
		{"above the cutoff", mk(0.3, 0.3), true, LabelCorrection},
		{"drift larger", mk(0.1, 0.45), true, LabelDrift},
		{"equal labels are a correction", mk(0.3, 0.3), true, LabelCorrection},
		{"an overlap that neither label reaches alone", mk(0.3, 0.25), true, LabelCorrection},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, kind := Qualify(c.p, 0.5)
			if got != c.want || kind != c.kind {
				t.Fatalf("Qualify = %v %s, want %v %s", got, kind, c.want, c.kind)
			}
		})
	}

	setup := func(t *testing.T) (*world, string) {
		t.Helper()
		w := newWorld(t)
		w.mkdir("work")
		id := w.turn("p/s.jsonl", "work", "task", "done", "fix alpha beta")
		w.write()
		w.memory("work", "- keep to alpha rules\n")
		return w, id
	}
	t.Run("qualification disabled sends nothing and writes the report", func(t *testing.T) {
		w, id := setup(t)
		f := &fake{p: byPrefix}
		r := w.run(t, Env{Transport: f, Decisions: ParseTestCutoffs("duplicate=0.5")}, w.request())
		it := r.item(t, id)
		if len(f.sent()) != 0 || it.Status != StatusUnevaluated ||
			it.Reason != ReasonClassificationDisabled ||
			r.report == nil ||
			!r.report.Complete ||
			r.report.Decisions[Qualification] != "disabled" ||
			r.resp.Status != jev.StatusPass {
			t.Fatalf("sent %d, item %+v, response %+v", len(f.sent()), it, r.resp)
		}
		if !strings.Contains(r.resp.Context, "qualification is disabled") {
			t.Fatalf("context %q", r.resp.Context)
		}
	})
	t.Run("the duplicate decision disabled sends no dup_ request", func(t *testing.T) {
		w, id := setup(t)
		f := &fake{p: byPrefix}
		r := w.run(
			t,
			Env{Transport: f, Decisions: ParseTestCutoffs("qualification=0.5")},
			w.request(),
		)
		it := r.item(t, id)
		if it.Duplicate != DupUnassessed || it.DupReason != ReasonDupDisabled || !it.Done() {
			t.Fatalf("%+v", it)
		}
		for _, b := range f.sent() {
			if strings.Contains(string(b), `"dup_`) {
				t.Fatalf("a dup_ request was sent: %s", b)
			}
		}
		if !r.report.Complete || r.report.Decisions[Duplicate] != "disabled" ||
			!strings.Contains(r.resp.Context, "the duplicate check is disabled") {
			t.Fatalf("report %+v, context %q", r.report, r.resp.Context)
		}
	})
	t.Run("both enabled", func(t *testing.T) {
		w, id := setup(t)
		r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		it := r.item(t, id)
		if it.Duplicate != DupDuplicate || it.Kind != LabelCorrection || !it.Qualified ||
			it.DupP == nil || *it.DupP != 0.9 || it.Excerpt != "fix alpha beta" {
			t.Fatalf("%+v", it)
		}
		// The report keeps the raw kind_ answer: its choice and every label's probability.
		if it.Choice != LabelCorrection || len(it.Probabilities) != len(Labels) ||
			it.Probabilities[LabelCorrection] != 0.9 {
			t.Fatalf("choice %q probabilities %v", it.Choice, it.Probabilities)
		}
	})
}

// TestState is C9's state building: the units are sent newest first under r<n> IDs whatever
// order observe listed them in, the bodies carry no observe ID or local path and are the same
// under another root, and an embedded path in a reply is sent as written.
func TestState(t *testing.T) {
	build := func(t *testing.T) (*world, map[string]string) {
		t.Helper()
		w := newWorld(t)
		w.mkdir("work")
		ids := map[string]string{}
		// Observe lists the items oldest first; sent order is newest first.
		for _, r := range []string{"fix oldest", "ok middle", "back newest see /srv/app/main.go"} {
			ids[r] = w.turn("p/s.jsonl", "work", "task "+r, "done "+r, r)
		}
		w.write()
		return w, ids
	}
	run := func(t *testing.T) (*world, map[string]string, [][]byte) {
		t.Helper()
		w, ids := build(t)
		f := &fake{p: byPrefix}
		w.run(t, Env{Transport: f}, w.request())
		return w, ids, f.sent()
	}
	w, ids, bodies := run(t)
	var kind []byte
	for _, b := range bodies {
		if strings.Contains(string(b), `"kind_r1"`) {
			kind = b
		}
	}
	s := decodeBody(kind)
	order := make([]string, 0, len(s.State.Items))
	got := make([]string, 0, len(s.State.Items))
	for _, it := range s.State.Items {
		order = append(order, it.ID)
		got = append(got, it.Reply)
	}
	want := []string{"back newest see /srv/app/main.go", "ok middle", "fix oldest"}
	if !slices.Equal(order, []string{"r1", "r2", "r3"}) || !slices.Equal(got, want) {
		t.Fatalf("ids %v replies %q", order, got)
	}
	for _, b := range bodies {
		for id := range ids {
			_ = id
		}
		for _, it := range w.items {
			if strings.Contains(string(b), it.ID) {
				t.Fatalf("an observe ID %s is in a body: %s", it.ID, b)
			}
		}
		if strings.Contains(string(b), filepath.ToSlash(w.root)) ||
			strings.Contains(string(b), w.root) {
			t.Fatalf("a local root is in a body: %s", b)
		}
	}
	_, _, again := run(t)
	if len(again) != len(bodies) {
		t.Fatalf("%d bodies under another root, %d here", len(again), len(bodies))
	}
	for i := range bodies {
		if jev.Hash(bodies[i]) != jev.Hash(again[i]) {
			t.Fatalf("body %d differs under another root:\n%s\n%s", i, bodies[i], again[i])
		}
	}
	if !strings.Contains(string(kind), "/srv/app/main.go") {
		t.Fatal("an embedded path in a reply was cut")
	}
}

// TestPhases is C9's group order: over two groups, each phase sends the exact bodies of its
// requests and starts only after the previous phase's requests all returned; a dup_ body carries
// one qualified reply; no body is attempted twice.
func TestPhases(t *testing.T) {
	w := newWorld(t)
	w.mkdir("work")
	// 10 replies of 30 KiB: one to a request, so ten kind_ requests in groups of 4, 4 and 2.
	pad := strings.Repeat("p ", 15<<10)
	for i := range 10 {
		first := "fix"
		if i%2 == 1 {
			first = "ok"
		}
		w.turn("p/s.jsonl", "work", "task", "done", fmt.Sprintf("%s w%d %s", first, i, pad))
	}
	w.write()
	w.memory("work", "- w0 w2 w4 w6 w8 are known\n")
	f := &fake{p: byPrefix, gate: func(jev.Batch) { time.Sleep(15 * time.Millisecond) }}
	w.run(t, Env{Transport: f}, w.request())

	events := f.events()
	isKind := func(e event) bool { return strings.Contains(string(e.body), `"kind_r`) }
	var phases [][]event
	for _, e := range events {
		if len(phases) == 0 || isKind(phases[len(phases)-1][0]) != isKind(e) {
			phases = append(phases, nil)
		}
		phases[len(phases)-1] = append(phases[len(phases)-1], e)
	}
	// Order within a phase is free, but the phases alternate kind, dup, kind, dup.
	wantKinds := []bool{true, false, true, false, true, false}
	if len(phases) != len(wantKinds) {
		t.Fatalf("%d phases: %v", len(phases), kinds(events, isKind))
	}
	sizes := []int{4, 2, 4, 2, 2, 1}
	for i, p := range phases {
		if isKind(p[0]) != wantKinds[i] || len(p) != sizes[i] {
			t.Fatalf("phase %d: %d requests, kind %v; sequence %v", i, len(p), isKind(p[0]),
				kinds(events, isKind))
		}
		if i > 0 {
			var lastEnd int
			for _, e := range phases[i-1] {
				lastEnd = max(lastEnd, e.end)
			}
			for _, e := range p {
				if e.start < lastEnd {
					t.Fatalf(
						"phase %d started (%d) before phase %d ended (%d)",
						i,
						e.start,
						i-1,
						lastEnd,
					)
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, e := range events {
		h := jev.Hash(e.body)
		if seen[h] {
			t.Fatalf("a body was attempted twice")
		}
		seen[h] = true
	}
	// Each dup_ body is one qualified reply with its owner, assistant and the index.
	for _, p := range []int{1, 3, 5} {
		for _, e := range phases[p] {
			s := decodeBody(e.body)
			if len(s.Questions) != 1 || len(s.State.Items) != 0 || s.State.MemoryIndex == "" ||
				s.State.Owner != "task" || s.State.Assistant != "done" ||
				!strings.HasPrefix(s.State.Reply, "fix w") {
				t.Fatalf("dup_ body %s", e.body)
			}
		}
	}
	// Each phase holds exactly its replies, newest first by group: w9-w6, then w5-w2, then w1-w0.
	want := [][]string{
		{"w6", "w7", "w8", "w9"},
		{"w6", "w8"},
		{"w2", "w3", "w4", "w5"},
		{"w2", "w4"},
		{"w0", "w1"},
		{"w0"},
	}
	for i, p := range phases {
		var got []string
		for _, e := range p {
			s := decodeBody(e.body)
			replies := []string{s.State.Reply}
			if isKind(e) {
				replies = replies[:0]
				for _, it := range s.State.Items {
					replies = append(replies, it.Reply)
				}
			}
			for _, r := range replies {
				got = append(got, strings.Fields(r)[1])
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want[i]) {
			t.Fatalf("phase %d holds %v, want %v", i, got, want[i])
		}
	}
}

func kinds(events []event, isKind func(event) bool) string {
	var b strings.Builder
	for _, e := range events {
		if isKind(e) {
			b.WriteByte('K')
		} else {
			b.WriteByte('D')
		}
	}
	return b.String()
}
