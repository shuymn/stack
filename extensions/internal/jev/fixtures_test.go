package jev_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/calibrate"
	"github.com/shuymn/stack/extensions/jev/semantic"
	"github.com/shuymn/stack/internal/gittest"
)

const (
	semanticFixtures = "../../../fixtures/jev/semantic"
	semanticTable    = "../../jev/semantic/cutoffs.json"
)

// TestFixturesPinned is C11: records and the cutoff table name the pin, records match rebuilt
// bodies, the committed cutoffs are the refit of the recorded samples, and the body keys depend
// on the input and the pin only.
func TestFixturesPinned(t *testing.T) {
	env := gittest.CleanEnv(os.Environ())
	cases, err := calibrate.Load(t.Context(), semanticFixtures, proc.Exec{}, env)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(semanticTable)
	if err != nil {
		t.Fatal(err)
	}
	table, err := jev.ParseTable(data)
	if err != nil {
		t.Fatal(err)
	}

	pending := 0
	for _, c := range cases {
		replay, err := c.Fixture.RequestRecords()
		if err != nil {
			t.Fatalf("%s: %v", c.Path, err)
		}
		if c.Fixture.Pending() {
			pending++
			if len(replay.Records) != 0 || c.Fixture.Samples != nil {
				t.Fatalf("%s: pending with records or samples", c.Path)
			}
			continue
		}
		planned := make([]string, 0, len(c.Plan.Batches))
		for _, b := range c.Plan.Batches {
			planned = append(planned, b.Hash)
		}
		slices.Sort(planned)
		if got := slices.Sorted(maps.Keys(replay.Records)); !slices.Equal(got, planned) {
			t.Fatalf("%s: records %v do not match the rebuilt bodies %v", c.Path, got, planned)
		}
		for h, rec := range replay.Records {
			if rec.Answer == nil {
				continue
			}
			var a struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(rec.Answer, &a); err != nil || a.Model != jev.Pin {
				t.Fatalf("%s: record %s names %q (%v)", c.Path, h, a.Model, err)
			}
		}
	}

	t.Run("keys follow the state, the questions and the pin only", func(t *testing.T) {
		again, err := calibrate.Load(t.Context(), semanticFixtures, proc.Exec{}, env)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range cases {
			for j, b := range c.Plan.Batches {
				if again[i].Plan.Batches[j].Hash != b.Hash {
					t.Fatalf("%s: another root changed body %d", c.Path, j)
				}
			}
		}
		// The clock alone: the same spec committed at another date gives the same bodies.
		for _, date := range []string{"2026-01-01T00:00:00Z", "2033-03-03T03:03:03Z"} {
			var spec semantic.RepoSpec
			if err := json.Unmarshal(cases[0].Fixture.Repo, &spec); err != nil {
				t.Fatal(err)
			}
			r := gittest.New(t)
			r.Env = append(r.Env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
			if err := spec.Build(gittest.Ops{Repo: r}); err != nil {
				t.Fatal(err)
			}
			st, err := semantic.Prepare(t.Context(), semantic.Git{
				Runner: proc.Exec{}, Dir: r.Dir,
				Env: r.Env,
			}, semantic.FixtureRange)
			if err != nil {
				t.Fatal(err)
			}
			var req jev.Request
			if err := json.Unmarshal(cases[0].Fixture.Request, &req); err != nil {
				t.Fatal(err)
			}
			plan, err := jev.Pack(semantic.Input(semantic.Texts{
				Title: req.Title, Body: req.Body,
				Branch: req.Branch,
			}, st))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Batches[0].Hash != cases[0].Plan.Batches[0].Hash {
				t.Fatalf("the commit date %s changed the body", date)
			}
		}
		units := cases[0].Units
		base := semantic.Input(semantic.Texts{Title: "t"}, semantic.State{Units: units})
		key := func(in jev.Input) string {
			plan, err := jev.Pack(in)
			if err != nil {
				t.Fatal(err)
			}
			return plan.Batches[0].Hash
		}
		k0 := key(base)
		if key(base) != k0 {
			t.Fatal("the same input gave another key")
		}
		state := semantic.Input(semantic.Texts{Title: "t2"}, semantic.State{Units: units})
		question := base
		question.Questions = slices.Clone(base.Questions)
		question.Questions[0].Instructions += " "
		pin := base
		pin.Model = "jev-1.13.0"
		for name, in := range map[string]jev.Input{
			"state": state, "question": question,
			"pin": pin,
		} {
			if key(in) == k0 {
				t.Fatalf("changing the %s kept the key", name)
			}
		}
	})

	// Last, so every check above has run: the merge gate on the first calibration run.
	checkRefit(t, cases, table, pending)
}

// calibrateCommand is what records the cal cases and writes the cutoff table, from the
// repository root.
const calibrateCommand = "TYPESAFE_API_KEY=<key> mise exec -- go run ./extensions/jev/cmd/jev-calibrate"

// Gate actions on an uncalibrated corpus.
const (
	gateFail = "fail"
	gateSkip = "skip"
)

// calibrationGate decides the gate on the first calibration run: while the cutoff table is a
// placeholder or any cal case is pending it fails in CI (ci is the CI variable, "true" on GitHub
// Actions) and skips elsewhere, so the pre-push check still passes; msg names the command to run.
// It returns the action, "" once calibrated, and the message.
func calibrationGate(table jev.Table, pending int, ci string) (string, string) {
	if table.Placeholder == "" && pending == 0 {
		return "", ""
	}
	msg := fmt.Sprintf("not calibrated: the cutoff table is a placeholder (%v) and %d cal cases "+
		"are pending; from the repository root run\n\t%s\nwhich records "+
		"fixtures/jev/semantic/cal/*.json (records, samples, expect, want) and rewrites "+
		"extensions/jev/semantic/cutoffs.json", table.Placeholder != "", pending,
		calibrateCommand)
	if ci == "true" {
		return gateFail, msg
	}
	return gateSkip, msg
}

// checkRefit applies the calibration gate, then requires the committed table to be the refit of
// the recorded samples.
func checkRefit(t *testing.T, cases []calibrate.Case, table jev.Table, pending int) {
	t.Helper()
	switch action, msg := calibrationGate(table, pending, os.Getenv("CI")); action {
	case gateFail:
		t.Error(msg)
		return
	case gateSkip:
		t.Skip(msg)
	}
	refit, err := json.Marshal(calibrate.Refit(cases))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	if string(refit) != string(committed) {
		t.Fatalf("refit %s\ncommitted %s", refit, committed)
	}
}

func TestCalibrationGate(t *testing.T) {
	placeholder := jev.Table{Model: jev.Pin, Placeholder: "not yet calibrated"}
	calibrated := jev.Table{Model: jev.Pin}
	for _, c := range []struct {
		name    string
		table   jev.Table
		pending int
		ci      string
		want    string
	}{
		{"placeholder table in CI fails", placeholder, 18, "true", gateFail},
		{"placeholder table alone in CI fails", placeholder, 0, "true", gateFail},
		{"pending cases in CI fail", calibrated, 1, "true", gateFail},
		{"placeholder table locally skips", placeholder, 18, "", gateSkip},
		{"calibrated corpus passes in CI", calibrated, 0, "true", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			action, msg := calibrationGate(c.table, c.pending, c.ci)
			if action != c.want {
				t.Fatalf("action %q, want %q", action, c.want)
			}
			if action != "" && !strings.Contains(msg, calibrateCommand) {
				t.Fatalf("message %q does not name the command", msg)
			}
		})
	}
}

// corpusCount is one decision's cal cases of one split and class.
type corpusCount struct{ en, ja, hard int }

// TestCorpus is §3.5's corpus: per decision 12 positive and 12 negative fit cases and 8 + 8
// heldout ones, each class balanced between Japanese and English and half hard, and every group
// of translations or near-duplicates in one split.
func TestCorpus(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(semanticFixtures, "cal", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]corpusCount{}
	groups := map[string]string{}
	for _, p := range paths {
		f, err := jev.ReadFixture(p)
		if err != nil {
			t.Fatal(err)
		}
		var l semantic.Label
		if err := json.Unmarshal(f.Label, &l); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if l.Why == "" || (l.Split != calibrate.SplitFit && l.Split != calibrate.SplitHeldout) {
			t.Fatalf("%s: label %+v", p, l)
		}
		if g, ok := groups[l.Group]; l.Group != "" && ok && g != l.Split {
			t.Fatalf("%s: group %s spans %s and %s", p, l.Group, g, l.Split)
		}
		groups[l.Group] = l.Split
		for _, q := range semantic.Questions {
			truth := l.Unsupported
			if q.Name == semantic.MetaDuty {
				truth = l.MetaTruth(slices.Collect(maps.Keys(l.Files)))
			}
			key := fmt.Sprintf("%s %s %v", q.Name, l.Split, truth)
			c := got[key]
			switch l.Lang {
			case "en":
				c.en++
			case "ja":
				c.ja++
			default:
				t.Fatalf("%s: lang %q", p, l.Lang)
			}
			if slices.Contains(l.Hard, q.Name) {
				c.hard++
			}
			got[key] = c
		}
		for _, h := range l.Hard {
			if !slices.ContainsFunc(semantic.Questions, func(q jev.Question) bool {
				return q.Name == h
			}) {
				t.Fatalf("%s: hard names %q", p, h)
			}
		}
	}
	fit := corpusCount{en: 6, ja: 6, hard: 6}
	held := corpusCount{en: 4, ja: 4, hard: 4}
	want := map[string]corpusCount{}
	for _, q := range semantic.Questions {
		for _, truth := range []bool{true, false} {
			want[fmt.Sprintf("%s fit %v", q.Name, truth)] = fit
			want[fmt.Sprintf("%s heldout %v", q.Name, truth)] = held
		}
	}
	// The seven straightforward meta-duty negatives of the first corpus are all in fit, and fit
	// holds six Japanese and six English negatives: one hard case short of half.
	want[semantic.MetaDuty+" fit false"] = corpusCount{en: 6, ja: 6, hard: 5}
	if !maps.Equal(got, want) {
		t.Fatalf("corpus %v, want %v", got, want)
	}
}
