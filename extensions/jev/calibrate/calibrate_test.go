package calibrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/semantic"
	"github.com/shuymn/stack/internal/gittest"
)

const fixtures = "../../../fixtures/jev/semantic"

func TestMain(m *testing.M) {
	os.Exit(gittest.Main(m))
}

func cleanEnv() []string { return gittest.CleanEnv(os.Environ()) }

// copyFixtures copies fixtures/jev/semantic into a temporary directory.
func copyFixtures(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	for _, kind := range []string{"fixed", "cal"} {
		paths, err := filepath.Glob(filepath.Join(fixtures, kind, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dst, kind), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dst, kind, filepath.Base(p))
			err = os.WriteFile(out, data, 0o600) //nolint:gosec // G703: a temp copy of a fixture
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return dst
}

// fakeJev answers each planned body from the case labels, with a small spread per attempt; it
// rejects mixed-size's second body for size and gives one fixture's first attempt no status.
type fakeJev struct {
	mu       sync.Mutex
	byHash   map[string]planned
	attempts map[string]int
	t        *testing.T
}

type planned struct {
	c     Case
	batch int
	files []string
}

func newFake(t *testing.T, cases []Case) *fakeJev {
	t.Helper()
	f := &fakeJev{byHash: map[string]planned{}, attempts: map[string]int{}, t: t}
	for _, c := range cases {
		for i, b := range c.Plan.Batches {
			f.byHash[b.Hash] = planned{c, i, batchFiles(b, c.Units)}
		}
	}
	return f
}

const unreplayable = "clean.json"

func (f *fakeJev) Send(_ context.Context, b jev.Batch) jev.Outcome {
	f.mu.Lock()
	n := f.attempts[b.Hash]
	f.attempts[b.Hash]++
	f.mu.Unlock()
	p, ok := f.byHash[b.Hash]
	if !ok {
		f.t.Errorf("unplanned body %s", b.Hash)
		return jev.Outcome{Class: jev.Persistent, Cause: "unplanned"}
	}
	name := filepath.Base(p.c.Path)
	switch {
	case name == unreplayable && n == 0:
		return jev.Outcome{Class: jev.Transient, Cause: jev.CauseNetwork}
	case name == "mixed-size.json" && p.batch == 1:
		return jev.Outcome{Class: jev.TooLarge, Status: 400, Cause: jev.CauseMaxTokens}
	}
	spread := []float64{0, 0.01, -0.01}[n%3]
	answers := map[string]any{}
	for _, q := range b.Questions {
		truth := p.c.Label.Unsupported
		if q.Name == semantic.MetaDuty {
			truth = p.c.Label.MetaTruth(p.files)
		}
		v := 0.15 + spread
		if truth {
			v = 0.85 + spread
		}
		answers[q.Name] = map[string]any{"type": "noul", "noul": v}
	}
	body, err := jev.Marshal(map[string]any{
		"model": jev.Pin, "answers": answers,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
	})
	if err != nil {
		f.t.Error(err)
	}
	o := jev.Validate(body, b.Questions)
	o.Status, o.Body = 200, body
	return o
}

//nolint:gocyclo // one run, then each of its effects checked in turn
func TestCalibrate(t *testing.T) {
	dir := copyFixtures(t)
	cases, err := Load(t.Context(), dir, proc.Exec{}, cleanEnv())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "fixed", unreplayable))
	if err != nil {
		t.Fatal(err)
	}
	fake := newFake(t, cases)
	table := filepath.Join(dir, "cutoffs.json")
	var stdout bytes.Buffer
	err = Run(t.Context(), Options{
		Fixtures: dir, Table: table, Transport: fake,
		Runner: proc.Exec{}, Environ: cleanEnv(), Stdout: &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}

	for h, n := range fake.attempts {
		if n != Attempts {
			t.Fatalf("body %s sent %d times", h, n)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, "fixed", unreplayable))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) ||
		!strings.Contains(stdout.String(), unreplayable+": left unchanged") {
		t.Fatalf("the unreplayable fixture changed or was not reported: %s", stdout.String())
	}

	recorded, err := Load(t.Context(), dir, proc.Exec{}, cleanEnv())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range recorded {
		if filepath.Base(c.Path) == unreplayable {
			continue
		}
		replay, err := c.Fixture.RequestRecords()
		if err != nil {
			t.Fatal(err)
		}
		planned := make([]string, 0, len(c.Plan.Batches))
		for _, b := range c.Plan.Batches {
			planned = append(planned, b.Hash)
			for _, q := range b.Questions {
				if len(c.Fixture.Samples[b.Hash][q.Name]) != Attempts {
					t.Fatalf("%s: samples %v", c.Path, c.Fixture.Samples[b.Hash])
				}
			}
		}
		if got := slices.Sorted(maps.Keys(replay.Records)); !slices.Equal(got,
			slices.Sorted(slices.Values(planned))) {
			t.Fatalf("%s: records %v, planned %v", c.Path, got, planned)
		}
		if c.Fixture.Placeholder != "" {
			t.Fatalf("%s: placeholder kept", c.Path)
		}
		if filepath.Base(c.Path) == "mixed-size.json" {
			rec := replay.Records[c.Plan.Batches[1].Hash]
			if rec.Status != 400 || rec.Error != jev.CauseMaxTokens {
				t.Fatalf("size rejection recorded as %+v", rec)
			}
		}
		if c.Fixed() {
			continue
		}
		if c.Fixture.Expect == nil || c.Fixture.Want == nil {
			t.Fatalf("%s: expect or want not refreshed", c.Path)
		}
		wantHit := c.Label.Unsupported ||
			c.Label.MetaTruth(slices.Collect(maps.Keys(c.Label.Files)))
		if (c.Fixture.Expect.Status == jev.StatusWarn) != wantHit {
			t.Fatalf("%s: expect %s for label %+v", c.Path, c.Fixture.Expect.Status, c.Label)
		}
	}

	data, err := os.ReadFile(table)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jev.ParseTable(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range semantic.Questions {
		d := got.Decisions[q.Name]
		if !d.Enabled || d.Cutoff == nil || *d.Cutoff != 0.5 || got.Placeholder != "" {
			t.Fatalf("%s: %+v", q.Name, d)
		}
	}
	// Refitting from the recorded samples gives the written table.
	refit, err := json.Marshal(Refit(recorded))
	if err != nil {
		t.Fatal(err)
	}
	written, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(refit, written) {
		t.Fatalf("refit %s\nwritten %s", refit, written)
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

// TestBuilders checks that jev-calibrate's own repository builder gives the bodies of the tests'
// gittest repositories, whose dates differ, for every fixture.
func TestBuilders(t *testing.T) {
	cases, err := Load(t.Context(), fixtures, proc.Exec{}, cleanEnv())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		var spec semantic.RepoSpec
		var req jev.Request
		if err := json.Unmarshal(c.Fixture.Repo, &spec); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(c.Fixture.Request, &req); err != nil {
			t.Fatal(err)
		}
		r := gittest.New(t)
		r.Env = append(r.Env, "GIT_AUTHOR_DATE=2031-05-06T07:08:09Z",
			"GIT_COMMITTER_DATE=2031-05-06T07:08:09Z")
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
		plan, err := jev.Pack(semantic.Input(semantic.Texts{
			Title: req.Title, Body: req.Body,
			Branch: req.Branch,
		}, st))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Batches) != len(c.Plan.Batches) {
			t.Fatalf("%s: %d batches, want %d", c.Path, len(plan.Batches), len(c.Plan.Batches))
		}
		for i, b := range plan.Batches {
			if b.Hash != c.Plan.Batches[i].Hash {
				t.Fatalf("%s: body %d differs between the builders", c.Path, i)
			}
		}
	}
}
