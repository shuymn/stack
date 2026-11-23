package calibrate

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	// firstLow makes these replies score as noise on the first call of their kind_ body only.
	firstLow map[string]bool
	// firstOnly makes these replies score as a correction on the first call of their kind_ body
	// only.
	firstOnly map[string]bool
	// dupMiss drops the dup_ answer of these replies on the attempts listed (1-based; 0: every
	// attempt).
	dupMiss map[string]int
	mu      sync.Mutex
	sent    map[string]int
}

func (f *labelJev) Send(_ context.Context, b jev.Batch) jev.Outcome {
	f.mu.Lock()
	if f.sent == nil {
		f.sent = map[string]int{}
	}
	f.sent[b.Hash]++
	attempt := f.sent[b.Hash]
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
			high := (l.Correcting() && l.Dup != f.hideDup || f.qualify[it.Reply] ||
				f.firstOnly[it.Reply] && attempt == 1) &&
				(!f.firstLow[it.Reply] || attempt != 1)
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
			if n, ok := f.dupMiss[d.State.Reply]; ok && (n == 0 || n == attempt) {
				continue
			}
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
// case short in each count, off its heldout dup_kind, undo or pair quotas, with a dup_kind off its
// dup or hard label, or with a split-spanning group, fails.
//
//nolint:gocyclo,maintidx // one mutation per corpus rule
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
	set := func(cs []ClassifyCase, i int, id string, mut func(l *ItemLabel)) {
		l := cs[i].Label.Items[id]
		mut(&l)
		cs[i].Label.Items[id] = l
	}
	// The fit split alone meets the pair floor, so a heldout pair check is not the floor's.
	fitPairs := map[string]bool{}
	for _, c := range cases {
		for _, l := range c.Label.Items {
			if c.Label.Split == SplitFit && strings.HasPrefix(l.Group, "pair-") {
				fitPairs[l.Group] = true
			}
		}
	}
	if len(fitPairs) < minPairs {
		t.Fatalf("fit has %d pairs", len(fitPairs))
	}
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
		{"too few hard items", "heldout has 10 hard items of 32", func(cs []ClassifyCase) {
			// Items with a dup_kind stay hard, so only the share check can fail.
			for i, c := range cs {
				for id, l := range c.Label.Items {
					if l.DupKind == "" {
						set(cs, i, id, func(l *ItemLabel) { l.Hard = false })
					}
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
		{"fewer than four pairs", "2 antecedent pairs (groups pair-*), want at least 4", func(cs []ClassifyCase) {
			// The fit pairs go; heldout keeps its en and ja pair.
			for i, c := range cs {
				for id := range c.Label.Items {
					if c.Label.Split == SplitFit {
						set(cs, i, id, func(l *ItemLabel) { l.Group = "" })
					}
				}
			}
		}},
		{"a pair with one member", "has members [drift], want one drift and one request", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.Kind == classify.LabelRequest && l.Lang == "en" &&
					strings.HasPrefix(l.Group, "pair-")
			})
			set(cs, i, id, func(l *ItemLabel) { l.Group = "" })
		}},
		{"a held-out-only dup_kind in fit", "dup_kind multi is held-out only", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return c.Label.Split == SplitFit && l.Dup == classify.DupDuplicate && l.Hard
			})
			set(cs, i, id, func(l *ItemLabel) { l.DupKind = "multi" })
		}},
		{"heldout duplicate 5 + 3 and new 3 + 5", "heldout languages: duplicate", func(cs []ClassifyCase) {
			// Correcting stays 8 + 8 by language.
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.Dup == classify.DupDuplicate && l.Lang == "ja"
			})
			j, id2 := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.Dup == classify.DupNew && l.Lang == "en"
			})
			set(cs, i, id, func(l *ItemLabel) { l.Lang = "en" })
			set(cs, j, id2, func(l *ItemLabel) { l.Lang = "ja" })
		}},
		{"a heldout hard-negative kind missing", "dup_kind object", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.DupKind == "object"
			})
			set(cs, i, id, func(l *ItemLabel) { l.DupKind = "" })
		}},
		{"a heldout positive dup_kind missing", "dup_kind paraphrase", func(cs []ClassifyCase) {
			i, id := find(cs, func(c ClassifyCase, _ string, l ItemLabel) bool {
				return held(c, "", l) && l.DupKind == "paraphrase"
			})
			set(cs, i, id, func(l *ItemLabel) { l.DupKind = "" })
		}},
		{"no heldout undo positive", "marked undo", func(cs []ClassifyCase) {
			for i, c := range cs {
				for id, l := range c.Label.Items {
					if held(c, "", l) && l.Dup == classify.DupDuplicate {
						set(cs, i, id, func(l *ItemLabel) { l.Undo = false })
					}
				}
			}
		}},
		{"a dup_kind whose polarity mismatches dup", "does not match dup", func(cs []ClassifyCase) {
			i, id := find(cs, func(_ ClassifyCase, _ string, l ItemLabel) bool {
				return l.DupKind == "object"
			})
			set(cs, i, id, func(l *ItemLabel) { l.Dup = classify.DupDuplicate })
		}},
		{"a dup_kind on an item not hard", "not hard", func(cs []ClassifyCase) {
			i, id := find(cs, func(_ ClassifyCase, _ string, l ItemLabel) bool {
				return l.DupKind == "multi"
			})
			set(cs, i, id, func(l *ItemLabel) { l.Hard = false })
		}},
		{"no antecedent pair in heldout", "heldout antecedent pairs", func(cs []ClassifyCase) {
			for i, c := range cs {
				for id, l := range c.Label.Items {
					if held(c, "", l) {
						set(cs, i, id, func(l *ItemLabel) { l.Group = "" })
					}
				}
			}
		}},
		{"two heldout pairs of one language", "want one en and one ja", func(cs []ClassifyCase) {
			// The ja pair's halves become English.
			for i, c := range cs {
				for id, l := range c.Label.Items {
					if held(c, "", l) && strings.HasPrefix(l.Group, "pair-") && l.Lang == "ja" {
						set(cs, i, id, func(l *ItemLabel) { l.Lang = "en" })
					}
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

// TestClassifyPairs is C10's pair-text check: each antecedent pair of the committed corpus sends
// one assistant message and reply under two owner turns, and a member whose assistant message
// differs fails.
func TestClassifyPairs(t *testing.T) {
	cases, err := LoadClassify(classifyFixtures)
	if err != nil {
		t.Fatal(err)
	}
	o := ClassifyOptions{Runner: proc.Exec{}, Environ: cleanEnv()}
	if err := CheckClassifyPairs(t.Context(), o, cases); err != nil {
		t.Fatalf("the shipped corpus: %v", err)
	}
	// The first assistant message of the en pair in heldout-b gains a sentence.
	for i, c := range cases {
		if c.Label.Split != SplitHeldout {
			continue
		}
		for path, raw := range c.Fixture.Files {
			var recs []map[string]any
			if json.Unmarshal(raw, &recs) != nil {
				continue
			}
			for _, r := range recs {
				msg, _ := r["message"].(map[string]any)
				parts, _ := msg["content"].([]any)
				for _, part := range parts {
					m, _ := part.(map[string]any)
					if text, _ := m["text"].(string); strings.Contains(text, "migration 0042") {
						m["text"] = text + " Done."
						data, err := json.Marshal(recs)
						if err != nil {
							t.Fatal(err)
						}
						files := maps.Clone(c.Fixture.Files)
						files[path] = data
						cases[i].Fixture.Files = files
						err = CheckClassifyPairs(t.Context(), o, cases)
						if err == nil || !strings.Contains(err.Error(), "does not share its "+
							"assistant message and reply") {
							t.Fatalf("err %v", err)
						}
						return
					}
				}
			}
		}
	}
	t.Fatal("no pair member to change")
}

// TestClassify is C10 for classify, against a fake endpoint that answers by label.
//
//nolint:gocyclo,maintidx // one run, then each of its effects checked in turn
func TestClassify(t *testing.T) {
	probe := copyClassify(t)
	labels := labelIndex(t, probe)

	t.Run("records the kind_ bodies and the dup_ union, refits and refreshes", func(t *testing.T) {
		dir := copyClassify(t)
		// A fit noise reply qualifies: its dup_ body joins the union, the heldout figures stay.
		noiseReply := replies(t, dir, labels, SplitFit, func(l ItemLabel) bool {
			return l.Kind == classify.LabelNoise && l.Lang == "en"
		})[0]
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

	// heldDup are the heldout duplicate replies; failDup makes the duplicate decision fail its own
	// gate on heldout (3 of the 8 found, recall 0.38) while fit stays as labelled.
	heldDup := replies(t, probe, labels, SplitHeldout, func(l ItemLabel) bool {
		return l.Correcting() && l.Dup == classify.DupDuplicate
	})
	failDup := func() map[string]float64 {
		m := map[string]float64{}
		for _, r := range heldDup[3:] {
			m[r] = 0.1
		}
		return m
	}
	// failQual makes qualification fail its own gate on heldout: every heldout item that is not
	// correcting qualifies too (precision 0.50).
	failQual := func() map[string]bool {
		m := map[string]bool{}
		for _, r := range replies(t, probe, labels, SplitHeldout, func(l ItemLabel) bool {
			return !l.Correcting()
		}) {
			m[r] = true
		}
		return m
	}

	t.Run(
		"each combination of the own gates fills end_to_end as the table states",
		func(t *testing.T) {
			for _, tc := range []struct {
				name           string
				fake           func() *labelJev
				list           string
				scored         bool
				support, fn    int
				qualOn, dupOn  bool
				reason, review []string
			}{
				{
					"both pass", func() *labelJev { return &labelJev{labels: labels} },
					jev.ListNew, true, 8, 0, true, true, nil, nil,
				},
				{
					"qualification passes, duplicate fails",
					func() *labelJev { return &labelJev{labels: labels, dupScore: failDup()} },
					jev.ListQualified, true, 16, 0, true, false, nil,
					[]string{"REVIEW: duplicate fails its own gate"},
				},
				{
					"qualification fails, duplicate passes",
					func() *labelJev { return &labelJev{labels: labels, qualify: failQual()} },
					jev.ListNew, false, 8, 8, false, true,
					[]string{"heldout precision 0.50"},
					[]string{"REVIEW: qualification fails its own gate"},
				},
				{
					"both fail", func() *labelJev {
						return &labelJev{labels: labels, qualify: failQual(), dupScore: failDup()}
					},
					jev.ListQualified, false, 16, 16, false, false,
					[]string{"heldout precision 0.50"},
					[]string{
						"REVIEW: qualification fails its own gate",
						"REVIEW: duplicate fails its own gate",
					},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					dir := copyClassify(t)
					out, err := runClassify(t, dir, tc.fake())
					if err != nil {
						t.Fatalf("%v\n%s", err, out)
					}
					tb := readTable(t, dir)
					q, d, e := tb.Decisions[classify.Qualification], tb.Decisions[classify.Duplicate],
						tb.EndToEnd
					if e == nil || e.List != tc.list || e.Scored != tc.scored ||
						e.Support != tc.support ||
						e.FN != tc.fn {
						t.Fatalf("e2e %+v\n%s", e, out)
					}
					if q.Enabled != tc.qualOn || d.Enabled != tc.dupOn {
						t.Fatalf(
							"enabled: qualification %v, duplicate %v\n%s",
							q.Enabled,
							d.Enabled,
							out,
						)
					}
					for _, r := range tc.reason {
						if !strings.Contains(q.Reason, r) {
							t.Fatalf("qualification reason %q, want %q", q.Reason, r)
						}
					}
					if tc.reason == nil && q.Reason != "" {
						t.Fatalf("qualification reason %q", q.Reason)
					}
					// A qualification failing its own gate keeps its reason: no end-to-end REVIEW.
					if strings.Contains(q.Reason, "end-to-end") ||
						strings.Contains(out, "REVIEW: qualification ships disabled") {
						t.Fatalf("an end-to-end REVIEW\n%s", out)
					}
					for _, r := range tc.review {
						if !strings.Contains(out, r) {
							t.Fatalf("no %q\n%s", r, out)
						}
					}
					if tc.review == nil && strings.Contains(out, "REVIEW") {
						t.Fatalf("an unexpected REVIEW\n%s", out)
					}
					if tc.list == jev.ListQualified &&
						(e.NewSupport == nil || *e.NewSupport != 8 || e.NewRecall == nil) {
						t.Fatalf("new figures %+v", e)
					}
					verify(t, dir)
				})
			}
		},
	)

	// Qualification finds all 8 heldout duplicates and misses all 8 new items: precision 1.0 and
	// recall 0.5 pass its own gate, yet the week's new rules are missed, so it ships disabled
	// whichever list is scored.
	knownOnly := func(dupScore map[string]float64) func() *labelJev {
		return func() *labelJev {
			return &labelJev{labels: labels, hideDup: classify.DupNew, dupScore: dupScore}
		}
	}
	t.Run("known corrections alone fail with the duplicate decision passing", func(t *testing.T) {
		dir := copyClassify(t)
		out, err := runClassify(t, dir, knownOnly(nil)())
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tb := readTable(t, dir)
		q, e := tb.Decisions[classify.Qualification], *tb.EndToEnd
		if q.Heldout.Precision != 1 || q.Heldout.Recall != 0.5 ||
			!tb.Decisions[classify.Duplicate].Enabled {
			t.Fatalf("own gates: %+v", q.Heldout)
		}
		if q.Enabled || e.List != jev.ListNew || e.TP != 0 || e.Passed ||
			q.Reason != "end-to-end `new` list on heldout: precision 0.00, recall 0.00, support 8 "+
				"miss the target 0.80 / 0.50; REVIEW" || !strings.Contains(out, "REVIEW") {
			t.Fatalf("qualification %+v e2e %+v\n%s", q, e, out)
		}
		// With `new`, the table has neither new-item key.
		raw := readFile(t, filepath.Join(dir, "cutoffs.json"))
		if strings.Contains(raw, `"new_support"`) || strings.Contains(raw, `"new_recall"`) {
			t.Fatalf("new-item keys with the `new` list:\n%s", raw)
		}
		verify(t, dir)
	})

	t.Run("known corrections alone fail with the duplicate decision disabled", func(t *testing.T) {
		dir := copyClassify(t)
		fake := knownOnly(failDup())
		out, err := runClassify(t, dir, fake())
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tb := readTable(t, dir)
		q, e := tb.Decisions[classify.Qualification], *tb.EndToEnd
		if tb.Decisions[classify.Duplicate].Enabled || q.Heldout.Precision != 1 ||
			q.Heldout.Recall != 0.5 {
			t.Fatalf("own gates: qualification %+v", q.Heldout)
		}
		if q.Enabled || e.List != jev.ListQualified || e.Precision != 1 || e.Recall != 0.5 ||
			e.NewRecall == nil || *e.NewRecall != 0 || e.NewSupport == nil || *e.NewSupport != 8 ||
			e.Passed {
			t.Fatalf("qualification %+v e2e %+v\n%s", q, e, out)
		}
		if want := "end-to-end `qualified` list on heldout: precision 1.00, recall 0.50, new " +
			"recall 0.00, support 16 miss the target 0.80 / 0.50; REVIEW"; q.Reason != want {
			t.Fatalf("reason %q, want %q", q.Reason, want)
		}
		// With `qualified`, the table has both new-item keys, new_recall 0 included.
		raw := readFile(t, filepath.Join(dir, "cutoffs.json"))
		if !strings.Contains(raw, `"new_support": 8`) || !strings.Contains(raw, `"new_recall": 0`) {
			t.Fatalf("no new-item keys:\n%s", raw)
		}
		verify(t, dir)

		// A recorded acceptance of these figures ships qualification enabled; new_recall 0
		// round-trips through the table and still matches.
		cases, err := LoadClassify(dir)
		if err != nil {
			t.Fatal(err)
		}
		hash := HeldoutHash(cases)
		accept := func(mut func(a *jev.Acceptance)) (bool, jev.Table) {
			t.Helper()
			fig := *readTable(t, dir).EndToEnd
			a := &jev.Acceptance{
				Pin: jev.Pin, Heldout: hash, Figures: fig, Reason: "the owner reviewed it",
			}
			mut(a)
			tb2 := tb
			tb2.Accepted = a
			if err := jev.WriteTable(filepath.Join(dir, "cutoffs.json"), tb2); err != nil {
				t.Fatal(err)
			}
			if out, err := runClassify(t, dir, fake()); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := readTable(t, dir)
			return got.Decisions[classify.Qualification].Enabled, got
		}
		on, got := accept(func(*jev.Acceptance) {})
		if !on || got.Accepted == nil || got.Decisions[classify.Duplicate].Enabled {
			t.Fatalf("a matching acceptance did not enable qualification: %+v", got)
		}
		verify(t, dir)
		for name, mut := range map[string]func(a *jev.Acceptance){
			"a changed pin":        func(a *jev.Acceptance) { a.Pin = "jev-0.0.0" },
			"a changed hash":       func(a *jev.Acceptance) { a.Heldout = "0000" },
			"a changed figure":     func(a *jev.Acceptance) { a.Figures.Support++ },
			"a changed list":       func(a *jev.Acceptance) { a.Figures.List = jev.ListNew },
			"a changed new recall": func(a *jev.Acceptance) { r := 0.25; a.Figures.NewRecall = &r },
			"no reason":            func(a *jev.Acceptance) { a.Reason = "" },
		} {
			if on, _ := accept(mut); on {
				t.Fatalf("%s did not void the acceptance", name)
			}
		}
	})

	t.Run("an acceptance never enables a qualification failing its own gate", func(t *testing.T) {
		dir := copyClassify(t)
		fake := func() *labelJev {
			return &labelJev{labels: labels, qualify: failQual(), dupScore: failDup()}
		}
		if out, err := runClassify(t, dir, fake()); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tb := readTable(t, dir)
		cases, err := LoadClassify(dir)
		if err != nil {
			t.Fatal(err)
		}
		tb.Accepted = &jev.Acceptance{
			Pin: jev.Pin, Heldout: HeldoutHash(cases), Figures: *tb.EndToEnd, Reason: "accepted",
		}
		if err := jev.WriteTable(filepath.Join(dir, "cutoffs.json"), tb); err != nil {
			t.Fatal(err)
		}
		if out, err := runClassify(t, dir, fake()); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		got := readTable(t, dir)
		if q := got.Decisions[classify.Qualification]; q.Enabled || got.Accepted != nil ||
			!strings.HasPrefix(q.Reason, "heldout precision") {
			t.Fatalf("qualification %+v, accepted %+v", q, got.Accepted)
		}
		verify(t, dir)
	})

	// heldCorrecting are the heldout correcting replies.
	heldCorrecting := replies(t, probe, labels, SplitHeldout, ItemLabel.Correcting)
	firstLow := func(rs []string) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r] = true
		}
		return m
	}
	t.Run("the own gate counts every attempt, the list the first answers", func(t *testing.T) {
		heldOthers := replies(t, probe, labels, SplitHeldout, func(l ItemLabel) bool {
			return !l.Correcting()
		})
		for _, tc := range []struct {
			name      string
			low, high []string
			reason    string
		}{
			// 9 of 16 first answers miss: own recall 39/48, the list's 7/16.
			{
				"a list under its recall", heldCorrecting[:9], nil,
				"end-to-end `qualified` list on heldout: precision 1.00, recall 0.44, new recall ",
			},
			// 5 others qualify on their first call only: own precision 48/53, the list's 16/21.
			{
				"a list under its precision", nil, heldOthers[:5],
				"end-to-end `qualified` list on heldout: precision 0.76, recall 1.00, new recall " +
					"1.00, support 16",
			},
			// Every first answer misses: the own gate passes on the other calls, the list is empty.
			{
				"an empty list", heldCorrecting, nil,
				"end-to-end `qualified` list on heldout: precision 0.00, recall 0.00, new recall " +
					"0.00, support 16",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := copyClassify(t)
				out, err := runClassify(t, dir, &labelJev{
					labels: labels, firstLow: firstLow(tc.low), firstOnly: firstLow(tc.high),
					dupScore: failDup(),
				})
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				tb := readTable(t, dir)
				q := tb.Decisions[classify.Qualification]
				if q.Heldout.Recall < 0.5 || q.Heldout.Precision < ClassifyTarget || q.Enabled ||
					!strings.HasPrefix(q.Reason, tc.reason) {
					t.Fatalf("qualification %+v (heldout %+v) e2e %+v\n%s", q, q.Heldout,
						tb.EndToEnd, out)
				}
				verify(t, dir)
			})
		}
	})

	t.Run("a new recall of exactly 0.50 passes", func(t *testing.T) {
		dir := copyClassify(t)
		// Qualification misses 4 of the 8 heldout new items on every call.
		miss := map[string]bool{}
		for _, r := range replies(t, dir, labels, SplitHeldout, func(l ItemLabel) bool {
			return l.Correcting() && l.Dup == classify.DupNew
		})[:4] {
			miss[r] = true
		}
		out, err := runClassify(t, dir, &labelJev{labels: labels, miss: miss, dupScore: failDup()})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tb := readTable(t, dir)
		q, e := tb.Decisions[classify.Qualification], tb.EndToEnd
		if e == nil || e.List != jev.ListQualified || e.NewRecall == nil || *e.NewRecall != 0.5 ||
			!e.Passed || !q.Enabled || q.Reason != "" {
			t.Fatalf("qualification %+v e2e %+v\n%s", q, e, out)
		}
		verify(t, dir)
	})

	// heldItem finds a heldout correcting item by label: its ID and reply.
	heldItem := func(pred func(ItemLabel) bool) (string, string, ItemLabel) {
		t.Helper()
		cases, err := LoadClassify(probe)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if c.Label.Split != SplitHeldout {
				continue
			}
			for _, id := range slices.Sorted(maps.Keys(c.Label.Items)) {
				if l := c.Label.Items[id]; l.Correcting() && pred(l) {
					for k, kl := range labels {
						if kl == l {
							return id, strings.SplitN(k, "\x00", 2)[1], l
						}
					}
				}
			}
		}
		t.Fatal("no such heldout item")
		return "", "", ItemLabel{}
	}
	kindID, _, kindLabel := heldItem(func(l ItemLabel) bool {
		return l.DupKind != "" && l.Dup == classify.DupNew
	})
	undoID, undoReply, _ := heldItem(func(l ItemLabel) bool {
		return l.Undo && l.DupKind == "" && l.Dup == classify.DupDuplicate
	})

	t.Run("a missing dup_ score is a miss and one at the cutoff is duplicate", func(t *testing.T) {
		dir := copyClassify(t)
		news := replies(t, dir, labels, SplitHeldout, func(l ItemLabel) bool {
			return l.Correcting() && l.Dup == classify.DupNew && l.DupKind == ""
		})
		// One new item has no dup_ score on any call, another scores exactly the cutoff; a third
		// attempt of the undo positive is missing.
		out, err := runClassify(t, dir, &labelJev{
			labels:   labels,
			dupMiss:  map[string]int{news[0]: 0, undoReply: 3},
			dupScore: map[string]float64{news[1]: 0.5},
		})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tb := readTable(t, dir)
		d, e := tb.Decisions[classify.Duplicate], tb.EndToEnd
		if !d.Enabled || *d.Cutoff != 0.5 || e == nil || e.List != jev.ListNew || e.TP != 6 ||
			e.FN != 2 || e.Support != 8 {
			t.Fatalf("duplicate %+v e2e %+v\n%s", d, e, out)
		}
		// O1's per-case lines: ID, expected label, category, undo and three attempt outcomes.
		attempts := func(xs ...string) string { return strings.Join(xs, ", ") + "\n" }
		dupY, dupN := classify.DupDuplicate, classify.DupNew
		for _, line := range []string{
			"    " + kindID + ": expected new, dup_kind " + kindLabel.DupKind + ", undo " +
				strconv.FormatBool(kindLabel.Undo) + ": " + attempts(dupN, dupN, dupN),
			"    " + undoID + ": expected duplicate, dup_kind -, undo true: " +
				attempts(dupY, dupY, "missing"),
		} {
			if !strings.Contains(out, "  heldout cases at 0.50:\n") ||
				!strings.Contains(out, line) {
				t.Fatalf("no line %q:\n%s", line, out)
			}
		}
		verify(t, dir)
	})

	t.Run("with no cutoff the per-case lines give scores", func(t *testing.T) {
		dir := copyClassify(t)
		// Every fit duplicate scores low: no cutoff passes the margin test.
		low := map[string]float64{}
		for _, r := range replies(t, dir, labels, SplitFit, func(l ItemLabel) bool {
			return l.Correcting() && l.Dup == classify.DupDuplicate
		}) {
			low[r] = 0.1
		}
		out, err := runClassify(t, dir, &labelJev{
			labels: labels, dupScore: low, dupMiss: map[string]int{undoReply: 2},
		})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if tb := readTable(t, dir); tb.Decisions[classify.Duplicate].Cutoff != nil {
			t.Fatalf("a duplicate cutoff fits: %+v", tb.Decisions[classify.Duplicate])
		}
		for _, line := range []string{
			"  heldout cases, no cutoff fits (scores):\n",
			"    " + undoID + ": expected duplicate, dup_kind -, undo true: scores 0.90, missing, " +
				"0.90\n",
		} {
			if !strings.Contains(out, line) {
				t.Fatalf("no line %q:\n%s", line, out)
			}
		}
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
