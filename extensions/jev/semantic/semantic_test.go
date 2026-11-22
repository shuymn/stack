package semantic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// noRunner fails the test on any child process.
type noRunner struct{ t *testing.T }

func (n noRunner) Run(context.Context, proc.Cmd) (proc.Result, error) {
	n.t.Error("a child process was started")
	return proc.Result{}, proc.ErrStart
}

func (n noRunner) Stream(context.Context, proc.Cmd, func(io.Reader) error) error {
	n.t.Error("a child process was started")
	return proc.ErrStart
}

// eofReader records whether it was read to EOF.
type eofReader struct {
	r   io.Reader
	eof bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.eof = true
	}
	return n, err //nolint:wrapcheck // a reader passes io.EOF through
}

// countingServer counts requests and answers p for every question.
func countingServer(
	t *testing.T,
	status int,
	p map[string]float64,
) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		body, _ := io.ReadAll(r.Body)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"max_tokens_exceeded"}`))
			return
		}
		_, _ = w.Write(answer(t, body, p))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestOptOut(t *testing.T) {
	srv, hits := countingServer(t, http.StatusOK, nil)
	home := t.TempDir()
	data, err := json.Marshal(map[string]any{
		"stage": "outbound", "repo": "", "range": "a..b",
		"prompt": "", "title": "t", "body": "b", "branch": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	stdin := &eofReader{r: strings.NewReader(string(data))}
	var stdout, stderr strings.Builder
	code := Main(t.Context(), Env{
		Stdin: stdin, Stdout: &stdout, Stderr: &stderr, Home: home,
		Getenv: func(string) string { return "" }, Runner: noRunner{t}, Endpoint: srv.URL,
	})
	if code != 0 || stdout.String() != `{"status":"pass","context":"","findings":[]}`+"\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout.String())
	}
	if !stdin.eof {
		t.Fatal("stdin was not read to EOF")
	}
	if hits.Load() != 0 {
		t.Fatal("a connection was opened")
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("files written under home: %v", entries)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestNothingToAsk(t *testing.T) {
	srv, hits := countingServer(t, http.StatusOK, map[string]float64{MetaDuty: 0.9})
	r := gittest.New(t)
	r.Commit("base")
	r.Tag(RefMergeBase)
	r.Commit("empty")
	r.Tag(RefHead)
	env := repoEnv(r)
	env.Endpoint, env.Home = srv.URL, t.TempDir()

	for _, c := range []struct {
		name   string
		req    any
		status string
		kind   string
	}{
		{"not JSON", "{", jev.StatusWarn, jev.KindError},
		{"not an object", `["stage"]`, jev.StatusWarn, jev.KindError},
		{"no stage", `{"range":"x"}`, jev.StatusWarn, jev.KindError},
		{"numeric stage", `{"stage":1}`, jev.StatusWarn, jev.KindError},
		{
			"another stage",
			map[string]any{"stage": "entry", "range": FixtureRange},
			jev.StatusPass,
			"",
		},
		{
			"empty range",
			map[string]any{"stage": "outbound", "repo": r.Dir, "range": ""},
			jev.StatusPass, "",
		},
		{"a range whose diff has no files", outboundReq(r, "t", "b"), jev.StatusPass, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := runMain(t, env, c.req)
			if res.code != 0 || res.resp.Status != c.status {
				t.Fatalf("exit %d, response %+v", res.code, res.resp)
			}
			if c.kind == "" {
				if res.resp.Context != "" || len(res.resp.Findings) != 0 {
					t.Fatalf("response %+v", res.resp)
				}
				return
			}
			if len(res.resp.Findings) != 1 || res.resp.Findings[0].Kind != c.kind ||
				res.resp.Findings[0].Detail != "invalid request" ||
				res.resp.Context != "jev-semantic: invalid request" {
				t.Fatalf("response %+v", res.resp)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests sent", hits.Load())
	}
}

// stateRepo has two files whose diff exercises the config variants of C6: a blank context line
// (suppressBlankEmpty), a block whose hunk slides (indentHeuristic, git's own t4061 case), two
// files (orderFile) and a subdirectory (relative).
func stateRepo(t *testing.T) *gittest.Repo {
	t.Helper()
	base := "package a\n\nfunc a() {\n\tx()\n}\n\nfunc c() {\n\tz()\n}\n"
	head := "package a\n\nfunc a() {\n\tx()\n}\n\nfunc b() {\n\ty()\n}\n\nfunc c() {\n\tz()\n}\n"
	slide, slid := "1\n2\na\n\nb\n3\n4\n", "1\n2\na\n\nb\na\n\nb\n3\n4\n"
	return buildRepo(t, RepoSpec{
		Base: map[string]Content{"a.go": {Text: base}, "sub/z.go": {Text: slide}},
		Commits: []CommitSpec{{Message: "feat: add b", Files: map[string]*Content{
			"a.go": {Text: head}, "sub/z.go": {Text: slid},
		}}},
	})
}

func hashes(t *testing.T, g Git, texts Texts) []string {
	t.Helper()
	st, err := Prepare(t.Context(), g, FixtureRange)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := jev.Pack(Input(texts, st))
	if err != nil {
		t.Fatal(err)
	}
	hs := make([]string, 0, len(plan.Batches))
	for _, b := range plan.Batches {
		hs = append(hs, b.Hash)
	}
	return hs
}

//nolint:gocyclo,maintidx // one subtest per claim of C6
func TestState(t *testing.T) {
	texts := Texts{Title: "feat: add b", Body: "Adds b.", Branch: "feat/b"}

	t.Run("identical bodies under config variants", func(t *testing.T) {
		r := stateRepo(t)
		g := Git{Runner: proc.Exec{}, Dir: r.Dir, Env: r.Env}
		want := hashes(t, g, texts)
		raw := r.Git("diff", FixtureRange)

		cfg := filepath.Join(r.Root, "variant.gitconfig")
		order := filepath.Join(r.Root, "order")
		if err := os.WriteFile(order, []byte("sub/z.go\na.go\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Each variant also sets a value git rejects, so plain git fails unless it ignores the
		// variant.
		if err := os.WriteFile(cfg, []byte("[diff]\n\tnoprefix = true\n\tcontext = 9\n"+
			"[core]\n\tabbrev = bogus\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		withEnv := func(kv ...string) []string {
			return append(slices.Clone(r.Env), kv...)
		}
		for _, v := range []struct {
			name string
			env  []string
		}{
			{"system", withEnv("GIT_CONFIG_SYSTEM="+cfg, "GIT_CONFIG_NOSYSTEM=0")},
			{"global", withEnv("GIT_CONFIG_GLOBAL=" + cfg)},
			{"GIT_CONFIG_PARAMETERS", withEnv(
				"GIT_CONFIG_PARAMETERS='diff.noprefix'='true' 'core.abbrev'='bogus'")},
			{"GIT_CONFIG_COUNT", withEnv("GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=diff.context",
				"GIT_CONFIG_VALUE_0=0", "GIT_CONFIG_KEY_1=diff.context",
				"GIT_CONFIG_VALUE_1=bogus")},
		} {
			t.Run(v.name, func(t *testing.T) {
				res, err := proc.Exec{}.Run(t.Context(), proc.Cmd{
					Name: "git", Args: []string{"diff", FixtureRange}, Dir: r.Dir, Env: v.env,
				})
				if err == nil && res.ExitCode == 0 {
					t.Fatal("plain git accepts the variant, so the test does not exercise it")
				}
				got := hashes(t, Git{Runner: proc.Exec{}, Dir: r.Dir, Env: v.env}, texts)
				if !slices.Equal(got, want) {
					t.Fatal("the body changed")
				}
			})
		}
		for _, v := range []struct{ key, value string }{
			{"diff.suppressBlankEmpty", "true"},
			{"diff.noprefix", "true"},
			{"diff.relative", "true"},
			{"diff.indentHeuristic", "false"},
			{"core.abbrev", "12"},
			{"diff.orderFile", filepath.ToSlash(order)},
			{"diff.mnemonicPrefix", "true"},
			{"diff.algorithm", "patience"},
			{"diff.context", "1"},
			{"color.ui", "always"},
			{"core.quotePath", "true"},
		} {
			t.Run(v.key, func(t *testing.T) {
				r.Config(v.key, v.value)
				defer r.Git("config", "--unset", v.key)
				dir := r.Dir
				if v.key == "diff.relative" {
					dir = filepath.Join(r.Dir, "sub")
				}
				switch v.key {
				// mnemonicPrefix applies only to index and work-tree comparisons.
				case "core.quotePath", "color.ui", "diff.algorithm", "diff.mnemonicPrefix":
				default:
					// The variant changes plain git's output, so the test exercises it.
					rawDir := r.Git("-C", dir, "diff", FixtureRange)
					if rawDir == raw {
						t.Fatalf("%s does not change plain git diff here", v.key)
					}
				}
				got := hashes(t, Git{Runner: proc.Exec{}, Dir: dir, Env: r.Env}, texts)
				if !slices.Equal(got, want) {
					t.Fatal("the body changed")
				}
			})
		}
	})

	t.Run("unsupported_claim only when the whole PR fits", func(t *testing.T) {
		r := stateRepo(t)
		g := Git{Runner: proc.Exec{}, Dir: r.Dir, Env: r.Env}
		st, err := Prepare(t.Context(), g, FixtureRange)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := jev.Pack(Input(texts, st))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Batches) != 1 || len(plan.Batches[0].Questions) != 2 {
			t.Fatalf("small PR: %d batches, %v", len(plan.Batches), plan.Batches[0].Questions)
		}
		big := buildRepo(t, RepoSpec{
			Base: map[string]Content{"a.txt": {Text: "a\n"}},
			Commits: []CommitSpec{{Message: "docs: add", Files: map[string]*Content{
				"b.txt": {
					Line:   "line {n} of a long generated document for the packer",
					Repeat: 1200,
				},
			}}},
		})
		st, err = Prepare(t.Context(), Git{Runner: proc.Exec{}, Dir: big.Dir, Env: big.Env},
			FixtureRange)
		if err != nil {
			t.Fatal(err)
		}
		plan, err = jev.Pack(Input(texts, st))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Batches) < 2 {
			t.Fatalf("large PR: %d batches", len(plan.Batches))
		}
		for _, b := range plan.Batches {
			if len(b.Questions) != 1 || b.Questions[0].Name != MetaDuty {
				t.Fatalf("a part asked %v", b.Questions)
			}
		}
		if !slices.Equal(plan.Unasked, []jev.Unasked{{
			Question: Unsupported,
			Reason:   jev.ReasonNotWhole,
		}}) {
			t.Fatalf("unasked %v", plan.Unasked)
		}
	})

	t.Run("a test added only on the base is not in the merge-base range", func(t *testing.T) {
		r := buildRepo(t, RepoSpec{
			Base: map[string]Content{"calc.go": {Text: "package calc\n"}},
			Commits: []CommitSpec{{Message: "feat: add", Files: map[string]*Content{
				"calc.go": {Text: "package calc\n\nfunc Add(a, b int) int { return a + b }\n"},
			}}},
			Upstream: []CommitSpec{{Message: "test: cover calc", Files: map[string]*Content{
				"calc_test.go": {Text: "package calc\n"},
			}}},
		})
		st, err := Prepare(t.Context(), Git{Runner: proc.Exec{}, Dir: r.Dir, Env: r.Env},
			FixtureRange)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Units) != 1 || st.Units[0].Name != "calc.go" {
			t.Fatalf("units %v", st.Units)
		}
		// The two-dot range from the diverged base would show the test deleted.
		wrong, err := Prepare(t.Context(), Git{Runner: proc.Exec{}, Dir: r.Dir, Env: r.Env},
			RefBase+".."+RefHead)
		if err != nil {
			t.Fatal(err)
		}
		if len(wrong.Units) != 2 {
			t.Fatalf("the diverged fixture does not diverge: %v", wrong.Units)
		}
	})

	t.Run("quoted and spaced file names", func(t *testing.T) {
		//nolint:gosmopolitan // a non-ASCII name git leaves unquoted under core.quotePath=false
		want := []string{"a b.go", "日本.go"}
		if runtime.GOOS != "windows" {
			// A name git C-quotes; Windows does not allow a tab in a file name.
			want = append(want, "t\tab.go")
		}
		files := map[string]*Content{}
		for _, n := range want {
			files[n] = &Content{Text: n + "\n"}
		}
		r := buildRepo(t, RepoSpec{
			Base:    map[string]Content{"x": {Text: "x\n"}},
			Commits: []CommitSpec{{Message: "add", Files: files}},
		})
		st, err := Prepare(t.Context(), Git{Runner: proc.Exec{}, Dir: r.Dir, Env: r.Env},
			FixtureRange)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(st.Units))
		for _, u := range st.Units {
			names = append(names, u.Name)
		}
		slices.Sort(names)
		slices.Sort(want)
		if !slices.Equal(names, want) {
			t.Fatalf("names %q, want %q", names, want)
		}
	})

	t.Run("a git failure warns before any request", func(t *testing.T) {
		srv, hits := countingServer(t, http.StatusOK, nil)
		r := stateRepo(t)
		env := repoEnv(r)
		env.Endpoint, env.Home = srv.URL, t.TempDir()
		req := outboundReq(r, "t", "b")
		req["range"] = "no-such-ref.." + RefHead
		res := runMain(t, env, req)
		if res.resp.Status != jev.StatusWarn || len(res.resp.Findings) != 1 ||
			res.resp.Findings[0].Kind != jev.KindError ||
			res.resp.Findings[0].Detail != "git log exited 128" {
			t.Fatalf("response %+v", res.resp)
		}
		if hits.Load() != 0 {
			t.Fatal("a request was sent")
		}
	})
}

func TestDecision(t *testing.T) {
	units := []jev.Unit{{Name: "a_test.go", Text: "-x\n"}}
	plan := jev.Plan{Whole: true, Batches: []jev.Batch{{
		Pieces: []jev.Piece{{Unit: 0}}, Questions: Questions,
	}}}
	run := func(p float64, ds map[string]jev.Decision) jev.Response {
		o := jev.Outcome{Class: jev.OK, Answers: map[string]jev.Answer{
			MetaDuty: {Type: jev.TypeNoul, Noul: p}, Unsupported: {Type: jev.TypeNoul, Noul: 0},
		}}
		return Respond(plan, units, []jev.Outcome{o}, ds, io.Discard)
	}
	cutoff := 0.71
	ds := map[string]jev.Decision{
		MetaDuty:    {Enabled: true, Cutoff: &cutoff},
		Unsupported: {Enabled: true, Cutoff: &cutoff},
	}
	for _, c := range []struct {
		p    float64
		want string
	}{{0.7099, jev.StatusPass}, {0.71, jev.StatusWarn}, {0.72, jev.StatusWarn}} {
		r := run(c.p, ds)
		if r.Status != c.want {
			t.Fatalf("p=%v: %+v", c.p, r)
		}
		if c.want == jev.StatusWarn {
			f := r.Findings[0]
			if f.Kind != KindMetaDuty || f.Where != "a_test.go" ||
				f.Detail != "p="+num(c.p)+" ≥ 0.71 (jev-1.14.0)" || r.Context == "" {
				t.Fatalf("p=%v: %+v", c.p, r)
			}
		}
	}
	disabled := map[string]jev.Decision{
		MetaDuty:    {Cutoff: &cutoff, Reason: "not calibrated"},
		Unsupported: {Cutoff: &cutoff, Reason: "not calibrated"},
	}
	r := run(0.99, disabled)
	if r.Status != jev.StatusPass || len(r.Findings) != 0 ||
		!strings.Contains(r.Context, "meta_duty_regression is disabled: not calibrated") {
		t.Fatalf("disabled: %+v", r)
	}
}

func TestFailures(t *testing.T) {
	r := stateRepo(t)
	for _, c := range []struct {
		name    string
		status  int
		rstatus string
		kind    string
		context string
		stderr  string
		ledger  string
	}{
		{
			"401 warns with the action", 401, jev.StatusWarn, jev.KindError,
			"jev-semantic: Jev answered HTTP 401: verify the Jev API key in TYPESAFE_API_KEY if " +
				"set and non-blank, otherwise in ~/.config/stack/jev/key",
			"", `"outcome":"error"`,
		},
		{
			"403 warns with the action", 403, jev.StatusWarn, jev.KindError,
			"jev-semantic: Jev answered HTTP 403: verify the Jev API key in TYPESAFE_API_KEY if " +
				"set and non-blank, otherwise in ~/.config/stack/jev/key",
			"", `"outcome":"error"`,
		},
		{
			"529 is unassessed", 529, jev.StatusPass, jev.KindUnassessed,
			"jev-semantic: not assessed: meta_duty_regression, unsupported_claim on 1 of 1 " +
				"requests: HTTP 529 (a.go, sub/z.go)",
			"jev: 1 requests skipped: HTTP 529\n", `"outcome":"transient"`,
		},
		{
			"too large is unassessed", 400, jev.StatusPass, jev.KindUnassessed,
			"jev-semantic: not assessed: meta_duty_regression, unsupported_claim on 1 of 1 " +
				"requests: HTTP 400 max_tokens_exceeded (a.go, sub/z.go)",
			"jev: 1 requests skipped: HTTP 400 max_tokens_exceeded\n",
			`"outcome":"max_tokens_exceeded"`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := countingServer(t, c.status, nil)
			env := repoEnv(r)
			env.Endpoint, env.Home = srv.URL, t.TempDir()
			res := runMain(t, env, outboundReq(r, "t", "b"))
			if res.resp.Status != c.rstatus || res.resp.Findings[0].Kind != c.kind ||
				res.resp.Context != c.context || res.stderr != c.stderr {
				t.Fatalf("response %+v, stderr %q", res.resp, res.stderr)
			}
			ledger, err := os.ReadFile(jev.LedgerPath(env.Home))
			if err != nil || strings.Count(string(ledger), "\n") != 1 ||
				!strings.Contains(string(ledger), c.ledger) {
				t.Fatalf("ledger %q (%v)", ledger, err)
			}
		})
	}
}

func TestBudget(t *testing.T) {
	defer func(b time.Duration) { budget = b }(budget)
	budget = 2 * time.Second

	t.Run(
		"slow git: no request, questions unassessed, answer within the budget",
		func(t *testing.T) {
			srv, hits := countingServer(t, http.StatusOK, nil)
			// The fake git sleeps past the deadline and leaves a grandchild holding its pipes, so a
			// pipe wait longer than §3.1's would miss the budget.
			git := proctest.Install(t, "git", proctest.Scenario{Responses: []proctest.Response{
				{Sleep: 30 * time.Second, Grandchild: 30 * time.Second},
			}})
			env := Env{
				Getenv: func(string) string { return "k" }, Git: git, Endpoint: srv.URL,
				Home: t.TempDir(),
			}
			start := time.Now()
			res := runMain(t, env, map[string]any{
				"stage": "outbound", "repo": "",
				"range": FixtureRange,
			})
			if elapsed := time.Since(start); elapsed > budget {
				t.Fatalf("answered after %v, over the %v budget", elapsed, budget)
			}
			if res.resp.Status != jev.StatusPass || len(res.resp.Findings) != 1 ||
				res.resp.Findings[0].Kind != jev.KindUnassessed ||
				!strings.HasSuffix(res.resp.Findings[0].Detail, "preparation deadline") {
				t.Fatalf("response %+v", res.resp)
			}
			if hits.Load() != 0 {
				t.Fatal("a request was sent")
			}
		},
	)

	t.Run("slow HTTP: answered batches kept, the rest unassessed", func(t *testing.T) {
		r := buildRepo(t, RepoSpec{
			Base: map[string]Content{"a_test.go": {Text: "package a\n\nassert(t, x)\n"}},
			Commits: []CommitSpec{{Message: "refactor: tidy", Files: map[string]*Content{
				"a_test.go": {Text: "package a\n"},
				"b.txt": {
					Line:   "generated line {n} of the first padding document",
					Repeat: 600,
				},
				"c.txt": {
					Line:   "generated line {n} of the second padding document",
					Repeat: 600,
				},
			}}},
		})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(body), `"file":"a_test.go"`) {
				select {
				case <-time.After(30 * time.Second):
				case <-req.Context().Done():
					return
				}
			}
			_, _ = w.Write(answer(t, body, map[string]float64{MetaDuty: 0.9}))
		}))
		defer srv.Close()
		env := repoEnv(r)
		env.Endpoint, env.Home = srv.URL, t.TempDir()
		start := time.Now()
		res := runMain(t, env, outboundReq(r, "refactor: tidy", ""))
		if elapsed := time.Since(start); elapsed > budget {
			t.Fatalf("answered after %v", elapsed)
		}
		kinds := map[string]int{}
		for _, f := range res.resp.Findings {
			kinds[f.Kind]++
			if f.Kind == jev.KindUnassessed && f.Where != "message" &&
				!strings.HasSuffix(f.Detail, jev.CauseDeadline) {
				t.Fatalf("finding %+v", f)
			}
		}
		if res.resp.Status != jev.StatusWarn || kinds[KindMetaDuty] != 1 ||
			kinds[jev.KindUnassessed] != 2 {
			t.Fatalf("response %+v", res.resp)
		}
	})
}

func TestReserve(t *testing.T) {
	if budget != 10*time.Second || reserve != 500*time.Millisecond {
		t.Fatalf("budget %v, reserve %v; the design sets 10 s and 500 ms", budget, reserve)
	}
}

// TestPartialAnswer: a valid answer beside a null one keeps its finding and adds a jev-error
// for the null one (a clarification of §3.1: answers are checked one by one).
func TestPartialAnswer(t *testing.T) {
	units := []jev.Unit{{Name: "a_test.go", Text: "-x\n"}}
	plan := jev.Plan{Whole: true, Batches: []jev.Batch{{
		Pieces: []jev.Piece{{Unit: 0}}, Questions: Questions,
	}}}
	o := jev.Outcome{
		Class:   jev.OK,
		Answers: map[string]jev.Answer{MetaDuty: {Type: jev.TypeNoul, Noul: 0.9}},
		Invalid: map[string]string{Unsupported: jev.CauseMissing},
	}
	r := Respond(plan, units, []jev.Outcome{o}, testDecisions(), io.Discard)
	kinds := map[string]int{}
	for _, f := range r.Findings {
		kinds[f.Kind]++
	}
	if r.Status != jev.StatusWarn || kinds[KindMetaDuty] != 1 || kinds[jev.KindError] != 1 {
		t.Fatalf("response %+v", r)
	}
}

func TestAllDisabled(t *testing.T) {
	srv, hits := countingServer(t, http.StatusOK, map[string]float64{MetaDuty: 0.9})
	r := stateRepo(t)
	env := repoEnv(r)
	env.Endpoint, env.Home = srv.URL, t.TempDir()
	env.Decisions = map[string]jev.Decision{
		MetaDuty: {Reason: "not calibrated"}, Unsupported: {Reason: "not calibrated"},
	}
	res := runMain(t, env, outboundReq(r, "t", "b"))
	if hits.Load() != 0 {
		t.Fatalf("%d requests sent with every decision disabled", hits.Load())
	}
	if res.resp.Status != jev.StatusPass ||
		!strings.Contains(res.resp.Context, "every decision is disabled") {
		t.Fatalf("response %+v", res.resp)
	}
}
