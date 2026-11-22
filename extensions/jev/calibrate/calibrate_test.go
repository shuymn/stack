package calibrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/semantic"
	"github.com/shuymn/stack/internal/gittest"
)

const fixtures = "../../../fixtures/jev/semantic"

func TestMain(m *testing.M) {
	cleanup := buildBinaries()
	code := gittest.Main(m)
	cleanup()
	os.Exit(code)
}

// refreshCutoff is the non-0.50 cutoff TestRefreshReplays refreshes and replays at.
const refreshCutoff = "0.56"

// built holds stack, a stack-jev-semantic test build with refreshCutoff injected for both
// decisions, and in committed/ one with the committed table. Replay never reaches the endpoint;
// it is set to a closed local port all the same.
var built struct {
	dir, stack, committed string
	err                   error
}

func buildBinaries() func() {
	dir, err := os.MkdirTemp("", "jev-calibrate-test-")
	if err != nil {
		built.err = err
		return func() {}
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	built.dir, built.committed = dir, filepath.Join(dir, "committed")
	built.stack = filepath.Join(dir, "stack"+exe)
	endpoint := "-X github.com/shuymn/stack/extensions/internal/jev.endpoint=http://127.0.0.1:9"
	cutoffs := " -X github.com/shuymn/stack/extensions/jev/semantic.testCutoffs=" +
		semantic.MetaDuty + "=" + refreshCutoff + "," + semantic.Unsupported + "=" + refreshCutoff
	env := gittest.CleanEnv(os.Environ())
	for _, c := range [][]string{
		{"build", "-o", built.stack, "../../../cmd/stack"},
		{
			"build", "-ldflags", endpoint + cutoffs, "-o",
			filepath.Join(dir, "stack-jev-semantic"+exe), "../semantic/cmd/stack-jev-semantic",
		},
		{
			"build", "-ldflags", endpoint, "-o",
			filepath.Join(built.committed, "stack-jev-semantic"+exe),
			"../semantic/cmd/stack-jev-semantic",
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, "go", c...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			built.err = fmt.Errorf("go %s: %w: %s", strings.Join(c, " "), err, out)
			break
		}
	}
	return func() { _ = os.RemoveAll(dir) }
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
// answers 200 for every body, including mixed-size's second one, which its fixed fixture records
// as a size rejection.
type fakeJev struct {
	// failFirst gives the first attempt of each body of these cases a timeout with no status.
	failFirst map[string]bool
	// partial answers only meta_duty_regression for these cases.
	partial map[string]bool
	// shift moves every score of these cases.
	shift    map[string]float64
	mu       sync.Mutex
	byHash   map[string]planned
	attempts map[string]int
	t        *testing.T
}

type planned struct {
	c     Case
	files []string
}

func newFake(t *testing.T, cases []Case) *fakeJev {
	t.Helper()
	f := &fakeJev{byHash: map[string]planned{}, attempts: map[string]int{}, t: t}
	// Fixed cases are indexed too, although calibration never sends their bodies: TestCalibrate
	// then names a fixed fixture's body sent by mistake.
	for _, c := range cases {
		for _, b := range c.Plan.Batches {
			f.byHash[b.Hash] = planned{c, b.Units(c.Units)}
		}
	}
	return f
}

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
	if f.failFirst[name] && n == 0 {
		return jev.Outcome{Class: jev.Transient, Cause: jev.CauseTimeout}
	}
	if f.partial[name] {
		// Only meta_duty_regression answered: unsupported_claim is invalid.
		return jev.Outcome{
			Class: jev.OK, Status: 200, Body: []byte(`{}`),
			Answers: map[string]jev.Answer{semantic.MetaDuty: {Type: jev.TypeNoul, Noul: 0.5}},
			Invalid: map[string]string{semantic.Unsupported: jev.CauseMissing},
		}
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
		v += f.shift[name]
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
	fixedBefore := readAll(t, filepath.Join(dir, "fixed"))
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
		if fake.byHash[h].c.Fixed() {
			t.Fatalf("a fixed fixture's body %s was sent", h)
		}
	}
	// Fixed fixtures keep their synthetic answers and failures, e.g. mixed-size's 400.
	if fixedAfter := readAll(t, filepath.Join(dir, "fixed")); !maps.EqualFunc(fixedBefore,
		fixedAfter, bytes.Equal) {
		t.Fatal("calibration rewrote a fixed fixture")
	}
	recorded, err := Load(t.Context(), dir, proc.Exec{}, cleanEnv())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range recorded {
		if c.Fixed() {
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
		// The fake's negatives reach 0.16 and its positives 0.84: both rules take the 0.50 floor.
		if !d.Enabled || d.Cutoff == nil || *d.Cutoff != 0.5 || d.Baseline == nil ||
			*d.Baseline != 0.5 || got.Placeholder != "" {
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

// readAll maps each file of dir to its bytes.
func readAll(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = data
	}
	return out
}

// slowLog delays git log until its context ends, as a hung git would.
type slowLog struct{ proc.Exec }

func (s slowLog) Run(ctx context.Context, c proc.Cmd) (proc.Result, error) {
	if slices.Contains(c.Args, "log") {
		<-ctx.Done()
		return proc.Result{ExitCode: -1, TimedOut: true}, nil
	}
	return s.Exec.Run(ctx, c)
}

func TestPrepareDeadline(t *testing.T) {
	defer func(d time.Duration) { PrepareTimeout = d }(PrepareTimeout)
	PrepareTimeout = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := Load(ctx, fixtures, slowLog{}, cleanEnv())
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("Load returned %v after %v", err, time.Since(start))
	}
}

// replayResult runs `stack run outbound --ext jev-semantic --replay path` in r with binDir first
// on PATH and returns the extension's result.
func replayResult(t *testing.T, r *gittest.Repo, binDir, path string) jev.Response {
	t.Helper()
	if built.err != nil {
		t.Fatalf("building the test binaries failed: %v", built.err)
	}
	env := slices.DeleteFunc(slices.Clone(r.Env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, "HOME") || strings.EqualFold(k, "USERPROFILE") ||
			strings.EqualFold(k, "PATH")
	})
	home := t.TempDir()
	sep := string(os.PathListSeparator)
	env = append(env, "HOME="+home, "USERPROFILE="+home,
		"PATH="+binDir+sep+built.dir+sep+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, built.stack, "run", "outbound", "--ext", "jev-semantic",
		"--replay", path)
	cmd.Dir, cmd.Env = r.Dir, env
	out, err := cmd.Output()
	if err != nil && cmd.ProcessState.ExitCode() < 0 {
		t.Fatal(err)
	}
	var agg struct {
		Results []struct {
			Status   string        `json:"status"`
			Context  string        `json:"context"`
			Findings []jev.Finding `json:"findings"`
			Error    string        `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &agg); err != nil || len(agg.Results) != 1 ||
		agg.Results[0].Error != "" {
		t.Fatalf("stack run: %s (%v)", out, err)
	}
	x := agg.Results[0]
	return jev.Response{Status: x.Status, Context: x.Context, Findings: x.Findings}
}

// TestRefreshReplays refreshes a cal case at a cutoff other than the fixed fixtures' test
// cutoff, and with the committed table, and replays each through the build that carries those
// cutoffs: the replay must give exactly the refreshed expect and want.
func TestRefreshReplays(t *testing.T) {
	dir := copyFixtures(t)
	cases, err := Load(t.Context(), dir, proc.Exec{}, cleanEnv())
	if err != nil {
		t.Fatal(err)
	}
	var c *Case
	for i := range cases {
		if filepath.Base(cases[i].Path) == "en-deleted-assertion-claims-tests.json" {
			c = &cases[i]
		}
	}
	fake := newFake(t, cases)
	ok, err := record(t.Context(), Options{Transport: fake, Stdout: io.Discard}, c)
	if err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	var spec semantic.RepoSpec
	if err := json.Unmarshal(c.Fixture.Repo, &spec); err != nil {
		t.Fatal(err)
	}
	r := gittest.New(t)
	if err := spec.Build(gittest.Ops{Repo: r}); err != nil {
		t.Fatal(err)
	}
	cutoff, err := strconv.ParseFloat(refreshCutoff, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		name      string
		decisions map[string]jev.Decision
		binDir    string
	}{
		{"cutoff " + refreshCutoff, map[string]jev.Decision{
			semantic.MetaDuty:    {Enabled: true, Cutoff: &cutoff},
			semantic.Unsupported: {Enabled: true, Cutoff: &cutoff},
		}, built.dir},
		{"committed table", semantic.Decisions(), built.committed},
	} {
		t.Run(v.name, func(t *testing.T) {
			if err := Refresh(c, v.decisions); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "case.json")
			if err := jev.WriteFixture(path, c.Fixture); err != nil {
				t.Fatal(err)
			}
			got := replayResult(t, r, v.binDir, path)
			want := c.Fixture.Want
			if got.Status != c.Fixture.Expect.Status || got.Context != want.Context ||
				!slices.Equal(got.Findings, want.Findings) {
				t.Fatalf("replay %+v\nrefreshed %s %+v", got, c.Fixture.Expect.Status, *want)
			}
		})
	}
}

// TestRecalibrateFailure: when a calibrated case's refetch has no replayable first outcome, a
// re-calibration writes neither the new table nor any fixture, and says a rerun is needed.
func TestRecalibrateFailure(t *testing.T) {
	dir := copyFixtures(t)
	cases, err := Load(t.Context(), dir, proc.Exec{}, cleanEnv())
	if err != nil {
		t.Fatal(err)
	}
	table := filepath.Join(dir, "cutoffs.json")
	opts := Options{
		Fixtures: dir, Table: table, Transport: newFake(t, cases),
		Runner: proc.Exec{}, Environ: cleanEnv(), Stdout: io.Discard,
	}
	if err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	calBefore := readAll(t, filepath.Join(dir, "cal"))
	tableBefore, err := os.ReadFile(table)
	if err != nil {
		t.Fatal(err)
	}
	// The last case fails; the first one, recorded before it, now scores differently, so writing
	// it before the failure is seen would change it.
	again := newFake(t, cases)
	again.shift = map[string]float64{"en-deleted-assertion-claims-tests.json": 0.05}
	again.failFirst = map[string]bool{"ja-vet-dropped-speedup.json": true}
	opts.Transport = again
	err = Run(t.Context(), opts)
	if err == nil || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("Run returned %v", err)
	}
	tableAfter, err := os.ReadFile(table)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tableBefore, tableAfter) ||
		!maps.EqualFunc(calBefore, readAll(t, filepath.Join(dir, "cal")), bytes.Equal) {
		t.Fatal("a failed re-calibration wrote the table or a fixture")
	}
}

// TestUnansweredSample: a question a valid answer leaves out is sampled as null, not 0.
func TestUnansweredSample(t *testing.T) {
	cases, err := Load(t.Context(), fixtures, proc.Exec{}, cleanEnv())
	if err != nil {
		t.Fatal(err)
	}
	var c *Case
	for i := range cases {
		if filepath.Base(cases[i].Path) == "en-feature-with-test.json" {
			c = &cases[i]
		}
	}
	fake := newFake(t, cases)
	fake.partial = map[string]bool{"en-feature-with-test.json": true}
	ok, err := record(t.Context(), Options{Transport: fake, Stdout: io.Discard}, c)
	if err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	for _, b := range c.Plan.Batches {
		for _, p := range c.Fixture.Samples[b.Hash][semantic.Unsupported] {
			if p != nil {
				t.Fatalf("unanswered question sampled as %v", *p)
			}
		}
		for _, p := range c.Fixture.Samples[b.Hash][semantic.MetaDuty] {
			if p == nil || *p != 0.5 {
				t.Fatalf("answered question sampled as %v", p)
			}
		}
	}
}

// synthCase is a one-batch cal case with recorded scores: unsupported_claim answers scores, and
// meta_duty_regression a constant low score on a case with no labelled file.
func synthCase(name, lang, split string, unsupported bool, scores ...float64) Case {
	hash := "sha256:" + name
	qs := []jev.Question{{Name: semantic.MetaDuty}, {Name: semantic.Unsupported}}
	low := 0.1
	samples := map[string][]*float64{semantic.MetaDuty: {&low, &low, &low}}
	for _, s := range scores {
		samples[semantic.Unsupported] = append(samples[semantic.Unsupported], &s)
	}
	return Case{
		Path:    filepath.Join("cal", name+".json"),
		Fixture: jev.Fixture{Samples: map[string]map[string][]*float64{hash: samples}},
		Label:   semantic.Label{Unsupported: unsupported, Split: split, Lang: lang},
		Plan:    jev.Plan{Batches: []jev.Batch{{Hash: hash, Questions: qs}}},
	}
}

func TestReport(t *testing.T) {
	cases := make([]Case, 0, 18)
	for i, lang := range []string{"en", "en", "en", "ja", "ja"} {
		cases = append(cases, synthCase(fmt.Sprintf("%s-pos-%d", lang, i), lang, SplitFit, true,
			0.9, 0.9, 0.9))
	}
	// Under the margin test 0.12 is a miss; at the runtime cutoff it flips the case's decision.
	cases = append(cases, synthCase("ja-flip", "ja", SplitFit, true, 0.9, 0.9, 0.12))
	for i, lang := range []string{"en", "en", "en", "ja", "ja", "ja"} {
		cases = append(cases, synthCase(fmt.Sprintf("%s-neg-%d", lang, i), lang, SplitFit, false,
			0.1, 0.1, 0.1))
	}
	for i, lang := range []string{"en", "ja", "ja"} {
		cases = append(cases,
			synthCase(fmt.Sprintf("%s-hpos-%d", lang, i), lang, SplitHeldout, true, 0.9, 0.9, 0.9),
			synthCase(fmt.Sprintf("%s-hneg-%d", lang, i), lang, SplitHeldout, false, 0.1, 0.1, 0.1))
	}
	var out bytes.Buffer
	Report(&out, cases, Refit(cases))
	got := out.String()
	for _, want := range []string{
		"unsupported_claim: enabled true, cutoff 0.50, baseline 0.50 (old rule)",
		"  fit at 0.50: TP 17 FP 0 FN 1 TN 18, precision 1.00, recall 0.94, 6 + 6 cases",
		"  heldout at 0.50: TP 9 FP 0 FN 0 TN 9, precision 1.00, recall 1.00, 3 + 3 cases",
		"  margin test (±0.05) on fit: TP 17 FP 0 FN 1 TN 18, precision 1.00, recall 0.94, 6 + 6 cases: pass",
		"  baseline 0.50: fit TP 17 FP 0 FN 1 TN 18",
		"  flips across calls at 0.50: en none; ja ja-flip",
		"meta_duty_regression: enabled false, cutoff none, baseline none (old rule)",
		"  REVIEW: no cutoff passes the margin test",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report lacks %q:\n%s", want, got)
		}
	}
}
