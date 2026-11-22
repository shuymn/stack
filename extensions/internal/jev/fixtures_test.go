package jev_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
			if err := spec.Build(gitOps{r}); err != nil {
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

// checkRefit is the gate on the first calibration run: it fails while the cutoff table is a
// placeholder or any cal case is pending, and afterwards while the committed table differs from
// the refit of the recorded samples.
func checkRefit(t *testing.T, cases []calibrate.Case, table jev.Table, pending int) {
	t.Helper()
	if table.Placeholder != "" || pending != 0 {
		t.Errorf("not calibrated: the cutoff table is a placeholder (%v) and %d cal cases are "+
			"pending; from the repository root run\n\t%s\nwhich records "+
			"fixtures/jev/semantic/cal/*.json (records, samples, expect, want) and rewrites "+
			"extensions/jev/semantic/cutoffs.json", table.Placeholder != "", pending,
			calibrateCommand)
		return
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

// gitOps builds a RepoSpec with gittest.
type gitOps struct{ r *gittest.Repo }

func (o gitOps) Write(name, content string) error {
	o.r.Write(name, content)
	return nil
}

func (o gitOps) Remove(name string) error {
	if err := os.Remove(filepath.Join(o.r.Dir, filepath.FromSlash(name))); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

func (o gitOps) Git(args ...string) error {
	o.r.Git(args...)
	return nil
}
