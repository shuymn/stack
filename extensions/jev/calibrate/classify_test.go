package calibrate

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/classify"
)

const classifyFixtures = "../../../fixtures/jev/classify"

// copyClassify copies the cal corpus into a temporary directory.
func copyClassify(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "cal"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(classifyFixtures, "cal", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no cal fixtures (%v)", err)
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dst, "cal", filepath.Base(p))
		if err := os.WriteFile(out, data, 0o600); err != nil { //nolint:gosec // G703: a temp copy
			t.Fatal(err)
		}
	}
	return dst
}

// itemKey identifies an item by what is sent for it.
func itemKey(owner, reply string) string { return owner + "\x00" + reply }

// labelIndex maps each item's sent (owner, reply) to its label by running every fixture once with
// a transport that answers nothing useful and learns the bodies and the r<n> IDs.
func labelIndex(t *testing.T, dir string) map[string]ItemLabel {
	t.Helper()
	cases, err := LoadClassify(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ItemLabel{}
	zero := 0.0
	for _, c := range cases {
		var sent map[string]string
		spy := &spyTransport{}
		ds := map[string]jev.Decision{
			classify.Qualification: {Enabled: true, Cutoff: &zero},
			classify.Duplicate:     {Reason: "off"},
		}
		o := ClassifyOptions{Runner: proc.Exec{}, Environ: cleanEnv()}
		if _, _, err := classifyRun(t.Context(), o, c, ds, spy, func(m map[string]string) {
			sent = m
		}); err != nil {
			t.Fatal(err)
		}
		byID := map[string]string{}
		for id, r := range sent {
			byID[r] = id
		}
		for _, b := range spy.bodies {
			for _, it := range decodeItems(b) {
				out[itemKey(it.Owner, it.Reply)] = c.Label.Items[byID[it.ID]]
			}
		}
	}
	if len(out) < 70 {
		t.Fatalf("only %d items indexed", len(out))
	}
	return out
}

type bodyItem struct {
	ID        string `json:"id"`
	Owner     string `json:"owner"`
	Assistant string `json:"assistant"`
	Reply     string `json:"reply"`
}

type bodyDoc struct {
	State struct {
		Items       []bodyItem `json:"items"`
		MemoryIndex string     `json:"memory_index"` //nolint:tagliatelle // the state key
		Owner       string     `json:"owner"`
		Reply       string     `json:"reply"`
	} `json:"state"`
}

func decodeItems(body []byte) []bodyItem {
	var d bodyDoc
	_ = json.Unmarshal(body, &d)
	return d.State.Items
}

// spyTransport collects bodies and answers every question with an unrecorded outcome.
type spyTransport struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (s *spyTransport) Send(_ context.Context, b jev.Batch) jev.Outcome {
	s.mu.Lock()
	s.bodies = append(s.bodies, b.Body)
	s.mu.Unlock()
	return jev.Outcome{Class: jev.Transient, Cause: jev.CauseNetwork}
}

// labelJev answers every body from the item labels: a correcting item scores high on kind_, a
// duplicate high on dup_. override changes the labels of named replies; missing leaves a
// question unanswered.
type labelJev struct {
	labels map[string]ItemLabel
	// qualify makes these replies qualify whatever their label.
	qualify map[string]bool
	// miss drops the kind_ answer of these replies.
	miss map[string]bool
	// missDupLabel makes correcting items with this dup label score as the opposite on kind_.
	hideDup string
	// kindScore sets the qualification score of these replies on every call.
	kindScore map[string]float64
	// dupScore sets the dup_ score of these replies on every call.
	dupScore map[string]float64
	// tooLarge answers 413 to a kind_ body carrying any of these replies.
	tooLarge map[string]bool
	mu       sync.Mutex
	sent     map[string]int
}

func (f *labelJev) Send(_ context.Context, b jev.Batch) jev.Outcome {
	f.mu.Lock()
	if f.sent == nil {
		f.sent = map[string]int{}
	}
	f.sent[b.Hash]++
	f.mu.Unlock()
	var d bodyDoc
	_ = json.Unmarshal(b.Body, &d)
	byID := map[string]bodyItem{}
	for _, it := range d.State.Items {
		byID[it.ID] = it
	}
	for _, it := range d.State.Items {
		if f.tooLarge[it.Reply] {
			return jev.Outcome{Class: jev.TooLarge, Status: 413}
		}
	}
	answers := map[string]any{}
	for _, q := range b.Questions {
		switch {
		case strings.HasPrefix(q.Name, "kind_"):
			it := byID[strings.TrimPrefix(q.Name, "kind_")]
			l := f.labels[itemKey(it.Owner, it.Reply)]
			if f.miss[it.Reply] {
				continue
			}
			high := l.Correcting() && l.Dup != f.hideDup || f.qualify[it.Reply]
			p := map[string]float64{}
			for _, x := range classify.Labels {
				p[x] = 0.025
			}
			choice := classify.LabelNoise
			switch score, ok := f.kindScore[it.Reply]; {
			case ok:
				choice = classify.LabelCorrection
				p[choice] = score - p[classify.LabelDrift]
			case high:
				choice = classify.LabelCorrection
				p[choice] = 0.9
			default:
				p[choice] = 0.9
			}
			answers[q.Name] = map[string]any{
				"type": "choice", "choice": choice, "confidence": 0.9, "probabilities": p,
			}
		default:
			l := f.labels[itemKey(d.State.Owner, d.State.Reply)]
			s := 0.1
			if l.Dup == classify.DupDuplicate {
				s = 0.9
			}
			if v, ok := f.dupScore[d.State.Reply]; ok {
				s = v
			}
			answers[q.Name] = map[string]any{"type": "noul", "noul": s}
		}
	}
	body, _ := jev.Marshal(map[string]any{
		"model": jev.Pin, "answers": answers, "usage": map[string]int{"input_tokens": 5},
	})
	o := jev.Validate(body, b.Questions)
	o.Status, o.Body = 200, body
	return o
}

// replies returns the replies of the items of split whose label pred accepts, in case order.
func replies(
	t *testing.T, dir string, _ map[string]ItemLabel, split string, pred func(ItemLabel) bool,
) []string {
	t.Helper()
	cases, err := LoadClassify(dir)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	var out []string
	for _, c := range cases {
		if c.Label.Split != split {
			continue
		}
		var sent map[string]string
		spy := &spyTransport{}
		ds := map[string]jev.Decision{
			classify.Qualification: {Enabled: true, Cutoff: &zero},
			classify.Duplicate:     {Reason: "off"},
		}
		o := ClassifyOptions{Runner: proc.Exec{}, Environ: cleanEnv()}
		if _, _, err := classifyRun(t.Context(), o, c, ds, spy, func(m map[string]string) {
			sent = m
		}); err != nil {
			t.Fatal(err)
		}
		reply := map[string]string{}
		for _, b := range spy.bodies {
			for _, it := range decodeItems(b) {
				reply[it.ID] = it.Reply
			}
		}
		for _, id := range slices.Sorted(maps.Keys(c.Label.Items)) {
			if pred(c.Label.Items[id]) {
				out = append(out, reply[sent[id]])
			}
		}
	}
	return out
}

// verify runs the pinned check over a calibrated corpus and its table.
func verify(t *testing.T, dir string) {
	t.Helper()
	cases, err := LoadClassify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyClassify(
		t.Context(), proc.Exec{}, cleanEnv(), cases, readTable(t, dir),
	); err != nil {
		t.Fatalf("the pinned check: %v", err)
	}
}

func runClassify(t *testing.T, dir string, f jev.Transport) (string, error) {
	t.Helper()
	var out strings.Builder
	err := RunClassify(t.Context(), ClassifyOptions{
		Fixtures: dir, Table: filepath.Join(dir, "cutoffs.json"), Transport: f,
		Runner: proc.Exec{}, Environ: cleanEnv(), Stdout: &out,
	})
	return out.String(), err
}

func readTable(t *testing.T, dir string) jev.Table {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "cutoffs.json"))
	if err != nil {
		t.Fatal(err)
	}
	tb, err := jev.ParseTable(data)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// TestClassifyCorpus is C10's corpus check: the shipped corpus meets every count, and a corpus one
// case short in each count, or with a split-spanning group, fails.
func TestClassifyCorpus(t *testing.T) {
	cases, err := LoadClassify(classifyFixtures)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckClassifyCorpus(cases); err != nil {
		t.Fatalf("the shipped corpus: %v", err)
	}
	// clone deep-copies the labels.
	clone := func() []ClassifyCase {
		out := make([]ClassifyCase, len(cases))
		for i, c := range cases {
			out[i] = c
			out[i].Label.Items = maps.Clone(c.Label.Items)
		}
		return out
	}
	find := func(cs []ClassifyCase, pred func(ClassifyCase, string, ItemLabel) bool) (int, string) {
		for i, c := range cs {
			for _, id := range slices.Sorted(maps.Keys(c.Label.Items)) {
				if pred(c, id, c.Label.Items[id]) {
					return i, id
				}
			}
		}
		t.Fatal("no such item")
		return 0, ""
	}
	held := func(c ClassifyCase, _ string, _ ItemLabel) bool { return c.Label.Split == SplitHeldout }
	for _, m := range []struct {
		name string
		want string
		do   func(cs []ClassifyCase)
	}{
		{"one heldout correction short", "heldout has 5 corrections", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.Kind == classify.LabelCorrection && !l.Overlap
			})
			delete(cs[i].Label.Items, id)
		}},
		{"one heldout duplicate short", "heldout has", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.Dup == classify.DupDuplicate
			})
			l := cs[i].Label.Items[id]
			l.Dup = classify.DupNew
			cs[i].Label.Items[id] = l
		}},
		{"one heldout request short", "heldout has", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.Kind == classify.LabelRequest && l.Group == ""
			})
			delete(cs[i].Label.Items, id)
		}},
		{"a heldout language off", "heldout languages", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.Kind == classify.LabelNoise && l.Lang == "en"
			})
			l := cs[i].Label.Items[id]
			l.Lang = "ja"
			cs[i].Label.Items[id] = l
		}},
		{"a fit class short", "fit has", func(cs []ClassifyCase) {
			for i, c := range cs {
				if c.Label.Split != SplitFit {
					continue
				}
				for id, l := range c.Label.Items {
					if l.Overlap {
						delete(cs[i].Label.Items, id)
					}
				}
			}
		}},
		{"too few hard items", "hard", func(cs []ClassifyCase) {
			for i, c := range cs {
				for id, l := range c.Label.Items {
					l.Hard = false
					cs[i].Label.Items[id] = l
				}
			}
		}},
		{"a group spanning splits", "spans", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return c.Label.Split == SplitFit && l.Group == "pair-1"
			})
			j, id2 := find(cs, held)
			l := cs[i].Label.Items[id]
			cs[j].Label.Items[id2] = ItemLabel{
				Kind: classify.LabelNoise, Lang: "en", Why: "x", Group: l.Group,
			}
		}},
		{"fewer than four pairs", "antecedent pairs", func(cs []ClassifyCase) {
			for i, c := range cs {
				for id, l := range c.Label.Items {
					l.Group = ""
					cs[i].Label.Items[id] = l
				}
			}
		}},
		{"a case without its why", "no why", func(cs []ClassifyCase) {
			i, id := find(cs, held)
			l := cs[i].Label.Items[id]
			l.Why = ""
			cs[i].Label.Items[id] = l
		}},
	} {
		t.Run(m.name, func(t *testing.T) {
			cs := clone()
			m.do(cs)
			err := CheckClassifyCorpus(cs)
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("err %v, want %q", err, m.want)
			}
		})
	}
}

// TestClassify is C10 for classify, against a fake endpoint that answers by label.
//
//nolint:gocyclo,maintidx // one run, then each of its effects checked in turn
func TestClassify(t *testing.T) {
	probe := copyClassify(t)
	labels := labelIndex(t, probe)

	t.Run("records the kind_ bodies and the dup_ union, refits and refreshes", func(t *testing.T) {
		dir := copyClassify(t)
		noiseReply := ""
		for _, k := range slices.Sorted(maps.Keys(labels)) {
			if labels[k].Kind == classify.LabelNoise && labels[k].Lang == "en" {
				noiseReply = strings.SplitN(k, "\x00", 2)[1]
				break
			}
		}
		f := &labelJev{labels: labels, qualify: map[string]bool{noiseReply: true}}
		out, err := runClassify(t, dir, f)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for h, n := range f.sent {
			if n != Attempts {
				t.Fatalf("body %s sent %d times", h, n)
			}
		}
		table := readTable(t, dir)
		q, d := table.Decisions[classify.Qualification], table.Decisions[classify.Duplicate]
		if !q.Enabled || !d.Enabled || *q.Cutoff != 0.5 || *d.Cutoff != 0.5 ||
			table.EndToEnd == nil || !table.EndToEnd.Passed || table.Placeholder != "" {
			t.Fatalf("table %+v\n%s", table, out)
		}
		// Per language, reported: 4 heldout `new` items in each.
		for _, lang := range []string{"en", "ja"} {
			if !strings.Contains(out, "  "+lang+": TP 4 FP 0 FN 0, precision 1.00, recall 1.00, "+
				"support 4\n") {
				t.Fatalf("no %s end-to-end line:\n%s", lang, out)
			}
		}
		cases, err := LoadClassify(dir)
		if err != nil {
			t.Fatal(err)
		}
		dups, kinds := 0, 0
		for _, c := range cases {
			if c.Fixture.Pending() || c.Fixture.Expect == nil || c.Fixture.Want == nil ||
				len(c.Fixture.Want.Report) == 0 {
				t.Fatalf("%s: not recorded or refreshed", c.Path)
			}
			rec, err := c.Fixture.RequestRecords()
			if err != nil {
				t.Fatal(err)
			}
			for h, r := range rec.Records {
				if len(c.Fixture.Samples[h]) == 0 {
					t.Fatalf("%s: no samples for %s", c.Path, h)
				}
				body, _ := json.Marshal(r.Answer)
				if strings.Contains(string(body), `"dup_`) {
					dups++
				} else {
					kinds++
				}
			}
			var run struct {
				Run struct {
					ID string `json:"id"`
				} `json:"run"`
			}
			if json.Unmarshal(c.Fixture.Want.Report, &run) != nil || run.Run.ID != "" {
				t.Fatalf("%s: the run ID is not normalised", c.Path)
			}
		}
		// The recorded union is what the rebuilt plan asks, and the table is the refit of the samples.
		verify(t, dir)
		for name, mut := range map[string]func(tb *jev.Table){
			"altered end-to-end figures": func(tb *jev.Table) { tb.EndToEnd.TP++ },
			"a flipped enabled bit": func(tb *jev.Table) {
				d := tb.Decisions[classify.Duplicate]
				d.Enabled = !d.Enabled
				tb.Decisions[classify.Duplicate] = d
			},
			"an added acceptance": func(tb *jev.Table) {
				tb.Accepted = &jev.Acceptance{Pin: jev.Pin, Reason: "x"}
			},
		} {
			tb := readTable(t, dir)
			mut(&tb)
			if err := VerifyClassify(t.Context(), proc.Exec{}, cleanEnv(), cases, tb); err == nil {
				t.Fatalf("the pinned check accepted %s", name)
			}
		}
		// Each correcting item has one dup_ request, plus the unlabelled qualified one.
		want := 0
		for _, c := range cases {
			for _, l := range c.Label.Items {
				if l.Correcting() {
					want++
				}
			}
		}
		if dups != want+1 || kinds == 0 {
			t.Fatalf(
				"%d dup_ records, want %d (the correcting items and the unlabelled one that qualified); %d kind_",
				dups,
				want+1,
				kinds,
			)
		}
	})

	// hideNew: qualification finds the heldout duplicates and misses the new items, so both
	// decisions pass their own gates and the end-to-end `new` list is empty.
	t.Run("an empty new list ships qualification disabled", func(t *testing.T) {
		dir := copyClassify(t)
		// A fit negative qualifies at the fitted cutoff only: its dup_ body is in the union.
		negative := replies(t, dir, labels, SplitFit, func(l ItemLabel) bool {
			return l.Kind == classify.LabelNoise
		})[0]
		fake := func() *labelJev {
			return &labelJev{
				labels: labels, hideDup: classify.DupNew,
				kindScore: map[string]float64{negative: 0.6},
			}
		}
		out, err := runClassify(t, dir, fake())
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		verify(t, dir)
		tb := readTable(t, dir)
		q := tb.Decisions[classify.Qualification]
		if q.Enabled || !strings.Contains(q.Reason, "REVIEW") || tb.EndToEnd == nil ||
			tb.EndToEnd.Passed || tb.EndToEnd.TP != 0 || !strings.Contains(out, "REVIEW") {
			t.Fatalf("qualification %+v e2e %+v\n%s", q, tb.EndToEnd, out)
		}
		// Its own gate passed: precision 1.0 and recall 0.5 on heldout, and the dup decision too.
		if q.Heldout.Precision != 1 || q.Heldout.Recall != 0.5 ||
			!tb.Decisions[classify.Duplicate].Enabled {
			t.Fatalf("own gates: %+v", q.Heldout)
		}
		cases, err := LoadClassify(dir)
		if err != nil {
			t.Fatal(err)
		}
		hash, e2e := HeldoutHash(cases), *tb.EndToEnd
		accept := func(mut func(a *jev.Acceptance)) bool {
			a := &jev.Acceptance{
				Pin:     jev.Pin,
				Heldout: hash,
				Figures: e2e,
				Reason:  "the owner reviewed it",
			}
			mut(a)
			tb2 := tb
			tb2.Accepted = a
			if err := jev.WriteTable(filepath.Join(dir, "cutoffs.json"), tb2); err != nil {
				t.Fatal(err)
			}
			if _, err := runClassify(t, dir, fake()); err != nil {
				t.Fatal(err)
			}
			return readTable(t, dir).Decisions[classify.Qualification].Enabled
		}
		if !accept(func(*jev.Acceptance) {}) {
			t.Fatal("a matching acceptance did not enable qualification")
		}
		for name, mut := range map[string]func(a *jev.Acceptance){
			"a changed pin":    func(a *jev.Acceptance) { a.Pin = "jev-0.0.0" },
			"a changed hash":   func(a *jev.Acceptance) { a.Heldout = "0000" },
			"changed figures":  func(a *jev.Acceptance) { a.Figures.Support++ },
			"no reason":        func(a *jev.Acceptance) { a.Reason = "" },
			"a changed recall": func(a *jev.Acceptance) { a.Figures.Recall = 0.75 },
		} {
			if accept(mut) {
				t.Fatalf("%s did not void the acceptance", name)
			}
		}
	})

	// Qualification finds the heldout new items and misses the duplicates, so it passes its own
	// gate; the duplicate decision catches 3 of the 8 heldout duplicates and fails its own. The
	// runtime then lists no `new` item, whatever the end-to-end score at both cutoffs says.
	t.Run("a duplicate decision failing its own gate keeps qualification off", func(t *testing.T) {
		dir := copyClassify(t)
		missed := replies(t, dir, labels, SplitHeldout, func(l ItemLabel) bool {
			return l.Correcting() && l.Dup == classify.DupDuplicate
		})[3:]
		dupScore := map[string]float64{}
		for _, r := range missed {
			dupScore[r] = 0.1
		}
		out, err := runClassify(t, dir, &labelJev{
			labels: labels, hideDup: classify.DupDuplicate, dupScore: dupScore,
		})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tb := readTable(t, dir)
		q, d := tb.Decisions[classify.Qualification], tb.Decisions[classify.Duplicate]
		if d.Enabled || d.Heldout.Recall >= 0.5 || q.Heldout.Precision != 1 {
			t.Fatalf("own gates: qualification %+v, duplicate %+v", q.Heldout, d.Heldout)
		}
		if q.Enabled || tb.EndToEnd == nil || tb.EndToEnd.Passed ||
			!strings.Contains(q.Reason, "REVIEW") {
			t.Fatalf("qualification %+v e2e %+v\n%s", q, tb.EndToEnd, out)
		}
		// Unscored, the list is empty: every labelled new item is a miss, its support kept.
		if e := *tb.EndToEnd; e.Scored || e.Support != 8 || e.FN != 8 || e.TP != 0 ||
			!strings.Contains(
				out,
				"  ja: TP 0 FP 0 FN 4, precision 0.00, recall 0.00, support 4\n",
			) {
			t.Fatalf("e2e %+v\n%s", e, out)
		}
		verify(t, dir)
	})

	t.Run("a kind_ body answered 413 still records its correcting items' dup_", func(t *testing.T) {
		dir := copyClassify(t)
		big := replies(t, dir, labels, SplitFit, ItemLabel.Correcting)[0]
		f := &labelJev{labels: labels, tooLarge: map[string]bool{big: true}}
		if out, err := runClassify(t, dir, f); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		cases, err := LoadClassify(dir)
		if err != nil {
			t.Fatal(err)
		}
		dups, want, large := 0, 0, 0
		for _, c := range cases {
			for _, l := range c.Label.Items {
				if l.Correcting() {
					want++
				}
			}
			rec, err := c.Fixture.RequestRecords()
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rec.Records {
				body, _ := json.Marshal(r.Answer)
				switch {
				case r.Status == 413:
					large++
				case strings.Contains(string(body), `"dup_`):
					dups++
				}
			}
		}
		if large == 0 || dups != want {
			t.Fatalf("%d 413 records; %d dup_ records, want one per correcting item (%d)", large,
				dups, want)
		}
		verify(t, dir)
	})

	t.Run("an unreadable report fails before anything is written", func(t *testing.T) {
		dir := copyClassify(t)
		cases, err := LoadClassify(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := cases[0]
		// A file where the report's directory goes.
		c.Fixture.Files["home/.local"] = json.RawMessage(`"not a directory"`)
		if err := jev.WriteFixture(c.Path, c.Fixture); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{}
		for _, x := range cases {
			before[x.Path] = readFile(t, x.Path)
		}
		if out, err := runClassify(t, dir, &labelJev{labels: labels}); err == nil {
			t.Fatalf("calibration succeeded without a report\n%s", out)
		}
		for p, data := range before {
			if readFile(t, p) != data {
				t.Fatalf("%s was rewritten", p)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "cutoffs.json")); !os.IsNotExist(err) {
			t.Fatalf("a table was written (%v)", err)
		}
	})

	t.Run("a missing score on a true new item is a miss", func(t *testing.T) {
		dir := copyClassify(t)
		// Two English heldout new items lose their kind_ answers; Japanese keeps all four.
		miss := map[string]bool{}
		for _, r := range replies(t, dir, labels, SplitHeldout, func(l ItemLabel) bool {
			return l.Correcting() && l.Dup == classify.DupNew && l.Lang == "en"
		})[:2] {
			miss[r] = true
		}
		out, err := runClassify(t, dir, &labelJev{labels: labels, miss: miss})
		if err != nil {
			t.Fatal(err)
		}
		e := readTable(t, dir).EndToEnd
		if e == nil || e.FN != 2 || e.TP != 6 || e.Support != 8 {
			t.Fatalf("end to end %+v", e)
		}
		for _, line := range []string{
			"  en: TP 2 FP 0 FN 2, precision 1.00, recall 0.50, support 4\n",
			"  ja: TP 4 FP 0 FN 0, precision 1.00, recall 1.00, support 4\n",
		} {
			if !strings.Contains(out, line) {
				t.Fatalf("no line %q:\n%s", line, out)
			}
		}
	})

	t.Run("a corpus one case short is refused before any request", func(t *testing.T) {
		dir := copyClassify(t)
		cases, err := LoadClassify(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := cases[len(cases)-1]
		for id, l := range c.Label.Items {
			if l.Kind == classify.LabelRequest && l.Group == "" {
				delete(c.Label.Items, id)
				break
			}
		}
		label, _ := jev.Marshal(c.Label)
		c.Fixture.Label = label
		if err := jev.WriteFixture(c.Path, c.Fixture); err != nil {
			t.Fatal(err)
		}
		f := &labelJev{labels: labels}
		if _, err := runClassify(
			t,
			dir,
			f,
		); err == nil ||
			!strings.Contains(err.Error(), "not eligible") {
			t.Fatalf("err %v", err)
		}
		if len(f.sent) != 0 {
			t.Fatal("a request was sent")
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestHeldoutHashIncludesLayout: an acceptance is bound to the heldout repository layout and
// samples too.
func TestHeldoutHashIncludesLayout(t *testing.T) {
	cases := []ClassifyCase{{Label: ClassifyLabel{Split: SplitHeldout}}}
	before := HeldoutHash(cases)
	cases[0].Fixture.Repo = json.RawMessage(`{"layouts":[{"kind":"checkout","dir":"synthetic"}]}`)
	if HeldoutHash(cases) == before {
		t.Fatal("held-out layout changed without invalidating acceptance")
	}
	// A later attempt's score feeds the gates while leaving the first-answer figures alone.
	a, b := 0.9, 0.2
	cases[0].Fixture.Samples = map[string]map[string][]*float64{"h": {"kind_r1": {&a, &a, &a}}}
	before = HeldoutHash(cases)
	cases[0].Fixture.Samples["h"]["kind_r1"][2] = &b
	if HeldoutHash(cases) == before {
		t.Fatal("a held-out sample changed without invalidating acceptance")
	}
}
