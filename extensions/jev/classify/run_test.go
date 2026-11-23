package classify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// turn adds one finished turn and the owner's reply to the transcript at rel and returns the
// reply's observe item ID.
func (w *world) turn(rel, cwd, task, closing, reply string) string {
	lines := w.transcript(rel, cwd, owner(task), text(closing), owner(reply))
	return w.reply(rel, lines[2])
}

// serveFake serves policy p over HTTP, recording the bodies and the Authorization headers.
func serveFake(t *testing.T, p policy) (*httptest.Server, func() [][]byte, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		s := decodeBody(body)
		b := jev.Batch{Body: body}
		for q := range s.Questions {
			b.Questions = append(b.Questions, jev.Question{Name: q})
		}
		if p.status != nil {
			if st := p.status(b); st != 0 {
				w.WriteHeader(st)
				return
			}
		}
		_, _ = w.Write(answerFor(b, p))
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]byte {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(bodies)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(auths)
		}
}

// liveEnv is an Env running a live (no jev object) request against url over a live world.
func liveEnv(w *world, url string) Env {
	return Env{
		Endpoint: url, Now: func() time.Time { return testNow },
		Getenv: func(k string) string {
			switch k {
			case jev.KeyEnv:
				return "k"
			case claudeConfigEnv:
				return filepath.Join(w.root, "claude")
			}
			return ""
		},
	}
}

func cycleReq() map[string]any { return map[string]any{"stage": "cycle"} }

// TestOptOut is C1 for classify: with no key, a fresh observe file with resolvable items gives
// pass with no context, no report, no ledger line and no request.
func TestOptOut(t *testing.T) {
	w := newWorld(t)
	w.live = true
	w.turn("proj/s.jsonl", "work", "task", "done", "fix this")
	w.write()
	before := spyHits()
	res := runBinary(t, w.home, w.root, nil, built.stack, "run", "cycle", "--ext", "jev-classify")
	agg := parseAggregate(t, res.stdout)
	if res.code != 0 || len(agg.Results) != 1 || agg.Results[0].Error != "" ||
		agg.Results[0].Status != jev.StatusPass || agg.Results[0].Context != "" {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	if spyHits() != before {
		t.Fatal("a request reached the endpoint")
	}
	for _, p := range []string{reportPath(w.home), jev.LedgerPath(w.home)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s written: %v", p, err)
		}
	}
	// A blank variable opts out too, in process.
	srv, bodies, _ := serveFake(t, byPrefix)
	env := liveEnv(w, srv.URL)
	env.Getenv = func(string) string { return " \t" }
	if r := w.run(t, env, cycleReq()); r.resp.Status != jev.StatusPass || r.resp.Context != "" ||
		len(bodies()) != 0 || r.report != nil {
		t.Fatalf("blank key: %+v, %d requests", r.resp, len(bodies()))
	}
}

// TestKeyFile is the key lookup of a live request (§3.1) for classify.
func TestKeyFile(t *testing.T) {
	put := func(t *testing.T, home, content string) {
		t.Helper()
		p := jev.KeyPath(home)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T) *world {
		t.Helper()
		w := newWorld(t)
		w.live = true
		w.turn("proj/s.jsonl", "work", "task", "done", "fix this")
		w.write()
		return w
	}
	t.Run("file key when the variable is unset", func(t *testing.T) {
		w := setup(t)
		srv, bodies, auths := serveFake(t, byPrefix)
		put(t, w.home, "file-key\n")
		env := liveEnv(w, srv.URL)
		env.Getenv = func(string) string { return "" }
		r := w.run(t, env, cycleReq())
		if len(bodies()) == 0 || auths()[0] != "Bearer file-key" ||
			r.resp.Status != jev.StatusPass {
			t.Fatalf("auth %q, response %+v", auths(), r.resp)
		}
	})
	t.Run("the variable first, the file never read", func(t *testing.T) {
		w := setup(t)
		srv, _, auths := serveFake(t, byPrefix)
		if err := os.MkdirAll(jev.KeyPath(w.home), 0o700); err != nil {
			t.Fatal(err)
		}
		w.run(t, liveEnv(w, srv.URL), cycleReq())
		if got := auths(); len(got) == 0 || got[0] != "Bearer k" {
			t.Fatalf("auth %q", got)
		}
	})
	t.Run("an unusable file warns with no request and no report", func(t *testing.T) {
		w := setup(t)
		srv, bodies, _ := serveFake(t, byPrefix)
		put(t, w.home, " \n")
		env := liveEnv(w, srv.URL)
		env.Getenv = func(string) string { return "" }
		r := w.run(t, env, cycleReq())
		want := name + ": key file " + jev.KeyDisplay + " is blank: " +
			"write the key into it, or remove it to opt out"
		if r.resp.Status != jev.StatusWarn || len(r.resp.Findings) != 1 ||
			r.resp.Findings[0].Kind != jev.KindError || r.resp.Context != want ||
			len(bodies()) != 0 || r.report != nil {
			t.Fatalf("response %+v", r.resp)
		}
	})
	t.Run("a null jev object is an invalid replay", func(t *testing.T) {
		w := setup(t)
		srv, bodies, _ := serveFake(t, byPrefix)
		put(t, w.home, "file-key")
		req := cycleReq()
		req["jev"] = nil
		r := w.run(t, liveEnv(w, srv.URL), req)
		if r.resp.Status != jev.StatusWarn ||
			r.resp.Findings[0].Detail != "invalid replay records" ||
			len(bodies()) != 0 {
			t.Fatalf("response %+v", r.resp)
		}
	})
	t.Run("replay never looks the key up", func(t *testing.T) {
		w := newWorld(t)
		w.turn("p/s.jsonl", "work", "task", "done", "fix this")
		w.write()
		put(t, w.home, " \n")
		env := Env{Getenv: func(string) string { return "" }, Transport: nil}
		r := w.run(t, env, w.request())
		for _, f := range r.resp.Findings {
			if f.Kind == jev.KindError {
				t.Fatalf("replay looked the key up: %+v", r.resp)
			}
		}
		if len(r.resp.Findings) == 0 || r.resp.Findings[0].Kind != jev.KindUnrecorded {
			t.Fatalf("response %+v, want jev-unrecorded", r.resp)
		}
	})
}

// TestNothingToAsk is C5 for classify: invalid stdin warns, another stage passes, and a week with
// no owner reply passes with no request but writes its empty report.
func TestNothingToAsk(t *testing.T) {
	w := newWorld(t)
	w.write()
	f := &fake{p: byPrefix}
	if r := w.run(t, Env{Transport: f}, "not json"); r.resp.Status != jev.StatusWarn ||
		r.resp.Findings[0].Detail != "invalid request" {
		t.Fatalf("%+v", r.resp)
	}
	other := w.request()
	other["stage"] = "outbound"
	if r := w.run(t, Env{Transport: f}, other); r.resp.Status != jev.StatusPass ||
		r.resp.Context != "" || r.report != nil {
		t.Fatalf("%+v", r.resp)
	}
	r := w.run(t, Env{Transport: f}, w.request())
	if r.resp.Status != jev.StatusPass || len(f.sent()) != 0 || r.report == nil ||
		len(r.report.Items) != 0 || !r.report.Complete {
		t.Fatalf("empty week: %+v, report %+v", r.resp, r.report)
	}
}

// TestRun is C9's run outcomes: the observe file's freshness and validity, a week with new items,
// persistent failures beside successful requests, the ledger and the report.
//
//nolint:gocyclo,maintidx // one subtest per claim
func TestRun(t *testing.T) {
	writeObserve := func(w *world, mutate func(m map[string]any)) {
		w.write()
		var m map[string]any
		if err := json.Unmarshal(
			[]byte(readFile(t, filepath.Join(w.root, "observe.json"))),
			&m,
		); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		data, _ := jev.Marshal(m)
		w.writeFile("observe.json", string(data))
	}
	window := func(until time.Time) func(m map[string]any) {
		return func(m map[string]any) {
			m["window"] = Window{Since: iso(until.Add(-7 * 24 * time.Hour)), Until: iso(until)}
		}
	}
	for _, c := range []struct {
		name   string
		mutate func(m map[string]any)
		status string
		report bool
	}{
		{"stale", window(testNow.Add(-24*time.Hour - time.Minute)), jev.StatusPass, false},
		{"just fresh", window(testNow.Add(-24*time.Hour + time.Minute)), jev.StatusPass, true},
		{"six minutes ahead", window(testNow.Add(6 * time.Minute)), jev.StatusWarn, false},
		{"four minutes ahead", window(testNow.Add(4 * time.Minute)), jev.StatusPass, true},
		{"unknown version", func(m map[string]any) { m["version"] = 2 }, jev.StatusWarn, false},
		{"missing version", func(m map[string]any) { delete(m, "version") }, jev.StatusWarn, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.turn("p/s.jsonl", "work", "task", "done", "fix this")
			writeObserve(w, c.mutate)
			f := &fake{p: byPrefix}
			r := w.run(t, Env{Transport: f}, w.request())
			if r.resp.Status != c.status || (r.report != nil) != c.report {
				t.Fatalf("status %s report %v: %+v", r.resp.Status, r.report != nil, r.resp)
			}
			if !c.report && len(f.sent()) != 0 {
				t.Fatal("a request was sent")
			}
		})
	}
	t.Run("a missing file passes", func(t *testing.T) {
		w := newWorld(t)
		r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if r.resp.Status != jev.StatusPass || r.report != nil || r.resp.Context == "" {
			t.Fatalf("%+v", r.resp)
		}
	})
	t.Run("an unparsable file warns", func(t *testing.T) {
		w := newWorld(t)
		w.writeFile("observe.json", "{")
		r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if r.resp.Status != jev.StatusWarn || r.report != nil {
			t.Fatalf("%+v", r.resp)
		}
	})

	t.Run("a week with new items passes with no hit finding", func(t *testing.T) {
		w := newWorld(t)
		w.mkdir("work")
		w.turn("p/s.jsonl", "work", "task", "done", "fix alpha beta")
		w.turn("p/s.jsonl", "work", "task", "done", "back gamma delta")
		w.turn("p/s.jsonl", "work", "task", "done", "ok thanks")
		w.write()
		w.memory("work", "- keep to the scope\n")
		r := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if r.resp.Status != jev.StatusPass || len(r.resp.Findings) != 0 {
			t.Fatalf("%+v", r.resp)
		}
		c := r.report.Counts
		if c.Qualified != 2 || c.New != 2 || c.Classified != 3 || !r.report.Complete {
			t.Fatalf("counts %+v complete %v", c, r.report.Complete)
		}
	})

	t.Run("a persistent failure beside successful requests warns", func(t *testing.T) {
		w := newWorld(t)
		w.mkdir("work")
		// Items one per request, so one request can fail: 30 KiB replies fit one to a state.
		pad := strings.Repeat("p ", 15<<10)
		ids := make([]string, 0, 4)
		for i := range 4 {
			ids = append(ids, w.turn("p/s.jsonl", "work", "task", "done",
				fmt.Sprintf("fix w%d %s", i, pad)))
		}
		w.write()
		w.memory("work", "- w1 w3 are known\n")
		p := byPrefix
		p.status = func(b jev.Batch) int {
			s := decodeBody(b.Body)
			// The request of the oldest reply (kind_) and the dup_ of the second reply fail.
			for _, it := range s.State.Items {
				if strings.HasPrefix(it.Reply, "fix w0 ") {
					return http.StatusUnauthorized
				}
			}
			if strings.HasPrefix(s.State.Reply, "fix w1 ") {
				return http.StatusUnauthorized
			}
			return 0
		}
		f := &fake{p: p}
		r := w.run(t, Env{Transport: f}, w.request())
		if r.resp.Status != jev.StatusWarn {
			t.Fatalf("%+v", r.resp)
		}
		var errs int
		for _, fd := range r.resp.Findings {
			if fd.Kind == jev.KindError {
				errs++
			}
		}
		if errs != 2 {
			t.Fatalf("%d jev-error findings: %+v", errs, r.resp.Findings)
		}
		if it := r.item(
			t,
			ids[0],
		); it.Status != StatusUnevaluated || it.Reason != ReasonRequestFailed ||
			it.Done() {
			t.Fatalf("oldest: %+v", it)
		}
		if it := r.item(
			t,
			ids[1],
		); it.Duplicate != DupUnassessed || it.DupReason != ReasonRequestFailed ||
			it.Done() {
			t.Fatalf("second: %+v", it)
		}
		if it := r.item(t, ids[2]); it.Duplicate != DupNew || !it.Done() {
			t.Fatalf("third: %+v", it)
		}
		if it := r.item(t, ids[3]); it.Duplicate != DupDuplicate {
			t.Fatalf("fourth: %+v", it)
		}
		if r.report.Complete {
			t.Fatal("complete with failed items")
		}
		if !strings.Contains(r.resp.Context, ids2where(r, ids[2])) {
			t.Fatalf("the kept candidate is not listed: %s", r.resp.Context)
		}
	})

	t.Run("a malformed answer warns and leaves its item unevaluated", func(t *testing.T) {
		w := newWorld(t)
		id := w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
		other := w.turn("p/s.jsonl", "work", "task", "done", "ok fine")
		w.write()
		p := byPrefix
		p.drop = func(q string) bool { return q == "kind_r2" }
		r := w.run(t, Env{Transport: &fake{p: p}}, w.request())
		if r.resp.Status != jev.StatusWarn ||
			r.item(t, id).Status != StatusUnevaluated &&
				r.item(t, other).Status != StatusUnevaluated {
			t.Fatalf("%+v", r.resp)
		}
		var bad ReportItem
		for _, it := range r.report.Items {
			if it.Status == StatusUnevaluated {
				bad = it
			}
		}
		if bad.Reason != ReasonMalformed || bad.Done() || r.report.Complete {
			t.Fatalf("%+v", bad)
		}
	})

	t.Run("transient failures change no status", func(t *testing.T) {
		w := newWorld(t)
		id := w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
		w.write()
		p := byPrefix
		p.status = func(jev.Batch) int { return http.StatusServiceUnavailable }
		r := w.run(t, Env{Transport: &fake{p: p}}, w.request())
		it := r.item(t, id)
		if r.resp.Status != jev.StatusPass || it.Reason != ReasonTransient || r.report.Complete ||
			!strings.Contains(r.stderr, "jev: 1 requests skipped: HTTP 503") {
			t.Fatalf("%+v %+v stderr %q", r.resp, it, r.stderr)
		}
		if !slices.ContainsFunc(r.resp.Findings, func(f jev.Finding) bool {
			return f.Kind == jev.KindUnassessed
		}) {
			t.Fatalf("no jev-unassessed: %+v", r.resp.Findings)
		}
	})

	t.Run("live attempts add ledger lines and the report is private", func(t *testing.T) {
		w := newWorld(t)
		w.live = true
		w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
		w.write()
		srv, bodies, _ := serveFake(t, byPrefix)
		r := w.run(t, liveEnv(w, srv.URL), cycleReq())
		if r.resp.Status != jev.StatusPass || r.report == nil {
			t.Fatalf("%+v", r.resp)
		}
		ledger, err := os.ReadFile(jev.LedgerPath(w.home))
		if err != nil || strings.Count(string(ledger), "\n") != len(bodies()) {
			t.Fatalf("ledger %q for %d requests (%v)", ledger, len(bodies()), err)
		}
		if r.report.Usage.Requests != len(bodies()) ||
			r.report.Usage.InputTokens != int64(10*len(bodies())) {
			t.Fatalf("usage %+v", r.report.Usage)
		}
		if fi, err := os.Stat(
			reportPath(w.home),
		); err != nil ||
			fi.Mode().Perm()&0o077 != 0 && !isWindows() {
			t.Fatalf("report mode %v (%v)", fi, err)
		}
	})
}

func isWindows() bool { return filepath.Separator == '\\' }

func ids2where(r result, id string) string {
	for _, it := range r.report.Items {
		if it.ID == id {
			return it.Where
		}
	}
	return ""
}
