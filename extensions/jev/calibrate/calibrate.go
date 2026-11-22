// Package calibrate is jev-calibrate (docs/design/phase4-jev.md §3.5), run with the
// key: it builds each fixture's synthetic repository and requests with jev-semantic's own
// builders, sends every planned body through the transport three times, records the first
// outcome and all scores in the fixture, refits the cutoffs on the cal cases and refreshes the
// cal fixtures' expect and want.
package calibrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/semantic"
)

// Attempts is how many times each body is sent.
const Attempts = 3

// SendTimeout bounds each attempt.
const SendTimeout = time.Minute

// Targets are the precision targets of the semantic decisions (§3.5).
var Targets = map[string]float64{
	semantic.MetaDuty:    semanticTarget,
	semantic.Unsupported: semanticTarget,
}

// semanticTarget is the precision both semantic decisions must reach.
const semanticTarget = 0.8

// Split values of a fixture label.
const (
	SplitFit     = "fit"
	SplitHeldout = "heldout"
	SplitFixed   = "fixed"
)

var errNoKey = errors.New(jev.KeyEnv + " is not set")

// Options configures a run.
type Options struct {
	// Fixtures is fixtures/jev/semantic, with fixed/ and cal/ under it.
	Fixtures string
	// Table is the cutoff table to write.
	Table     string
	Transport jev.Transport
	// Runner and Environ run git for the synthetic repositories.
	Runner  proc.Runner
	Environ []string
	Stdout  io.Writer
}

// Case is one fixture with its rebuilt plan.
type Case struct {
	Path    string
	Fixture jev.Fixture
	Label   semantic.Label
	Units   []jev.Unit
	Plan    jev.Plan
}

// Fixed reports a fixture of fixed/, which keeps its hand-written want.
func (c Case) Fixed() bool { return filepath.Base(filepath.Dir(c.Path)) == SplitFixed }

// Run records every fixture, refits the cutoffs and refreshes the cal fixtures. A fixture whose
// first attempt of some body has no replayable outcome is left unchanged and reported.
func Run(ctx context.Context, o Options) error {
	cases, err := Load(ctx, o.Fixtures, o.Runner, o.Environ)
	if err != nil {
		return err
	}
	for i := range cases {
		c := &cases[i]
		ok, err := record(ctx, o, c)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := jev.WriteFixture(c.Path, c.Fixture); err != nil {
			return err
		}
	}
	table := Refit(cases)
	if err := jev.WriteTable(o.Table, table); err != nil {
		return err
	}
	for name, d := range table.Decisions {
		_, _ = fmt.Fprintf(o.Stdout, "%s: enabled %v, cutoff %s, %s\n", name, d.Enabled,
			cutoffText(d), d.Reason)
		for _, u := range d.Unstable {
			_, _ = fmt.Fprintf(o.Stdout, "  unstable %s\n", u)
		}
	}
	decisions := table.Decisions
	for i := range cases {
		c := &cases[i]
		if c.Fixed() {
			continue
		}
		if err := Refresh(c, decisions); err != nil {
			return err
		}
		if err := jev.WriteFixture(c.Path, c.Fixture); err != nil {
			return err
		}
	}
	return nil
}

func cutoffText(d jev.Decision) string {
	if d.Cutoff == nil {
		return "none"
	}
	return fmt.Sprintf("%.2f", *d.Cutoff)
}

// Load reads every fixture under dir and rebuilds its plan from its synthetic repository.
func Load(ctx context.Context, dir string, runner proc.Runner, environ []string) ([]Case, error) {
	var cases []Case
	for _, kind := range []string{SplitFixed, "cal"} {
		paths, err := filepath.Glob(filepath.Join(dir, kind, "*.json"))
		if err != nil {
			return nil, fmt.Errorf("list fixtures: %w", err)
		}
		for _, p := range paths {
			c, err := loadCase(ctx, p, runner, environ)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			cases = append(cases, c)
		}
	}
	return cases, nil
}

func loadCase(
	ctx context.Context,
	path string,
	runner proc.Runner,
	environ []string,
) (Case, error) {
	f, err := jev.ReadFixture(path)
	if err != nil {
		return Case{}, err
	}
	var req jev.Request
	var spec semantic.RepoSpec
	c := Case{Path: path, Fixture: f}
	for _, d := range []struct {
		raw json.RawMessage
		v   any
	}{{f.Request, &req}, {f.Repo, &spec}, {f.Label, &c.Label}} {
		if err := json.Unmarshal(d.raw, d.v); err != nil {
			return Case{}, fmt.Errorf("decode: %w", err)
		}
	}
	root, err := os.MkdirTemp("", "jev-calibrate-")
	if err != nil {
		return Case{}, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	repo, err := NewRepo(ctx, runner, environ, filepath.Join(root, "repo"))
	if err != nil {
		return Case{}, err
	}
	if err := spec.Build(repo); err != nil {
		return Case{}, err
	}
	g := semantic.Git{Runner: runner, Dir: repo.Dir, Env: environ}
	st, err := semantic.Prepare(ctx, g, semantic.FixtureRange)
	if err != nil {
		return Case{}, fmt.Errorf("prepare: %w", err)
	}
	c.Units = st.Units
	c.Plan, err = jev.Pack(semantic.Input(semantic.Texts{
		Title: req.Title, Body: req.Body, Branch: req.Branch,
	}, st))
	if err != nil {
		return Case{}, err
	}
	return c, nil
}

// record sends each planned body Attempts times and sets the fixture's records and samples. ok
// is false when a first attempt has no replayable outcome; the fixture is then left unchanged.
func record(ctx context.Context, o Options, c *Case) (bool, error) {
	records := map[string]jev.Record{}
	samples := map[string]map[string][]*float64{}
	for _, b := range c.Plan.Batches {
		var first jev.Outcome
		scores := map[string][]*float64{}
		for i := range Attempts {
			sctx, cancel := context.WithTimeout(ctx, SendTimeout)
			out := o.Transport.Send(sctx, b)
			cancel()
			if i == 0 {
				first = out
			}
			for _, q := range b.Questions {
				var p *float64
				if out.Class == jev.OK {
					v := out.Answers[q.Name].Noul
					p = &v
				}
				scores[q.Name] = append(scores[q.Name], p)
			}
		}
		rec, ok := replayable(first)
		if !ok {
			_, _ = fmt.Fprintf(o.Stdout, "%s: left unchanged: the first attempt of %s has no "+
				"replayable outcome (%s)\n", c.Path, b.Hash, first.Describe())
			return false, nil
		}
		records[b.Hash] = rec
		samples[b.Hash] = scores
	}
	if err := c.Fixture.SetRecords(records); err != nil {
		return false, err
	}
	c.Fixture.Samples = samples
	c.Fixture.Placeholder = ""
	return true, nil
}

// replayable is the record of a first outcome: its 200 body, or its HTTP status. An outcome
// without a status (a connect error, a timeout before headers) has none.
func replayable(o jev.Outcome) (jev.Record, bool) {
	switch {
	case o.Status == 200 && o.Body != nil:
		return jev.Record{Answer: json.RawMessage(o.Body)}, true
	case o.Status != 0 && o.Status != 200:
		rec := jev.Record{Status: o.Status}
		if o.Cause == jev.CauseMaxTokens {
			rec.Error = jev.CauseMaxTokens
		}
		return rec, true
	default:
		return jev.Record{}, false
	}
}

// Refit fits each decision on the cal cases' fit samples and checks it on their heldout samples.
// Each single-call score is one sample; a planned call with no score is a negative prediction. A
// case whose plan splits a file is fixed-only and not fitted.
func Refit(cases []Case) jev.Table {
	fit := map[string][]jev.Sample{}
	held := map[string][]jev.Sample{}
	for _, c := range cases {
		if c.Fixed() || c.Fixture.Samples == nil || splits(c.Plan) {
			continue
		}
		dst := fit
		if c.Label.Split == SplitHeldout {
			dst = held
		}
		name := strings.TrimSuffix(filepath.Base(c.Path), ".json")
		for _, b := range c.Plan.Batches {
			files := batchFiles(b, c.Units)
			for _, q := range b.Questions {
				truth := c.Label.Unsupported
				if q.Name == semantic.MetaDuty {
					truth = c.Label.MetaTruth(files)
				}
				for _, p := range c.Fixture.Samples[b.Hash][q.Name] {
					dst[q.Name] = append(
						dst[q.Name],
						jev.Sample{Case: name, Score: p, Truth: truth},
					)
				}
			}
		}
		// The whole-PR question not asked on a case that did not fit is a planned call with
		// no score.
		for _, u := range c.Plan.Unasked {
			if u.Question == semantic.Unsupported {
				for range Attempts {
					dst[u.Question] = append(dst[u.Question], jev.Sample{
						Case: name, Truth: c.Label.Unsupported,
					})
				}
			}
		}
	}
	t := jev.Table{Model: jev.Pin, Decisions: map[string]jev.Decision{}}
	for _, q := range semantic.Questions {
		t.Decisions[q.Name] = jev.Calibrate(fit[q.Name], held[q.Name], Targets[q.Name])
	}
	return t
}

func splits(p jev.Plan) bool {
	return slices.ContainsFunc(p.Batches, func(b jev.Batch) bool {
		return slices.ContainsFunc(b.Pieces, func(x jev.Piece) bool { return x.Parts > 0 })
	})
}

func batchFiles(b jev.Batch, units []jev.Unit) []string {
	var files []string
	for _, p := range b.Pieces {
		if n := units[p.Unit].Name; !slices.Contains(files, n) {
			files = append(files, n)
		}
	}
	return files
}

// Refresh sets a cal fixture's expect and want to what replaying its records gives under
// decisions.
func Refresh(c *Case, decisions map[string]jev.Decision) error {
	replay, err := c.Fixture.RequestRecords()
	if err != nil {
		return err
	}
	outcomes := jev.Do(context.Background(), replay, c.Plan.Batches)
	resp := semantic.Respond(c.Plan, c.Units, outcomes, decisions, nil)
	c.Fixture.Expect = &jev.Expect{Status: resp.Status}
	c.Fixture.Want = &jev.Want{Findings: resp.Findings, Context: resp.Context}
	return nil
}

// Key returns the key from getenv, or an error naming the variable.
func Key(getenv func(string) string) (string, error) {
	if k := getenv(jev.KeyEnv); k != "" {
		return k, nil
	}
	return "", errNoKey
}
