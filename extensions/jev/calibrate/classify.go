package calibrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/classify"
)

// minPairs is the antecedent pairs the corpus needs; minRecall the end-to-end recall (§3.5).
const (
	minPairs  = 4
	minRecall = 0.5
)

var (
	errCorpus        = errors.New("corpus")
	errReport        = errors.New("nothing written; a run of the runtime path wrote no report")
	errInvalidReport = errors.New("invalid report")
)

// ClassifyTarget is the precision target of both classify decisions and of the end-to-end list
// (§3.5).
const ClassifyTarget = 0.8

// ItemLabel is the hand-written label of one corpus item.
type ItemLabel struct {
	// Kind is correction, drift, approval, request or noise; an overlap carries its larger label.
	Kind string `json:"kind"`
	// Overlap marks a reply that both corrects and restores scope.
	Overlap bool `json:"overlap,omitempty"`
	// Dup is duplicate or new against the item's directory's index; set for correcting items.
	Dup   string `json:"dup,omitempty"`
	Lang  string `json:"lang"`
	Hard  bool   `json:"hard,omitempty"`
	Group string `json:"group,omitempty"`
	// DupKind names a hard duplicate category: a positive one on a duplicate item, a negative
	// one on a new item; it implies Hard (§3.5).
	DupKind string `json:"dup_kind,omitempty"` //nolint:tagliatelle // the label's snake_case key
	// Undo marks a reply that only undoes or stops a change.
	Undo bool   `json:"undo,omitempty"`
	Why  string `json:"why"`
}

// The dup_kind categories (§3.5): the positive ones of a duplicate item, the negative ones of a
// new item.
var (
	positiveDupKinds = []string{"restatement", "paraphrase", "general", "multi"}
	negativeDupKinds = []string{"object", "condition", "action", "broken", "partial"}
)

// heldoutOnlyDupKinds are the categories no fit case has: heldout-b is their first measure.
var heldoutOnlyDupKinds = []string{"multi", "partial"}

// heldoutDupKinds are the held-out quotas of each dup_kind.
//
//nolint:mnd // the §3.5 quotas
var heldoutDupKinds = map[string]int{
	"restatement": 2, "paraphrase": 1, "general": 1, "multi": 1,
	"object": 1, "condition": 1, "action": 1, "broken": 1, "partial": 1,
}

// Correcting reports a qualification positive: a correction, a drift or an overlap.
func (l ItemLabel) Correcting() bool {
	return l.Overlap || l.Kind == classify.LabelCorrection || l.Kind == classify.LabelDrift
}

// ClassifyLabel is a cal fixture's label: its split and its items by corpus-wide case ID, which
// is also the observe item ID.
type ClassifyLabel struct {
	Split string               `json:"split"`
	Items map[string]ItemLabel `json:"items"`
}

// ClassifyCase is one cal fixture.
type ClassifyCase struct {
	Path    string
	Fixture jev.Fixture
	Label   ClassifyLabel
}

// ClassifyOptions configures a classify calibration run.
type ClassifyOptions struct {
	// Fixtures is fixtures/jev/classify, with fixed/ and cal/ under it.
	Fixtures string
	// Table is the cutoff table to write; an owner acceptance in it is carried over.
	Table     string
	Transport jev.Transport
	Runner    proc.Runner
	Environ   []string
	Stdout    io.Writer
}

// LoadClassify reads the cal fixtures of dir, sorted by path.
func LoadClassify(dir string) ([]ClassifyCase, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "cal", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("list fixtures: %w", err)
	}
	slices.Sort(paths)
	cases := make([]ClassifyCase, 0, len(paths))
	for _, p := range paths {
		f, err := jev.ReadFixture(p)
		if err != nil {
			return nil, err
		}
		c := ClassifyCase{Path: p, Fixture: f}
		if err := json.Unmarshal(f.Label, &c.Label); err != nil {
			return nil, fmt.Errorf("%s: label: %w", p, err)
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// classCounts is one split's item counts.
type classCounts struct {
	correction, drift, overlap, dup, newc int
	approval, request, noise              int
	langCorrecting, langOthers            map[string]int
	langDup, langNew                      map[string]int
	hard, total                           int
	// dupKinds counts each dup_kind; undoDup the duplicate items marked undo; overlapNeg the
	// overlaps carrying a negative dup_kind.
	dupKinds            map[string]int
	undoDup, overlapNeg int
}

// pairMember is one member of an antecedent pair: its fixture, language and kind.
type pairMember struct{ path, lang, kind string }

// CheckClassifyCorpus checks §3.5's corpus rules: per split the item counts, the language
// balance of each side of the heldout, the share of hard items, every item with a hand-written
// why, every dup_kind matching its item's dup and implying hard, the heldout dup_kind and undo
// quotas, every group in one split, at least 4 antecedent pairs and the heldout's 2, one en and
// one ja.
//
//nolint:gocyclo,cyclop,funlen,maintidx // one check per count of §3.5
func CheckClassifyCorpus(cases []ClassifyCase) error {
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%w: %s", errCorpus, fmt.Sprintf(format, a...)))
	}
	by := map[string]*classCounts{}
	groups := map[string]string{}
	seen := map[string]bool{}
	// pairs holds each antecedent pair's members across the whole corpus.
	pairs := map[string][]pairMember{}
	for _, c := range cases {
		sp := c.Label.Split
		if sp != SplitFit && sp != SplitHeldout {
			fail("%s: split %q", c.Path, sp)
			continue
		}
		cc := by[sp]
		if cc == nil {
			cc = &classCounts{
				langCorrecting: map[string]int{}, langOthers: map[string]int{},
				langDup: map[string]int{}, langNew: map[string]int{}, dupKinds: map[string]int{},
			}
			by[sp] = cc
		}
		for id, l := range c.Label.Items {
			if seen[id] {
				fail("%s: case %s is in two fixtures", c.Path, id)
			}
			seen[id] = true
			if l.Why == "" || (l.Lang != "en" && l.Lang != "ja") ||
				!slices.Contains(classify.Labels, l.Kind) {
				fail("%s: case %s has no why, language or kind: %+v", c.Path, id, l)
			}
			if l.Correcting() != (l.Dup != "") ||
				l.Dup != "" && l.Dup != classify.DupDuplicate && l.Dup != classify.DupNew {
				fail(
					"%s: case %s: a correcting item has a duplicate label and no other does",
					c.Path,
					id,
				)
			}
			if l.Group != "" {
				if g, ok := groups[l.Group]; ok && g != sp {
					fail("group %s spans %s and %s", l.Group, g, sp)
				}
				groups[l.Group] = sp
				if strings.HasPrefix(l.Group, "pair-") {
					pairs[l.Group] = append(pairs[l.Group], pairMember{c.Path, l.Lang, l.Kind})
				}
			}
			if l.DupKind != "" {
				pos := slices.Contains(positiveDupKinds, l.DupKind)
				neg := slices.Contains(negativeDupKinds, l.DupKind)
				switch {
				case !pos && !neg:
					fail("%s: case %s: unknown dup_kind %q", c.Path, id, l.DupKind)
				case pos && l.Dup != classify.DupDuplicate || neg && l.Dup != classify.DupNew:
					fail("%s: case %s: dup_kind %s does not match dup %q", c.Path, id, l.DupKind,
						l.Dup)
				case !l.Hard:
					fail("%s: case %s: dup_kind %s on an item not hard", c.Path, id, l.DupKind)
				case sp == SplitFit && slices.Contains(heldoutOnlyDupKinds, l.DupKind):
					fail("%s: case %s: dup_kind %s is held-out only", c.Path, id, l.DupKind)
				}
				cc.dupKinds[l.DupKind]++
				if neg && l.Overlap {
					cc.overlapNeg++
				}
			}
			cc.total++
			if l.Hard {
				cc.hard++
			}
			if l.Correcting() {
				switch {
				case l.Overlap:
					cc.overlap++
				case l.Kind == classify.LabelCorrection:
					cc.correction++
				default:
					cc.drift++
				}
				if l.Dup == classify.DupDuplicate {
					cc.dup++
					cc.langDup[l.Lang]++
					if l.Undo {
						cc.undoDup++
					}
				} else {
					cc.newc++
					cc.langNew[l.Lang]++
				}
				cc.langCorrecting[l.Lang]++
				continue
			}
			cc.langOthers[l.Lang]++
			switch l.Kind {
			case classify.LabelApproval:
				cc.approval++
			case classify.LabelRequest:
				cc.request++
			default:
				cc.noise++
			}
		}
		if n := len(c.Label.Items); n < 20 || n > 40 {
			fail("%s: %d items, want 20-40", c.Path, n)
		}
	}
	fit, held := by[SplitFit], by[SplitHeldout]
	if fit == nil || held == nil {
		return errors.Join(append(errs, fmt.Errorf("%w: no fit or no heldout split", errCorpus))...)
	}
	correcting := fit.correction + fit.drift + fit.overlap
	switch {
	case correcting < 24 || fit.correction < 8 || fit.drift < 8 || fit.overlap < 4:
		fail("fit has %d corrections, %d drift, %d overlaps; want 24 correcting with 8, 8 and 4",
			fit.correction, fit.drift, fit.overlap)
	case fit.dup < 12 || fit.newc < 12:
		fail("fit has %d duplicate and %d new correcting items, want 12 + 12", fit.dup, fit.newc)
	case fit.approval < 4 || fit.request < 4 || fit.noise < 4 ||
		fit.approval+fit.request+fit.noise < 16:
		fail("fit has %d approvals, %d requests, %d noise; want 16 with 4 each", fit.approval,
			fit.request, fit.noise)
	}
	if held.correction != 6 || held.drift != 6 || held.overlap != 4 || held.dup != 8 ||
		held.newc != 8 {
		fail("heldout has %d corrections, %d drift, %d overlaps, %d duplicate, %d new; want "+
			"6, 6, 4, 8, 8", held.correction, held.drift, held.overlap, held.dup, held.newc)
	}
	if held.request != 6 || held.approval != 5 || held.noise != 5 {
		fail("heldout has %d requests, %d approvals, %d noise; want 6, 5, 5", held.request,
			held.approval, held.noise)
	}
	if held.langCorrecting["en"] != 8 || held.langCorrecting["ja"] != 8 ||
		held.langOthers["en"] != 8 || held.langOthers["ja"] != 8 {
		fail("heldout languages: correcting %v, others %v; want 8 + 8 each", held.langCorrecting,
			held.langOthers)
	}
	if held.langDup["en"] != 4 || held.langDup["ja"] != 4 || held.langNew["en"] != 4 ||
		held.langNew["ja"] != 4 {
		fail("heldout languages: duplicate %v, new %v; want 4 + 4 each", held.langDup,
			held.langNew)
	}
	for _, k := range slices.Concat(positiveDupKinds, negativeDupKinds) {
		if held.dupKinds[k] < heldoutDupKinds[k] {
			fail("heldout has %d dup_kind %s items, want at least %d", held.dupKinds[k], k,
				heldoutDupKinds[k])
		}
	}
	if held.undoDup == 0 {
		fail("heldout has no duplicate item marked undo, want at least 1")
	}
	if held.overlapNeg == 0 {
		fail("heldout has no overlap among its hard negatives, want at least 1")
	}
	heldPairs := map[string]int{}
	for _, g := range slices.Sorted(maps.Keys(pairs)) {
		var kinds []string
		langs, fixtures := map[string]bool{}, map[string]bool{}
		for _, m := range pairs[g] {
			kinds = append(kinds, m.kind)
			langs[m.lang], fixtures[m.path] = true, true
		}
		slices.Sort(kinds)
		if !slices.Equal(kinds, []string{classify.LabelDrift, classify.LabelRequest}) {
			fail("antecedent pair %s has members %v, want one drift and one request", g, kinds)
		}
		// A pair shares its assistant message and reply: both members live in one week.
		if len(fixtures) != 1 {
			fail("antecedent pair %s spans %d fixtures, want one", g, len(fixtures))
		}
		lang := strings.Join(slices.Sorted(maps.Keys(langs)), "+")
		if len(langs) != 1 {
			fail("antecedent pair %s mixes languages %s", g, lang)
		}
		if groups[g] == SplitHeldout {
			heldPairs[lang]++
		}
	}
	if len(heldPairs) != 2 || heldPairs["en"] != 1 || heldPairs["ja"] != 1 {
		fail("heldout antecedent pairs by language %v, want one en and one ja", heldPairs)
	}
	for sp, cc := range map[string]*classCounts{SplitFit: fit, SplitHeldout: held} {
		if 5*cc.hard < 2*cc.total {
			fail("%s has %d hard items of %d, want at least 40%%", sp, cc.hard, cc.total)
		}
	}
	if len(pairs) < minPairs {
		fail("%d antecedent pairs (groups pair-*), want at least 4", len(pairs))
	}
	return errors.Join(errs...)
}

// classifyRun is one fixture run in process: its materialised root and the transport that serves
// its requests. It returns the response and the normalised report.
func classifyRun(
	ctx context.Context, o ClassifyOptions, c ClassifyCase, ds map[string]jev.Decision,
	t jev.Transport, order func(map[string]string),
) (jev.Response, json.RawMessage, error) {
	root, err := os.MkdirTemp("", "jev-calibrate-classify-")
	if err != nil {
		return jev.Response{}, nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	env := slices.DeleteFunc(slices.Clone(o.Environ), func(kv string) bool {
		return strings.HasPrefix(strings.ToUpper(kv), "GIT_")
	})
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=jev-calibrate", "GIT_AUTHOR_EMAIL=jev-calibrate@example.invalid",
		"GIT_COMMITTER_NAME=jev-calibrate", "GIT_COMMITTER_EMAIL=jev-calibrate@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	git := func(dir string, args ...string) error {
		gctx, cancel := context.WithTimeout(ctx, gitTimeout)
		defer cancel()
		full := slices.Concat([]string{
			"-c", "core.autocrlf=false", "-c", "commit.gpgsign=false",
			"-c", "core.hooksPath=" + os.DevNull, "-c", "init.defaultBranch=main",
		}, args)
		res, err := o.Runner.Run(gctx, proc.Cmd{Name: "git", Args: full, Dir: dir, Env: env})
		switch {
		case err != nil:
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		case res.TimedOut || res.ExitCode != 0:
			return fmt.Errorf("%w: git %s: exit %d: %s", errGit, strings.Join(args, " "),
				res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		return nil
	}
	root, err = classify.Materialize(c.Fixture, root, git)
	if err != nil {
		return jev.Response{}, nil, err
	}
	home := filepath.Join(root, "home")
	var stdout, stderr bytes.Buffer
	classify.Main(ctx, classify.Env{
		Stdin: bytes.NewReader(c.Fixture.Request), Stdout: &stdout, Stderr: &stderr, Home: home,
		Dir: root, Getenv: func(string) string { return "" }, Environ: env, Runner: o.Runner,
		Decisions: ds, Transport: t, Order: order,
	})
	var resp jev.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return jev.Response{}, nil, fmt.Errorf("%s: response %q: %w", c.Path, stdout.String(), err)
	}
	// Every run must leave a valid report: a fixture's want is refreshed from it.
	raw, err := os.ReadFile(
		filepath.Join(home, ".local", "state", "stack", "jev-classify", "latest.json"),
	)
	var report json.RawMessage
	if err == nil {
		if report = normaliseReport(raw); report == nil {
			err = errInvalidReport
		}
	}
	if err != nil {
		return jev.Response{}, nil, fmt.Errorf("%w: %s: no readable report (%w); context %q",
			errReport, c.Path, err, resp.Context)
	}
	return resp, report, nil
}

// attempts sends each body Attempts times through a live transport, returns the first outcome
// and keeps every question's scores. Outcomes of a replay (a recorded answer) are returned as is.
type attempts struct {
	live jev.Transport
	mu   sync.Mutex
	// recs is the replayable record of each body's first attempt; bad lists bodies with none.
	recs   map[string]jev.Record
	bad    []string
	scores map[string]map[string][]*float64
	// first is each body's first outcome.
	first map[string]jev.Outcome
}

func newAttempts(live jev.Transport) *attempts {
	return &attempts{
		live: live, recs: map[string]jev.Record{}, scores: map[string]map[string][]*float64{},
		first: map[string]jev.Outcome{},
	}
}

// Send implements jev.Transport.
func (a *attempts) Send(ctx context.Context, b jev.Batch) jev.Outcome {
	a.mu.Lock()
	prev, done := a.first[b.Hash]
	a.mu.Unlock()
	if done {
		return prev
	}
	sc := map[string][]*float64{}
	var first jev.Outcome
	for i := range Attempts {
		sctx, cancel := context.WithTimeout(ctx, SendTimeout)
		out := a.live.Send(sctx, b)
		cancel()
		if i == 0 {
			first = out
		}
		for _, q := range b.Questions {
			var p *float64
			if ans, ok := out.Answers[q.Name]; out.Class == jev.OK && ok {
				v := ans.Noul
				if q.Type == jev.TypeChoice {
					v = classify.Score(ans.Probabilities)
				}
				p = &v
			}
			sc[q.Name] = append(sc[q.Name], p)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := replayable(first)
	if !ok {
		a.bad = append(a.bad, b.Hash)
		return first
	}
	a.recs[b.Hash], a.scores[b.Hash], a.first[b.Hash] = rec, sc, first
	return first
}

// forced answers a kind_ batch from its recorded first outcome with the questions of forced
// cases answered as a certain correction, and every other batch through live.
type forced struct {
	kind  *attempts
	dup   *attempts
	force map[string]bool
}

// Send implements jev.Transport.
func (f forced) Send(ctx context.Context, b jev.Batch) jev.Outcome {
	if len(b.Questions) == 0 || !strings.HasPrefix(b.Questions[0].Name, "kind_") {
		return f.dup.Send(ctx, b)
	}
	return forceOutcome(f.kind.first[b.Hash], b, f.force)
}

// forceOutcome rewrites the answers of the forced kind_ questions of a recorded outcome to a
// certain correction. The forced questions are the ground-truth correcting items, whose dup_
// bodies are recorded whatever the first kind_ answer was: when that answer is unusable (a 413,
// a failure, an undecodable body), the forced questions still qualify and the others are a
// certain noise, which qualifies none.
func forceOutcome(o jev.Outcome, b jev.Batch, force map[string]bool) jev.Outcome {
	var doc, answers map[string]json.RawMessage
	usable := o.Class == jev.OK && json.Unmarshal(o.Body, &doc) == nil &&
		json.Unmarshal(doc["answers"], &answers) == nil && answers != nil
	if !usable {
		if !slices.ContainsFunc(b.Questions, func(q jev.Question) bool { return force[q.Name] }) {
			return o
		}
		doc, answers = map[string]json.RawMessage{}, map[string]json.RawMessage{}
		doc["model"], _ = jev.Marshal(jev.Pin)
		for _, q := range b.Questions {
			if !force[q.Name] {
				answers[q.Name] = certainChoice(classify.LabelNoise)
			}
		}
	}
	for _, q := range b.Questions {
		if force[q.Name] {
			answers[q.Name] = certainChoice(classify.LabelCorrection)
		}
	}
	doc["answers"], _ = jev.Marshal(answers)
	body, _ := jev.Marshal(doc)
	return jev.Record{Answer: body}.Outcome(b.Questions)
}

// certainChoice is a kind_ answer of label with probability 1.
func certainChoice(label string) json.RawMessage {
	p := map[string]float64{}
	for _, l := range classify.Labels {
		p[l] = 0
	}
	p[label] = 1
	out, _ := jev.Marshal(map[string]any{
		"type": "choice", "choice": label, "confidence": 1, "probabilities": p,
	})
	return out
}

// caseData is what the run learned about one case.
type caseData struct {
	c     ClassifyCase
	sent  map[string]string // observe ID → r<n>
	kinds *attempts
	dups  *attempts
}

func (d *caseData) kindScores(id string) []*float64 {
	q := "kind_" + d.sent[id]
	for _, sc := range d.kinds.scores {
		if s, ok := sc[q]; ok {
			return s
		}
	}
	return nil
}

func (d *caseData) dupScores(id string) []*float64 {
	q := "dup_" + d.sent[id]
	for _, sc := range d.dups.scores {
		if s, ok := sc[q]; ok {
			return s
		}
	}
	return nil
}

func hit(p *float64, cutoff float64) bool { return p != nil && jev.Hit(*p, cutoff) }

// samples builds a decision's single-call samples of one split from per-case scores: each
// attempt is one sample, a missing score a negative prediction.
func classifySamples(
	data []*caseData, split string, truth func(ItemLabel) (bool, bool),
	scores func(*caseData, string) []*float64,
) []jev.Sample {
	var out []jev.Sample
	for _, d := range data {
		if d.c.Label.Split != split {
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(d.c.Label.Items)) {
			t, ok := truth(d.c.Label.Items[id])
			if !ok {
				continue
			}
			ss := scores(d, id)
			if len(ss) == 0 {
				ss = make([]*float64, Attempts)
			}
			for _, p := range ss {
				out = append(out, jev.Sample{Case: id, Score: p, Truth: t})
			}
		}
	}
	return out
}

func truthQual(l ItemLabel) (bool, bool) { return l.Correcting(), true }

func truthDup(l ItemLabel) (bool, bool) {
	return l.Dup == classify.DupDuplicate, l.Correcting()
}

// eligible is a decision's cutoff when it passed its own gate, else nil: the cutoffs the shipped
// table can decide with.
func eligible(d jev.Decision) *float64 {
	if !d.Enabled {
		return nil
	}
	return d.Cutoff
}

// endToEnd scores, on heldout, the list the report would show, from the first answers with the
// cutoffs of decisions that passed their own gates (§3.5 Activation). The duplicate decision's gate
// picks the list: `new` (qualified, with a dup_ score below its cutoff, against the items labelled
// correcting and new) or `qualified` (every qualified item, against the items labelled
// correcting, with recall also on those labelled new). Qualification's gate decides whether
// anything is listed: failed, nothing is, every labelled item is a miss and the support is kept.
// A missing kind_ or dup_ score is a miss; a dup_ score at the cutoff is `duplicate`. lang, when
// set, scores only the items in that language.
//
//nolint:gocognit // one pass over the labelled items, then the figures
func endToEnd(data []*caseData, qual, dup jev.Decision, lang string) jev.EndToEnd {
	qc, dc := eligible(qual), eligible(dup)
	e := jev.EndToEnd{List: jev.ListNew, Scored: qc != nil}
	if dc == nil {
		e.List = jev.ListQualified
	}
	newSupport, newListed := 0, 0
	for _, d := range data {
		if d.c.Label.Split != SplitHeldout {
			continue
		}
		for id, l := range d.c.Label.Items {
			if lang != "" && l.Lang != lang {
				continue
			}
			isNew := l.Correcting() && l.Dup == classify.DupNew
			want := isNew
			if e.List == jev.ListQualified {
				want = l.Correcting()
			}
			listed := false
			if ks := d.kindScores(id); e.Scored && len(ks) > 0 && hit(ks[0], *qc) {
				listed = true
				if e.List == jev.ListNew {
					ds := d.dupScores(id)
					listed = len(ds) > 0 && ds[0] != nil && !jev.Hit(*ds[0], *dc)
				}
			}
			switch {
			case want && listed:
				e.TP++
			case want:
				e.FN++
			case listed:
				e.FP++
			}
			if isNew {
				newSupport++
				if listed {
					newListed++
				}
			}
		}
	}
	if e.TP+e.FP > 0 {
		e.Precision = float64(e.TP) / float64(e.TP+e.FP)
	}
	// Every target item is a hit or a miss.
	e.Support = e.TP + e.FN
	if e.Support > 0 {
		e.Recall = float64(e.TP) / float64(e.Support)
	}
	e.Passed = e.TP > 0 && e.Precision >= ClassifyTarget && e.Recall >= minRecall
	if e.List == jev.ListQualified {
		recall := 0.0
		if newSupport > 0 {
			recall = float64(newListed) / float64(newSupport)
		}
		e.NewSupport, e.NewRecall = &newSupport, &recall
		e.Passed = e.Passed && recall >= minRecall
	}
	return e
}

// HeldoutHash hashes the heldout fixtures' cases, records, repository layouts and samples: what
// an acceptance is bound to.
func HeldoutHash(cases []ClassifyCase) string {
	h := sha256.New()
	for _, c := range cases {
		if c.Label.Split != SplitHeldout {
			continue
		}
		// JSON parts are compacted: the hash does not depend on how a fixture was indented.
		for _, part := range [][]byte{
			[]byte(filepath.Base(c.Path)), compact(c.Fixture.Request), compact(c.Fixture.Observe),
			compact(c.Fixture.Label), compact(c.Fixture.Repo),
		} {
			h.Write(part)
		}
		for _, k := range slices.Sorted(maps.Keys(c.Fixture.Files)) {
			h.Write([]byte(k))
			h.Write(compact(c.Fixture.Files[k]))
		}
		for _, k := range slices.Sorted(maps.Keys(c.Fixture.Memory)) {
			h.Write([]byte(k + c.Fixture.Memory[k]))
		}
		// Every attempt's score feeds the gates, so the samples are inputs too (encoding/json
		// sorts the map keys).
		samples, err := json.Marshal(c.Fixture.Samples)
		if err != nil {
			samples = []byte("unencodable samples")
		}
		h.Write(samples)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func compact(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

// readAccepted reads a previous table's owner acceptance, if any.
func readAccepted(path string) *jev.Acceptance {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var t jev.Table
	if json.Unmarshal(data, &t) != nil {
		return nil
	}
	return t.Accepted
}

// RunClassify records every cal fixture of the classify corpus, refits both decisions, scores the
// report end to end on heldout, sets the enabled bits and refreshes the fixtures' expect and want
// (§3.5). When any first attempt has no replayable outcome it writes nothing and returns an error
// asking for a rerun.
func RunClassify(ctx context.Context, o ClassifyOptions) error {
	cases, err := LoadClassify(o.Fixtures)
	if err != nil {
		return err
	}
	if err := CheckClassifyCorpus(cases); err != nil {
		return fmt.Errorf("the corpus is not eligible:\n%w", err)
	}
	zero := 0.0
	// Phase 1: every kind_ body as the runtime packs it, three calls each.
	data := make([]*caseData, 0, len(cases))
	var failed []string
	for _, c := range cases {
		d := &caseData{c: c, kinds: newAttempts(o.Transport), dups: newAttempts(o.Transport)}
		ds := map[string]jev.Decision{
			classify.Qualification: {Enabled: true, Cutoff: &zero},
			classify.Duplicate:     {Reason: "recording kind_ bodies"},
		}
		if _, _, err := classifyRun(ctx, o, c, ds, d.kinds, func(m map[string]string) {
			d.sent = m
		}); err != nil {
			return err
		}
		failed = append(failed, d.kinds.bad...)
		data = append(data, d)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: %d bodies", errRerun, len(failed))
	}
	qual := jev.Calibrate(classifySamples(data, SplitFit, truthQual, (*caseData).kindScores),
		classifySamples(data, SplitHeldout, truthQual, (*caseData).kindScores), ClassifyTarget)

	// Phase 2: the dup_ body of every ground-truth correcting item and of every item the first
	// kind_ answer qualifies at the fitted cutoff, three calls each; the union is what replay asks.
	for _, d := range data {
		force := forcedQuestions(d.c.Label, d.sent)
		if _, _, err := classifyRun(
			ctx,
			o,
			d.c,
			unionDecisions(qual.Cutoff),
			forced{kind: d.kinds, dup: d.dups, force: force},
			nil,
		); err != nil {
			return err
		}
		failed = append(failed, d.dups.bad...)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: %d bodies", errRerun, len(failed))
	}
	dup := jev.Calibrate(classifySamples(data, SplitFit, truthDup, (*caseData).dupScores),
		classifySamples(data, SplitHeldout, truthDup, (*caseData).dupScores), ClassifyTarget)

	// Records and samples, then the activation gate.
	for _, d := range data {
		recs := maps.Clone(d.kinds.recs)
		maps.Copy(recs, d.dups.recs)
		if err := d.c.Fixture.SetRecords(recs); err != nil {
			return err
		}
		d.c.Fixture.Samples = map[string]map[string][]*float64{}
		maps.Copy(d.c.Fixture.Samples, d.kinds.scores)
		maps.Copy(d.c.Fixture.Samples, d.dups.scores)
		d.c.Fixture.Placeholder = ""
	}
	updated := make([]ClassifyCase, len(data))
	for i, d := range data {
		updated[i] = d.c
	}
	e2e := endToEnd(data, qual, dup, "")
	table, review := activate(qual, dup, e2e, readAccepted(o.Table), HeldoutHash(updated))
	reportClassify(o.Stdout, data, table, review, map[string]jev.EndToEnd{
		"en": endToEnd(data, qual, dup, "en"), "ja": endToEnd(data, qual, dup, "ja"),
	})

	// Refresh expect and want by replaying the records under the decisions this table ships.
	ds := map[string]jev.Decision{
		classify.Qualification: table.Decisions[classify.Qualification],
		classify.Duplicate:     table.Decisions[classify.Duplicate],
	}
	for i := range updated {
		replay, err := updated[i].Fixture.RequestRecords()
		if err != nil {
			return err
		}
		resp, report, err := classifyRun(ctx, o, updated[i], ds, replay, nil)
		if err != nil {
			return err
		}
		updated[i].Fixture.Expect = &jev.Expect{Status: resp.Status}
		updated[i].Fixture.Want = &jev.Want{
			Findings: resp.Findings, Context: resp.Context, Report: report,
		}
	}
	for _, c := range updated {
		if err := jev.WriteFixture(c.Path, c.Fixture); err != nil {
			return err
		}
	}
	return jev.WriteTable(o.Table, table)
}

// activate builds the table from the two fitted decisions and the end-to-end score (§3.5
// Activation): qualification ships enabled exactly when it passed its own gate and either the
// scored list passes or the owner's acceptance holds for these heldout inputs and figures. The
// acceptance applies only in that case. A qualification failing its own gate keeps its own
// reason; one disabled by the end-to-end gate gets its reason and review is true.
func activate(
	qual, dup jev.Decision, e2e jev.EndToEnd, accepted *jev.Acceptance, hash string,
) (jev.Table, bool) {
	table := jev.Table{
		Model:     jev.Pin,
		Decisions: map[string]jev.Decision{classify.Qualification: qual, classify.Duplicate: dup},
		EndToEnd:  &e2e,
	}
	own := qual.Enabled
	holds := own && !e2e.Passed && accepted.Holds(hash, e2e)
	if holds {
		table.Accepted = accepted
	}
	qual.Enabled = own && (e2e.Passed || holds)
	review := own && !qual.Enabled
	if review {
		newRecall := ""
		if e2e.NewRecall != nil {
			newRecall = fmt.Sprintf(", new recall %.2f", *e2e.NewRecall)
		}
		qual.Reason = fmt.Sprintf(
			"end-to-end `%s` list on heldout: precision %.2f, recall %.2f%s, "+
				"support %d miss the target %.2f / %.2f; REVIEW",
			e2e.List,
			e2e.Precision,
			e2e.Recall,
			newRecall,
			e2e.Support,
			ClassifyTarget,
			minRecall,
		)
	}
	table.Decisions[classify.Qualification] = qual
	return table, review
}

// UnionCutoff is the qualification cutoff the record union is rebuilt at: the fitted one, whether
// or not qualification ships enabled, as calibration recorded it.
func UnionCutoff(t jev.Table) *float64 {
	return t.Decisions[classify.Qualification].Cutoff
}

// VerifyClassify is the pinned check of recorded cal fixtures against their committed table: each
// recorded case's records are exactly the rebuilt union and, once no case is pending, the table is
// the refit of the recorded samples, with the same end-to-end figures and enabled bits.
func VerifyClassify(
	ctx context.Context, runner proc.Runner, environ []string, cases []ClassifyCase, t jev.Table,
) error {
	pending := false
	for _, c := range cases {
		if c.Fixture.Pending() {
			pending = true
			continue
		}
		bodies, err := ClassifyBodies(ctx, runner, environ, c, UnionCutoff(t))
		if err != nil {
			return err
		}
		rec, err := c.Fixture.RequestRecords()
		if err != nil {
			return err
		}
		if got := slices.Sorted(maps.Keys(rec.Records)); !slices.Equal(got, bodies) {
			extra := slices.DeleteFunc(slices.Clone(got), func(h string) bool {
				return slices.Contains(bodies, h)
			})
			missing := slices.DeleteFunc(slices.Clone(bodies), func(h string) bool {
				return slices.Contains(got, h)
			})
			return fmt.Errorf("%w: %s: records not in the rebuilt union %v, union bodies not "+
				"recorded %v", errPinned, c.Path, extra, missing)
		}
	}
	if pending || t.Placeholder != "" {
		return nil
	}
	decisions, e2e, err := RefitClassify(ctx, runner, environ, cases)
	if err != nil {
		return err
	}
	want, _ := activate(decisions[classify.Qualification], decisions[classify.Duplicate], e2e,
		t.Accepted, HeldoutHash(cases))
	for name, d := range want.Decisions {
		c := t.Decisions[name]
		got, _ := jev.Marshal(
			[]any{d.Enabled, d.Cutoff, d.Fit, d.Heldout, d.Margin, d.Baseline, d.Unstable},
		)
		committed, _ := jev.Marshal(
			[]any{c.Enabled, c.Cutoff, c.Fit, c.Heldout, c.Margin, c.Baseline, c.Unstable},
		)
		if string(got) != string(committed) {
			return fmt.Errorf("%w: %s: refit %s, committed %s", errPinned, name, got, committed)
		}
	}
	if t.EndToEnd == nil || !t.EndToEnd.Equal(e2e) ||
		(t.Accepted != nil) != (want.Accepted != nil) {
		return fmt.Errorf("%w: end to end: recomputed %+v, committed %+v (accepted %v)",
			errPinned, e2e, t.EndToEnd, want.Accepted != nil)
	}
	return nil
}

var errPinned = errors.New("the cal fixtures do not match the cutoff table")

// unionDecisions are the decisions the dup_ union is recorded and rebuilt under: qualification at
// the fitted cutoff, enabled or not (1.0 without one, inclusive: forced items and any item scoring
// exactly 1.0 qualify), and every qualified item's dup_ asked.
func unionDecisions(qualCutoff *float64) map[string]jev.Decision {
	cutoff, zero := 1.0, 0.0
	if qualCutoff != nil {
		cutoff = *qualCutoff
	}
	return map[string]jev.Decision{
		classify.Qualification: {Enabled: true, Cutoff: &cutoff},
		classify.Duplicate:     {Enabled: true, Cutoff: &zero},
	}
}

// forcedQuestions are the kind_ questions of a case's ground-truth correcting items, by the r<n>
// IDs they were sent under.
func forcedQuestions(l ClassifyLabel, sent map[string]string) map[string]bool {
	force := map[string]bool{}
	for id, item := range l.Items {
		if item.Correcting() {
			force["kind_"+sent[id]] = true
		}
	}
	return force
}

// union is a replay that forces the correcting items to qualify and notes every body asked: the
// bodies the §3.5 record union holds.
type union struct {
	inner  jev.Transport
	mu     sync.Mutex
	force  map[string]bool
	hashes map[string]bool
}

// Send implements jev.Transport.
func (u *union) Send(ctx context.Context, b jev.Batch) jev.Outcome {
	u.mu.Lock()
	u.hashes[b.Hash] = true
	force := u.force
	u.mu.Unlock()
	out := u.inner.Send(ctx, b)
	if len(b.Questions) > 0 && strings.HasPrefix(b.Questions[0].Name, "kind_") {
		out = forceOutcome(out, b, force)
	}
	return out
}

// ClassifyBodies rebuilds the hashes of the record union of a recorded cal fixture: every kind_
// body, and the dup_ body of every ground-truth correcting item and of every item its first
// kind_ answer qualifies at qualCutoff (nil: none beyond the correcting items). Replay finds a
// record for each of them.
func ClassifyBodies(
	ctx context.Context, runner proc.Runner, environ []string, c ClassifyCase, qualCutoff *float64,
) ([]string, error) {
	replay, err := c.Fixture.RequestRecords()
	if err != nil {
		return nil, err
	}
	u := &union{inner: replay, hashes: map[string]bool{}}
	o := ClassifyOptions{Runner: runner, Environ: environ}
	_, _, err = classifyRun(ctx, o, c, unionDecisions(qualCutoff), u, func(sent map[string]string) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.force = forcedQuestions(c.Label, sent)
	})
	return slices.Sorted(maps.Keys(u.hashes)), err
}

// RefitClassify fits both decisions from the recorded samples of the cal fixtures, before the
// activation gate, and scores the report end to end on heldout.
func RefitClassify(
	ctx context.Context, runner proc.Runner, environ []string, cases []ClassifyCase,
) (map[string]jev.Decision, jev.EndToEnd, error) {
	data := make([]*caseData, 0, len(cases))
	for _, c := range cases {
		d := &caseData{c: c, kinds: newAttempts(nil), dups: newAttempts(nil)}
		d.kinds.scores, d.dups.scores = c.Fixture.Samples, c.Fixture.Samples
		// The run only learns the sent IDs, which come before any request: both decisions off.
		ds := map[string]jev.Decision{
			classify.Qualification: {Reason: "refit"},
			classify.Duplicate:     {Reason: "refit"},
		}
		o := ClassifyOptions{Runner: runner, Environ: environ}
		if _, _, err := classifyRun(ctx, o, c, ds, nil, func(m map[string]string) {
			d.sent = m
		}); err != nil {
			return nil, jev.EndToEnd{}, err
		}
		data = append(data, d)
	}
	qual := jev.Calibrate(classifySamples(data, SplitFit, truthQual, (*caseData).kindScores),
		classifySamples(data, SplitHeldout, truthQual, (*caseData).kindScores), ClassifyTarget)
	dup := jev.Calibrate(classifySamples(data, SplitFit, truthDup, (*caseData).dupScores),
		classifySamples(data, SplitHeldout, truthDup, (*caseData).dupScores), ClassifyTarget)
	return map[string]jev.Decision{classify.Qualification: qual, classify.Duplicate: dup},
		endToEnd(data, qual, dup, ""), nil
}

// normaliseReport blanks the run ID of a report, which is random; its times come from the
// fixture's clock.
func normaliseReport(raw []byte) json.RawMessage {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	if run, ok := doc["run"].(map[string]any); ok {
		run["id"] = ""
	}
	b, err := jev.Marshal(doc)
	if err != nil {
		return nil
	}
	return b
}

func reportClassify(
	w io.Writer, data []*caseData, t jev.Table, review bool, byLang map[string]jev.EndToEnd,
) {
	for _, name := range []string{classify.Qualification, classify.Duplicate} {
		d := t.Decisions[name]
		_, _ = fmt.Fprintf(
			w,
			"%s: enabled %v, cutoff %s, baseline %s (old rule)\n",
			name,
			d.Enabled,
			cutoffText(d.Cutoff),
			cutoffText(d.Baseline),
		)
		if d.Reason != "" {
			_, _ = fmt.Fprintf(w, "  reason: %s\n", d.Reason)
		}
		switch {
		case d.Cutoff == nil:
			_, _ = fmt.Fprintf(w, "  REVIEW: no cutoff passes the margin test\n")
		case !d.Enabled && (name != classify.Qualification || !review):
			_, _ = fmt.Fprintf(w, "  REVIEW: %s fails its own gate\n", name)
		}
		if d.Fit != nil {
			_, _ = fmt.Fprintf(
				w,
				"  fit: %s\n  heldout: %s\n",
				statsText(*d.Fit),
				statsText(*d.Heldout),
			)
		}
		for _, u := range d.Unstable {
			_, _ = fmt.Fprintf(w, "  unstable %s\n", u)
		}
		if at := d.Cutoff; at != nil {
			_, _ = fmt.Fprintf(w, "  flips across calls at %.2f: %s\n", *at,
				flipText(classifyFlips(data, name, *at)))
		}
		if name == classify.Duplicate {
			dupCases(w, data, d.Cutoff)
		}
	}
	if e := t.EndToEnd; e != nil {
		_, _ = fmt.Fprintf(
			w,
			"end to end (heldout, `%s` list, cutoffs that passed their own gates): scored %v, "+
				"TP %d FP %d FN %d, precision %.2f, recall %.2f, support %d%s, passed %v\n",
			e.List,
			e.Scored,
			e.TP,
			e.FP,
			e.FN,
			e.Precision,
			e.Recall,
			e.Support,
			newText(*e),
			e.Passed,
		)
		// Reported, not a gate: an aggregate can pass with one language missed.
		for _, lang := range slices.Sorted(maps.Keys(byLang)) {
			x := byLang[lang]
			_, _ = fmt.Fprintf(
				w,
				"  %s: TP %d FP %d FN %d, precision %.2f, recall %.2f, support %d%s\n",
				lang,
				x.TP,
				x.FP,
				x.FN,
				x.Precision,
				x.Recall,
				x.Support,
				newText(x),
			)
		}
	}
	if review {
		_, _ = fmt.Fprintf(w, "REVIEW: qualification ships disabled unless the owner records an "+
			"acceptance in the cutoff table\n")
	}
	if t.Accepted != nil {
		_, _ = fmt.Fprintf(w, "owner acceptance holds: %s\n", t.Accepted.Reason)
	}
}

// classifyFlips maps each language to the items whose decision differs across the calls.
func classifyFlips(data []*caseData, decision string, cutoff float64) map[string][]string {
	out := map[string][]string{}
	for _, d := range data {
		for _, id := range slices.Sorted(maps.Keys(d.c.Label.Items)) {
			l := d.c.Label.Items[id]
			ss := d.kindScores(id)
			if decision == classify.Duplicate {
				ss = d.dupScores(id)
			}
			if len(ss) == 0 {
				continue
			}
			first := hit(ss[0], cutoff)
			for _, p := range ss[1:] {
				if hit(p, cutoff) != first {
					out[l.Lang] = append(out[l.Lang], id)
					break
				}
			}
		}
	}
	return out
}

// newText is the new-item figures of a `qualified` list, empty for a `new` one.
func newText(e jev.EndToEnd) string {
	if e.NewSupport == nil || e.NewRecall == nil {
		return ""
	}
	return fmt.Sprintf(", new support %d, new recall %.2f", *e.NewSupport, *e.NewRecall)
}

// dupCases prints O1's per-case held-out duplicate outcomes: for each held-out correcting item its
// ID, expected label, dup_kind and undo, and its three attempts at the fitted cutoff (duplicate,
// new or missing), or, when no cutoff fits, its three scores labelled as scores.
func dupCases(w io.Writer, data []*caseData, cutoff *float64) {
	if cutoff != nil {
		_, _ = fmt.Fprintf(w, "  heldout cases at %.2f:\n", *cutoff)
	} else {
		_, _ = fmt.Fprintf(w, "  heldout cases, no cutoff fits (scores):\n")
	}
	for _, d := range data {
		if d.c.Label.Split != SplitHeldout {
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(d.c.Label.Items)) {
			l := d.c.Label.Items[id]
			if !l.Correcting() {
				continue
			}
			ss := d.dupScores(id)
			if len(ss) == 0 {
				ss = make([]*float64, Attempts)
			}
			parts := make([]string, 0, len(ss))
			for _, p := range ss {
				switch {
				case p == nil:
					parts = append(parts, "missing")
				case cutoff == nil:
					parts = append(parts, fmt.Sprintf("%.2f", *p))
				case jev.Hit(*p, *cutoff):
					parts = append(parts, classify.DupDuplicate)
				default:
					parts = append(parts, classify.DupNew)
				}
			}
			kind, label := l.DupKind, ""
			if kind == "" {
				kind = "-"
			}
			if cutoff == nil {
				label = "scores "
			}
			_, _ = fmt.Fprintf(w, "    %s: expected %s, dup_kind %s, undo %v: %s%s\n", id, l.Dup,
				kind, l.Undo, label, strings.Join(parts, ", "))
		}
	}
}
