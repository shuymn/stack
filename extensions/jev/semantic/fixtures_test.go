package semantic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/internal/gittest"
)

const fixtureDir = "../../../fixtures/jev/semantic"

type semFixture struct {
	path  string
	f     jev.Fixture
	req   jev.Request
	spec  RepoSpec
	label Label
}

func loadFixtures(t *testing.T) []semFixture {
	t.Helper()
	var out []semFixture
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
			sf := semFixture{path: p, f: f}
			for _, d := range []struct {
				raw json.RawMessage
				v   any
			}{{f.Request, &sf.req}, {f.Repo, &sf.spec}, {f.Label, &sf.label}} {
				if err := json.Unmarshal(d.raw, d.v); err != nil {
					t.Fatalf("%s: %v", p, err)
				}
			}
			out = append(out, sf)
		}
	}
	if len(out) == 0 {
		t.Fatal("no fixtures")
	}
	return out
}

func (sf semFixture) name() string {
	return filepath.Base(filepath.Dir(sf.path)) + "/" + filepath.Base(sf.path)
}

// fixed reports a fixture matched by the §3.5 fixed rule rather than exactly.
func (sf semFixture) fixed() bool { return filepath.Base(filepath.Dir(sf.path)) == "fixed" }

// matchWant compares an extension result with the fixture's want: for a fixed fixture the
// status, each finding's kind and where in order, and a non-empty context with hits; for a cal
// fixture the exact findings and context. Neither may carry jev-unrecorded or jev-error unless
// wanted.
func matchWant(sf semFixture, status, context string, findings []jev.Finding) error {
	if status != sf.f.Expect.Status {
		return fmt.Errorf("status %s, want %s", status, sf.f.Expect.Status)
	}
	want := sf.f.Want
	if want == nil {
		return errors.New("fixture has no want")
	}
	if !sf.fixed() {
		if !slices.Equal(findings, want.Findings) || context != want.Context {
			return fmt.Errorf("findings %+v context %q, want %+v %q", findings, context,
				want.Findings, want.Context)
		}
		return nil
	}
	type kw struct{ kind, where string }
	var got, exp []kw
	for _, f := range findings {
		got = append(got, kw{f.Kind, f.Where})
	}
	hits := false
	for _, f := range want.Findings {
		exp = append(exp, kw{f.Kind, f.Where})
		hits = hits || f.Kind == KindMetaDuty || f.Kind == KindUnsupported
	}
	if !slices.Equal(got, exp) {
		return fmt.Errorf("findings %+v, want %+v", got, exp)
	}
	if hits && context == "" {
		return errors.New("hits with an empty context")
	}
	return nil
}

// replay runs `stack run outbound --ext jev-semantic --replay <fixture>` in the fixture's
// repository and returns the single result. A fixed fixture runs the test build with the
// injected test cutoffs, a cal fixture the build with the committed cutoffs (§3.5). Replay
// writes nothing under the home directory, not even a ledger line.
func replay(t *testing.T, sf semFixture, path string, r *gittest.Repo, extra []string) (
	runResult, string, string, []jev.Finding,
) {
	t.Helper()
	if !sf.fixed() {
		extra = append(slices.Clone(extra), committedPath())
	}
	home := t.TempDir()
	res := runBinary(t, r, home, r.Dir, extra, built.stack, "run", "outbound",
		"--ext", "jev-semantic", "--replay", path)
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("%s: replay wrote under home: %v (%v)", sf.name(), entries, err)
	}
	agg := parseAggregate(t, res.stdout)
	if len(agg.Results) != 1 || agg.Results[0].Name != "jev-semantic" ||
		agg.Results[0].Error != "" {
		t.Fatalf("%s: aggregate %s (stderr %q)", sf.name(), res.stdout, res.stderr)
	}
	var fs []jev.Finding
	if err := json.Unmarshal(agg.Results[0].Findings, &fs); err != nil {
		t.Fatal(err)
	}
	return res, agg.Results[0].Status, agg.Results[0].Context, fs
}

func TestFixtures(t *testing.T) {
	needBuilt(t)
	bodies := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, sf := range loadFixtures(t) {
		t.Run(sf.name(), func(t *testing.T) {
			if sf.f.Pending() {
				t.Skipf("awaiting a jev-calibrate run with the key: %s", sf.f.Placeholder)
			}
			if sf.req.Jev == nil {
				t.Fatal("the fixture request has no jev object")
			}
			r := buildRepo(t, sf.spec)
			path, err := filepath.Abs(sf.path)
			if err != nil {
				t.Fatal(err)
			}
			for _, env := range []struct {
				name  string
				extra []string
			}{
				{"key unset", nil},
				{"dummy key and a failing endpoint", []string{jev.KeyEnv + "=dummy"}},
			} {
				res, status, context, fs := replay(t, sf, path, r, env.extra)
				if res.code != 0 {
					t.Fatalf("%s: exit %d: %s %s", env.name, res.code, res.stdout, res.stderr)
				}
				if err := matchWant(sf, status, context, fs); err != nil {
					t.Fatalf("%s: %v", env.name, err)
				}
			}
			if n := len(bodies()); n != 0 {
				t.Fatalf("replay sent %d requests", n)
			}

			hits := slices.ContainsFunc(sf.f.Want.Findings, func(f jev.Finding) bool {
				return f.Kind == KindMetaDuty || f.Kind == KindUnsupported
			})
			if !hits {
				return
			}
			// Without its records a hit fixture must fail its want.
			stripped := sf.f
			if err := stripped.SetRecords(map[string]jev.Record{}); err != nil {
				t.Fatal(err)
			}
			strippedPath := filepath.Join(t.TempDir(), "stripped.json")
			if err := jev.WriteFixture(strippedPath, stripped); err != nil {
				t.Fatal(err)
			}
			_, status, context, fs := replay(t, sf, strippedPath, r, nil)
			if matchWant(sf, status, context, fs) == nil {
				t.Fatal("the fixture still meets its want with its records deleted")
			}
			if !slices.ContainsFunc(fs, func(f jev.Finding) bool {
				return f.Kind == jev.KindUnrecorded
			}) {
				t.Fatalf("no jev-unrecorded: %+v", fs)
			}
		})
	}
}

func TestMixedLive(t *testing.T) {
	needBuilt(t)
	var sf semFixture
	for _, f := range loadFixtures(t) {
		if filepath.Base(f.path) == "mixed-size.json" {
			sf = f
		}
	}
	records, err := sf.f.RequestRecords()
	if err != nil {
		t.Fatal(err)
	}
	bodies := serve(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec, ok := records.Records[jev.Hash(body)]
		switch {
		case !ok:
			t.Errorf("unplanned body %s", jev.Hash(body))
			w.WriteHeader(http.StatusTeapot)
		case rec.Status != 0:
			w.WriteHeader(rec.Status)
			_, _ = w.Write([]byte(`{"detail":"` + rec.Error + `"}`))
		default:
			_, _ = w.Write(rec.Answer)
		}
	})
	r := buildRepo(t, sf.spec)
	files := t.TempDir()
	for name, text := range map[string]string{"title": sf.req.Title, "body": sf.req.Body} {
		if err := os.WriteFile(filepath.Join(files, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	res := runBinary(t, r, home, r.Dir, []string{jev.KeyEnv + "=dummy"}, built.stack, "run",
		"outbound", "--ext", "jev-semantic", "--range", FixtureRange,
		"--title-file", filepath.Join(files, "title"), "--body-file", filepath.Join(files, "body"),
		"--branch", sf.req.Branch)
	agg := parseAggregate(t, res.stdout)
	if res.code != 0 || len(agg.Results) != 1 || agg.Results[0].Error != "" {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	var fs []jev.Finding
	if err := json.Unmarshal(agg.Results[0].Findings, &fs); err != nil {
		t.Fatal(err)
	}
	if err := matchWant(sf, agg.Results[0].Status, agg.Results[0].Context, fs); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(fs, func(f jev.Finding) bool { return f.Kind == jev.KindError }) {
		t.Fatalf("a jev-error for the rejected batch: %+v", fs)
	}
	sent := map[string]int{}
	for _, b := range bodies() {
		sent[jev.Hash(b)]++
	}
	if len(sent) != len(records.Records) {
		t.Fatalf("%d distinct bodies sent, %d planned", len(sent), len(records.Records))
	}
	for h, n := range sent {
		if n != 1 {
			t.Fatalf("body %s sent %d times", h, n)
		}
	}
	ledger, err := os.ReadFile(jev.LedgerPath(home))
	if err != nil || strings.Count(string(ledger), "\n") != len(records.Records) {
		t.Fatalf("ledger %q (%v)\nstderr %s", ledger, err, res.stderr)
	}
}

func TestExitCodes(t *testing.T) {
	needBuilt(t)
	bodies := serve(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		_, _ = w.Write(answer(t, body, map[string]float64{MetaDuty: 0.9}))
	})
	r := buildRepo(t, RepoSpec{
		Base: map[string]Content{"a_test.go": {Text: "package a\n\nfunc TestA() { check() }\n"}},
		Commits: []CommitSpec{{Message: "refactor: tidy", Files: map[string]*Content{
			"a_test.go": {Text: "package a\n\nfunc TestA() {}\n"},
		}}},
	})
	files := t.TempDir()
	clean := filepath.Join(files, "clean")
	leaky := filepath.Join(files, "leaky")
	local := "/" + strings.Join([]string{"Users", "someone", "src"}, "/") + "/"
	for p, text := range map[string]string{clean: "refactor: tidy", leaky: "see " + local} {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, title  string
		code         int
		status       string
		publicStatus string
	}{
		{"clean texts", clean, 0, "warn", "pass"},
		{"texts hit", leaky, 1, "block", "block"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(bodies())
			res := runBinary(t, r, t.TempDir(), r.Dir, []string{jev.KeyEnv + "=dummy"},
				built.stack, "run", "outbound", "--ext", "public", "--ext", "jev-semantic",
				"--range", FixtureRange, "--title-file", c.title, "--branch", "x")
			agg := parseAggregate(t, res.stdout)
			if res.code != c.code || agg.Status != c.status || len(agg.Results) != 2 {
				t.Fatalf("exit %d: %s %s", res.code, res.stdout, res.stderr)
			}
			byName := map[string]string{}
			for _, x := range agg.Results {
				byName[x.Name] = x.Status
				if x.Error != "" {
					t.Fatalf("%s: synthetic result %q", x.Name, x.Error)
				}
				if x.Name != "jev-semantic" {
					continue
				}
				// The extension's own advisory, not a synthetic warn from a failed start.
				var fs []jev.Finding
				if err := json.Unmarshal(x.Findings, &fs); err != nil || len(fs) != 1 ||
					fs[0].Kind != KindMetaDuty {
					t.Fatalf("semantic findings %s (%v)", x.Findings, err)
				}
			}
			if byName["public"] != c.publicStatus || byName["jev-semantic"] != "warn" {
				t.Fatalf("results %v", byName)
			}
			if n := len(bodies()) - before; n != 1 {
				t.Fatalf("the fake endpoint received %d requests, want 1", n)
			}
		})
	}
}

// TestManyBatches is C4's many-batch case: a PR packed into many requests, each a hit, gives a
// result core accepts as the extension's own.
func TestManyBatches(t *testing.T) {
	needBuilt(t)
	bodies := serve(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		_, _ = w.Write(answer(t, body, map[string]float64{MetaDuty: 0.9}))
	})
	files := map[string]*Content{}
	for i := range 10 {
		files[fmt.Sprintf("docs/page%02d.md", i)] = &Content{
			Line: "Generated page line {n} that pads the request to its budget.", Repeat: 600,
		}
	}
	r := buildRepo(t, RepoSpec{
		Base:    map[string]Content{"README.md": {Text: "# x\n"}},
		Commits: []CommitSpec{{Message: "docs: add pages", Files: files}},
	})
	res := runBinary(t, r, t.TempDir(), r.Dir, []string{jev.KeyEnv + "=dummy"}, built.stack,
		"run", "outbound", "--ext", "jev-semantic", "--range", FixtureRange)
	agg := parseAggregate(t, res.stdout)
	if res.code != 0 || len(agg.Results) != 1 || agg.Results[0].Error != "" ||
		agg.Results[0].Status != jev.StatusWarn {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	var fs []jev.Finding
	if err := json.Unmarshal(agg.Results[0].Findings, &fs); err != nil {
		t.Fatal(err)
	}
	n := len(bodies())
	if n < 8 || len(fs) < n {
		t.Fatalf("%d requests, %d findings", n, len(fs))
	}
}
