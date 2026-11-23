package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/gittest"
)

const fixtureDir = "../../../fixtures/jev/classify"

// clsFixture is a classify fixture on disk.
type clsFixture struct {
	path string
	f    jev.Fixture
}

func (c clsFixture) name() string {
	return filepath.Base(filepath.Dir(c.path)) + "/" + filepath.Base(c.path)
}

// fixed reports a fixture matched by the §3.5 fixed rule rather than exactly.
func (c clsFixture) fixed() bool { return filepath.Base(filepath.Dir(c.path)) == "fixed" }

func loadFixtures(t *testing.T) []clsFixture {
	t.Helper()
	var out []clsFixture
	for _, kind := range []string{"fixed", "cal"} {
		paths, err := filepath.Glob(filepath.Join(fixtureDir, kind, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			f, err := jev.ReadFixture(p)
			if err != nil {
				t.Fatal(err)
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, clsFixture{path: abs, f: f})
		}
	}
	return out
}

// decisions are those of the build a fixture runs under: a cal fixture's is the committed table
// it was recorded with, a fixed fixture's a test build. An unknown build name has none.
func (c clsFixture) decisions() map[string]jev.Decision {
	switch {
	case !c.fixed():
		return Decisions()
	case c.f.Build != "":
		if cutoffs, ok := testBuilds[c.f.Build]; ok {
			return ParseTestCutoffs(cutoffs)
		}
		return nil
	}
	return ParseTestCutoffs(defaultTestCutoffs)
}

// materialize builds a fixture's disk inputs in a fresh fixture repository root and returns the
// root (symlinks resolved) and the allowlisted git environment of that repository.
func materialize(t *testing.T, f jev.Fixture) (string, []string) {
	t.Helper()
	r := gittest.New(t)
	git := func(dir string, args ...string) error {
		res, err := proc.Exec{}.Run(t.Context(), proc.Cmd{
			Name: "git",
			Dir:  dir,
			Env:  r.Env,
			Args: append(
				[]string{"-c", "core.autocrlf=false", "-c", "commit.gpgsign=false"},
				args...),
		})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("git %v in %s: %v exit %d: %s", args, dir, err, res.ExitCode, res.Stderr)
		}
		return nil
	}
	root, err := Materialize(f, r.Root, git)
	if err != nil {
		t.Fatal(err)
	}
	return root, r.Env
}

// replayResult is one `stack run cycle --replay` and the report it wrote.
type replayResult struct {
	code           int
	status         string
	context        string
	findings       []jev.Finding
	report         json.RawMessage
	stdout, stderr string
}

// replay runs the built stack over the fixture at path in root with the given extra environment
// (and the PATH entry of the build a fixture names), a temporary home, and reads the report.
func replay(t *testing.T, path, root string, env []string) replayResult {
	t.Helper()
	home := realDir(t)
	res := runBinary(t, home, root, env, built.stack, "run", "cycle", "--ext", "jev-classify",
		"--replay", path)
	agg := parseAggregate(t, res.stdout)
	if len(agg.Results) != 1 || agg.Results[0].Name != "jev-classify" ||
		agg.Results[0].Error != "" {
		t.Fatalf("aggregate %s (stderr %q)", res.stdout, res.stderr)
	}
	// Replay never writes a ledger line, and writes only the report under the home directory.
	if _, err := os.Stat(
		filepath.Join(home, ".local", "state", "stack", "jev"),
	); !os.IsNotExist(
		err,
	) {
		t.Fatalf("replay wrote a ledger: %v", err)
	}
	out := replayResult{
		code: res.code, status: agg.Results[0].Status, context: agg.Results[0].Context,
		stdout: res.stdout, stderr: res.stderr,
	}
	if err := json.Unmarshal(agg.Results[0].Findings, &out.findings); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(reportPath(home)); err == nil {
		out.report = data
	}
	return out
}

// env is extra with the PATH entry for the build a fixture runs under: the committed-cutoff build
// for a cal fixture, the named test build where a fixed fixture names one.
func (c clsFixture) env(extra ...string) []string {
	env := slices.Clone(extra)
	switch {
	case !c.fixed():
		env = append(env, binPath(built.committed))
	case c.f.Build != "":
		env = append(env, binPath(built.builds[c.f.Build]))
	}
	return env
}

// wantItem is the part of a report item a fixed fixture matches (§3.5).
//
//nolint:tagliatelle // the report uses snake_case keys.
type wantItem struct {
	ID        string `json:"id"`
	Where     string `json:"where"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Opener    string `json:"opener"`
	Qualified bool   `json:"qualified"`
	Kind      string `json:"kind"`
	Excerpt   string `json:"excerpt"`
	Duplicate string `json:"duplicate"`
	DupReason string `json:"dup_reason"`
}

// wantReport is the part of the report a fixed fixture matches: items by ID, complete, sources
// and counts.
type wantReport struct {
	Complete bool                      `json:"complete"`
	Sources  map[string]*ObserveSource `json:"sources"`
	Counts   Counts                    `json:"counts"`
	Items    []wantItem                `json:"items"`
}

func reduce(data json.RawMessage) (wantReport, error) {
	var r wantReport
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err //nolint:wrapcheck // a test helper
	}
	slices.SortFunc(r.Items, func(a, b wantItem) int { return strings.Compare(a.ID, b.ID) })
	if r.Items == nil {
		r.Items = []wantItem{}
	}
	return r, nil
}

// matchWant compares a replay result with the fixture's expect and want: for a fixed fixture the
// status, the finding kinds in order, the report's items by ID, complete, sources and counts, and
// the context lines; for a cal fixture the exact findings, context and normalised report. Neither
// may carry jev-unrecorded or jev-error unless wanted.
func matchWant(c clsFixture, got replayResult) string {
	f := c.f
	if f.Expect == nil || f.Want == nil {
		return "fixture has no expect or want"
	}
	if got.status != f.Expect.Status {
		return "status " + got.status + ", want " + f.Expect.Status
	}
	if got.context != f.Want.Context {
		return "context\n" + got.context + "\nwant\n" + f.Want.Context
	}
	if c.fixed() {
		kinds := func(fs []jev.Finding) []string {
			out := make([]string, 0, len(fs))
			for _, x := range fs {
				out = append(out, x.Kind)
			}
			return out
		}
		if !slices.Equal(kinds(got.findings), kinds(f.Want.Findings)) {
			return "finding kinds differ"
		}
		gr, err1 := reduce(got.report)
		wr, err2 := reduce(f.Want.Report)
		if err1 != nil || err2 != nil {
			return "report is not decodable"
		}
		if !reflect.DeepEqual(gr, wr) {
			return fmt.Sprintf("report\n%+v\nwant\n%+v", gr, wr)
		}
		return ""
	}
	if !slices.Equal(got.findings, f.Want.Findings) {
		return "findings differ"
	}
	// The run ID is random: asserted non-empty in the actual report, then blanked in both.
	norm := func(raw json.RawMessage, needID bool) any {
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			return nil
		}
		if run, ok := v["run"].(map[string]any); ok {
			if id, _ := run["id"].(string); needID && id == "" {
				return nil
			}
			run["id"] = ""
		}
		return v
	}
	if want := norm(f.Want.Report, false); want == nil ||
		!reflect.DeepEqual(norm(got.report, true), want) {
		return "the normalised report differs"
	}
	return ""
}

// capture wraps a transport and records every body it is asked to answer.
type capture struct {
	inner  jev.Transport
	mu     sync.Mutex
	bodies [][]byte
}

func (c *capture) Send(ctx context.Context, b jev.Batch) jev.Outcome {
	c.mu.Lock()
	c.bodies = append(c.bodies, b.Body)
	c.mu.Unlock()
	return c.inner.Send(ctx, b)
}

// planned runs the fixture in process over a fresh root, under the decisions of its build, and
// returns the bodies the runtime sends, in request order, and its result.
func planned(t *testing.T, c clsFixture) ([][]byte, replayResult) {
	t.Helper()
	root, env := materialize(t, c.f)
	records, err := c.f.RequestRecords()
	if err != nil {
		t.Fatal(err)
	}
	rec := &capture{inner: records}
	home := realDir(t)
	var stdout, stderr bytes.Buffer
	Main(t.Context(), Env{
		Stdin: bytes.NewReader(c.f.Request), Stdout: &stdout, Stderr: &stderr, Home: home,
		Dir: root, Getenv: func(string) string { return "" }, Environ: env,
		Decisions: c.decisions(), Transport: rec,
		Runner: proc.Exec{WaitDelay: jev.PipeWait},
	})
	var resp jev.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("in-process response %q: %v", stdout.String(), err)
	}
	got := replayResult{
		status: resp.Status, context: resp.Context, findings: resp.Findings,
		stdout: stdout.String(), stderr: stderr.String(),
	}
	if data, err := os.ReadFile(reportPath(home)); err == nil {
		got.report = data
	}
	return rec.bodies, got
}

// TestFixtureBuild: a cal fixture takes the committed-table build, in the binary and in process,
// even when it carries a Synthetic note as fixed fixtures do.
func TestFixtureBuild(t *testing.T) {
	c := clsFixture{
		path: filepath.Join("cal", "x.json"),
		f:    jev.Fixture{Synthetic: "written by hand"},
	}
	if env := c.env(); !slices.Contains(env, binPath(built.committed)) {
		t.Fatalf("env %v does not select the committed build", env)
	}
	got, _ := jev.Marshal(c.decisions())
	want, _ := jev.Marshal(Decisions())
	if string(got) != string(want) {
		t.Fatalf("decisions %s, want the committed table %s", got, want)
	}
}

// TestFixtures is C12 for classify: every fixed fixture and every recorded cal fixture, through
// the built binaries, gives one self-answered result with its expect and want, once with the key
// unset and once with a dummy key and an endpoint that fails the test on any request; a fixture
// with its records deleted, or its dup_ record deleted alone, fails its want.
func TestFixtures(t *testing.T) {
	needBuilt(t)
	for _, c := range loadFixtures(t) {
		t.Run(c.name(), func(t *testing.T) {
			if c.f.Pending() {
				t.Skipf("awaiting a jev-calibrate run with the key: %s", c.f.Placeholder)
			}
			checkFixture(t, c)
		})
	}
}

// checkFixture replays one fixture through the built binaries, once with the key unset and once
// with a dummy key and an endpoint that fails on any request, and once in process under the same
// decisions, and checks each against its want, then the sent triples and the deletions.
func checkFixture(t *testing.T, c clsFixture) {
	t.Helper()
	before := spyHits()
	var req struct {
		Jev json.RawMessage `json:"jev"`
	}
	if err := json.Unmarshal(c.f.Request, &req); err != nil || req.Jev == nil {
		t.Fatalf("the fixture request has no jev object (%v)", err)
	}
	if c.fixed() && c.f.Synthetic == "" {
		t.Fatal("a fixed fixture says why its records are written by hand")
	}
	if _, ok := testBuilds[c.f.Build]; c.f.Build != "" && !ok {
		t.Fatalf("unknown build %q", c.f.Build)
	}
	root, _ := materialize(t, c.f)
	for _, env := range []struct {
		name  string
		extra []string
	}{
		{"key unset", c.env()},
		{"dummy key and a failing endpoint", c.env(jev.KeyEnv + "=dummy")},
	} {
		got := replay(t, c.path, root, env.extra)
		if got.code != 0 {
			t.Fatalf("%s: exit %d: %s %s", env.name, got.code, got.stdout, got.stderr)
		}
		if msg := matchWant(c, got); msg != "" {
			t.Fatalf("%s: %s", env.name, msg)
		}
		for _, f := range got.findings {
			if (f.Kind == jev.KindUnrecorded || f.Kind == jev.KindError) &&
				!slices.ContainsFunc(c.f.Want.Findings, func(w jev.Finding) bool {
					return w.Kind == f.Kind
				}) {
				t.Fatalf("%s: unwanted %s", env.name, f.Kind)
			}
		}
	}
	if n := spyHits() - before; n != 0 {
		t.Fatalf("replay sent %d requests", n)
	}
	bodies, inProcess := planned(t, c)
	if msg := matchWant(c, inProcess); msg != "" {
		t.Fatalf("in process: %s", msg)
	}
	checkSent(t, c, bodies)
	checkDeletions(t, c, bodies)
}

// checkSent compares the owner and assistant texts each reply is sent with against want.sent.
func checkSent(t *testing.T, c clsFixture, bodies [][]byte) {
	t.Helper()
	if len(c.f.Want.Sent) == 0 {
		return
	}
	got := map[jev.Sent]bool{}
	for _, b := range bodies {
		s := decodeBody(b)
		for _, it := range s.State.Items {
			got[jev.Sent{Reply: it.Reply, Owner: it.Owner, Assistant: it.Assistant}] = true
		}
		if s.State.Reply != "" {
			got[jev.Sent{Reply: s.State.Reply, Owner: s.State.Owner, Assistant: s.State.Assistant}] = true
		}
	}
	for _, w := range c.f.Want.Sent {
		if !got[w] {
			t.Fatalf("no body carries %+v (sent %d distinct triples)", w, len(got))
		}
	}
}

// checkDeletions: with the records replay consumes deleted, a fixture fails its want, and with one
// dup_ record deleted alone a fixture with dup_ requests fails it too. Records its build never
// asks for (a cal fixture's union under a table with a decision disabled) are not deleted.
func checkDeletions(t *testing.T, c clsFixture, bodies [][]byte) {
	t.Helper()
	records, err := c.f.RequestRecords()
	if err != nil {
		t.Fatal(err)
	}
	consumed := map[string]bool{}
	for _, b := range bodies {
		if _, ok := records.Records[jev.Hash(b)]; ok {
			consumed[jev.Hash(b)] = true
		}
	}
	if len(consumed) == 0 {
		return
	}
	run := func(keep map[string]jev.Record) (replayResult, bool) {
		cut := c.f
		if err := cut.SetRecords(keep); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(realDir(t), "cut.json")
		if err := jev.WriteFixture(p, cut); err != nil {
			t.Fatal(err)
		}
		root, _ := materialize(t, cut)
		got := replay(t, p, root, c.env())
		return got, matchWant(c, got) == ""
	}
	unconsumed := map[string]jev.Record{}
	for h, r := range records.Records {
		if !consumed[h] {
			unconsumed[h] = r
		}
	}
	got, same := run(unconsumed)
	if same {
		t.Fatal("the fixture still meets its want with its consumed records deleted")
	}
	if !slices.ContainsFunc(
		got.findings,
		func(f jev.Finding) bool { return f.Kind == jev.KindUnrecorded },
	) {
		t.Fatalf("no jev-unrecorded: %+v", got.findings)
	}
	for _, b := range bodies {
		if !strings.Contains(string(b), `"memory_index"`) {
			continue
		}
		keep := map[string]jev.Record{}
		for h, r := range records.Records {
			if h != jev.Hash(b) {
				keep[h] = r
			}
		}
		if _, same := run(keep); same {
			t.Fatalf("the fixture still meets its want with a dup_ record deleted alone: %s", b)
		}
		return
	}
}
