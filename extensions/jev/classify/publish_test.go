package classify

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/internal/gittest"
)

// lockHolderEnv makes the test binary hold a report lock until killed (TestPublish's "a lock left
// by a killed process").
const lockHolderEnv = "STACK_JEV_CLASSIFY_HOLD_LOCK"

// holdLock is a child of this test binary: take the lock, say so, and wait for the parent to
// kill it. TestMain calls it when lockHolderEnv names a lock file.
func holdLock(path string) {
	l := flock.New(path)
	if ok, err := l.TryLock(); err != nil || !ok {
		os.Exit(3)
	}
	fmt.Println("locked")
	time.Sleep(time.Hour)
	os.Exit(0)
}

// stamp is a report's window end and run start.
type stamp struct {
	until   string
	started time.Time
}

// setNow sets the clock of a replay request.
func setNow(req map[string]any, t time.Time) {
	if j, ok := req["jev"].(map[string]any); ok {
		j["now"] = t.Format(time.RFC3339)
	}
}

// reportDoc is a report with its window and run start, for the compare-and-replace rule.
func reportDoc(until string, started time.Time, tag string) []byte {
	b, _ := jev.Marshal(map[string]any{
		"window": Window{Since: "2026-01-01T00:00:00Z", Until: until},
		"run":    map[string]string{"started": started.Format(time.RFC3339Nano)},
		"tag":    tag,
	})
	return b
}

func tagOf(t *testing.T, path string) string {
	t.Helper()
	var d struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &d); err != nil {
		t.Fatal(err)
	}
	return d.Tag
}

// entries lists a directory's file names.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

func far() time.Time { return time.Now().Add(time.Minute) }

// TestPublish is C9's publication under the lock.
//
//nolint:gocyclo,maintidx // one subtest per claim
func TestPublish(t *testing.T) {
	u1 := "2026-01-08T00:00:00Z"
	u2 := "2026-01-09T00:00:00Z"
	t1 := mustTime(t, u1)
	t2 := mustTime(t, u2)
	s1 := time.Date(2026, 1, 8, 1, 0, 0, 0, time.UTC)
	s2 := s1.Add(time.Hour)
	pub := func(path string, tag string, until string, started time.Time) publishResult {
		return publish(
			t.Context(),
			path,
			reportDoc(until, started, tag),
			mustTime(t, until),
			started,
			far(),
		)
	}

	t.Run("an older report is replaced, a newer one stands", func(t *testing.T) {
		for _, c := range []struct {
			name        string
			oldUntil    string
			oldStarted  time.Time
			newUntil    string
			newStarted  time.Time
			wantOutcome int
			wantTag     string
		}{
			{"a later window replaces", u1, s2, u2, s1, published, "new"},
			{"an earlier window is dropped", u2, s1, u1, s2, dropped, "old"},
			{"the same window, a later run replaces", u1, s1, u1, s2, published, "new"},
			{"the same window and start replaces", u1, s1, u1, s1, published, "new"},
			{"the same window, an earlier run is dropped", u1, s2, u1, s1, dropped, "old"},
		} {
			t.Run(c.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "jc", "latest.json")
				if r := pub(path, "old", c.oldUntil, c.oldStarted); r.outcome != published {
					t.Fatalf("first: %+v", r)
				}
				r := pub(path, "new", c.newUntil, c.newStarted)
				if r.outcome != c.wantOutcome || tagOf(t, path) != c.wantTag {
					t.Fatalf("outcome %d tag %s, want %d %s", r.outcome, tagOf(t, path),
						c.wantOutcome, c.wantTag)
				}
				if got := entries(
					t,
					filepath.Dir(path),
				); !slices.Equal(
					got,
					[]string{"latest.json", "latest.lock"},
				) {
					t.Fatalf("directory %v: a temp file survived", got)
				}
			})
		}
	})

	t.Run("a malformed old report is replaced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "latest.json")
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := pub(path, "new", u1, s1); r.outcome != published {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("an existing report that cannot be read is not replaced", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs POSIX permissions and a non-root user")
		}
		path := filepath.Join(t.TempDir(), "latest.json")
		pub(path, "old", u2, s2)
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		r := pub(path, "new", u1, s1)
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if r.outcome != failed || r.cause != "cannot read the existing report" ||
			tagOf(t, path) != "old" {
			t.Fatalf("%+v tag %s", r, tagOf(t, path))
		}
	})

	t.Run("a failed rename keeps the old file whole", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "latest.json")
		pub(path, "old", u1, s1)
		old := renameFile
		renameFile = func(string, string) error { return errors.New("boom") }
		t.Cleanup(func() { renameFile = old })
		r := pub(path, "new", u2, s2)
		if r.outcome != failed || r.cause != "cannot replace the report" ||
			tagOf(t, path) != "old" {
			t.Fatalf("%+v tag %s", r, tagOf(t, path))
		}
		if got := entries(
			t,
			filepath.Dir(path),
		); !slices.Equal(
			got,
			[]string{"latest.json", "latest.lock"},
		) {
			t.Fatalf("directory %v", got)
		}
	})

	t.Run("two runs contending inside compare-and-rename", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "latest.json")
		pub(path, "base", u1, s1)
		inCompare := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		afterCompare = func() {
			once.Do(func() {
				close(inCompare)
				<-release
			})
		}
		t.Cleanup(func() { afterCompare = nil })
		var wg sync.WaitGroup
		results := make([]publishResult, 2)
		wg.Go(func() { results[0] = pub(path, "A", u2, s1) })
		<-inCompare
		// B has the same newer window with a later start; it must wait for A's lock, then compare.
		started := make(chan struct{})
		wg.Go(func() {
			close(started)
			results[1] = pub(path, "B", u2, s2)
		})
		<-started
		time.Sleep(200 * time.Millisecond)
		if tagOf(t, path) != "base" {
			t.Fatal("B wrote while A held the lock")
		}
		close(release)
		wg.Wait()
		if results[0].outcome != published || results[1].outcome != published ||
			tagOf(t, path) != "B" {
			t.Fatalf("results %+v tag %s", results, tagOf(t, path))
		}
	})

	t.Run("runs finishing in reverse order keep the newer", func(t *testing.T) {
		for _, c := range []struct {
			name                 string
			newer, older         stamp
			wantTag, wantDropped string
		}{
			{"different windows", stamp{u2, s1}, stamp{u1, s2}, "newer", "older"},
			{"the same window", stamp{u1, s2}, stamp{u1, s1}, "newer", "older"},
		} {
			t.Run(c.name, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "jc")
				path := filepath.Join(dir, "latest.json")
				pub(path, "newer", c.newer.until, c.newer.started)
				r := pub(path, "older", c.older.until, c.older.started)
				if r.outcome != dropped || tagOf(t, path) != c.wantTag {
					t.Fatalf("%+v tag %s", r, tagOf(t, path))
				}
				if got := entries(
					t,
					dir,
				); !slices.Equal(
					got,
					[]string{"latest.json", "latest.lock"},
				) {
					t.Fatalf("the dropped run left %v", got)
				}
			})
		}
	})
	_, _ = t1, t2

	t.Run("a lock held past the bound is report lock busy, with no temp file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "jc")
		path := filepath.Join(dir, "latest.json")
		pub(path, "old", u1, s1)
		held := flock.New(filepath.Join(dir, "latest.lock"))
		if ok, err := held.TryLock(); err != nil || !ok {
			t.Fatalf("lock: %v %v", ok, err)
		}
		defer func() { _ = held.Unlock() }()
		start := time.Now()
		r := publish(
			t.Context(),
			path,
			reportDoc(u2, s2, "new"),
			t2,
			s2,
			time.Now().Add(400*time.Millisecond),
		)
		if r.outcome != failed || r.cause != "report lock busy" || tagOf(t, path) != "old" ||
			time.Since(start) > 3*time.Second {
			t.Fatalf("%+v after %v", r, time.Since(start))
		}
		if got := entries(t, dir); !slices.Equal(got, []string{"latest.json", "latest.lock"}) {
			t.Fatalf("directory %v", got)
		}
	})

	t.Run("a lock released before the bound is taken", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "jc")
		path := filepath.Join(dir, "latest.json")
		pub(path, "old", u1, s1)
		held := flock.New(filepath.Join(dir, "latest.lock"))
		if ok, err := held.TryLock(); err != nil || !ok {
			t.Fatalf("lock: %v %v", ok, err)
		}
		time.AfterFunc(300*time.Millisecond, func() { _ = held.Unlock() })
		r := publish(
			t.Context(),
			path,
			reportDoc(u2, s2, "new"),
			t2,
			s2,
			time.Now().Add(5*time.Second),
		)
		if r.outcome != published || tagOf(t, path) != "new" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("a lock left by a killed process does not block", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "jc")
		path := filepath.Join(dir, "latest.json")
		pub(path, "old", u1, s1)
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), exe)
		cmd.Env = append(
			gittest.CleanEnv(os.Environ()),
			lockHolderEnv+"="+filepath.Join(dir, "latest.lock"),
		)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Wait() }()
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "locked" {
			_ = cmd.Process.Kill()
			t.Fatalf("holder said %q (%v)", line, err)
		}
		// While the holder lives the lock is busy.
		busy := publish(
			t.Context(),
			path,
			reportDoc(u2, s2, "new"),
			t2,
			s2,
			time.Now().Add(300*time.Millisecond),
		)
		if busy.outcome != failed {
			_ = cmd.Process.Kill()
			t.Fatalf("lock not held: %+v", busy)
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		r := publish(
			t.Context(),
			path,
			reportDoc(u2, s2, "new"),
			t2,
			s2,
			time.Now().Add(5*time.Second),
		)
		if r.outcome != published || tagOf(t, path) != "new" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("an empty week replaces an older report", func(t *testing.T) {
		w := newWorld(t)
		w.mkdir("work")
		w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
		w.write()
		first := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if first.report == nil || len(first.report.Items) != 1 {
			t.Fatalf("%+v", first.resp)
		}
		// Observe's next window is later and holds no owner reply.
		w.items = nil
		later := testNow.Add(time.Hour)
		req := w.request()
		setNow(req, later)
		w.write()
		w.writeFile(
			"observe.json",
			strings.Replace(readFile(t, filepath.Join(w.root, "observe.json")),
				iso(testNow.Add(-time.Minute)), iso(later.Add(-time.Minute)), 1),
		)
		second := w.run(t, Env{Transport: &fake{p: byPrefix}}, req)
		if second.report == nil || len(second.report.Items) != 0 || !second.report.Complete {
			t.Fatalf("%+v %+v", second.resp, second.report)
		}
	})

	// A dropped run's stale summary and candidates are suppressed; its actionable warnings stay.
	for _, c := range []struct {
		name   string
		status int
		// populated: the newer report holds the same reply, not an empty week.
		populated bool
	}{
		{"a dropped run lists no candidate absent from the report that stands", 0, false},
		{"a dropped run keeps a persistent failure's warning", http.StatusUnauthorized, false},
		{"a dropped run leaves a populated newer report standing", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.mkdir("work")
			w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
			items := w.items
			wantItems := 0
			if c.populated {
				wantItems = 1
			} else {
				// The newer window, already published, holds no owner reply.
				w.items = nil
			}
			w.write()
			newer := testNow.Add(time.Hour)
			req := w.request()
			setNow(req, newer)
			w.writeFile(
				"observe.json",
				strings.Replace(readFile(t, filepath.Join(w.root, "observe.json")),
					iso(testNow.Add(-time.Minute)), iso(newer.Add(-time.Minute)), 1),
			)
			r1 := w.run(t, Env{Transport: &fake{p: byPrefix}}, req)
			if r1.resp.Status != jev.StatusPass || r1.report == nil ||
				len(r1.report.Items) != wantItems {
				t.Fatalf("newer run: %+v", r1.resp)
			}
			w.items = items
			w.write()
			p := byPrefix
			p.status = func(jev.Batch) int { return c.status }
			r2 := w.run(t, Env{Transport: &fake{p: p}}, w.request())
			drop := name + ": a report for a newer window or a later run already stands; " +
				"this run's report was dropped"
			want, status := drop, jev.StatusPass
			if c.status != 0 {
				want = name + ": Jev returned " + "HTTP 401" + " for kind_ requests: " +
					action(jev.Outcome{Status: c.status}) + "\n" + drop
				status = jev.StatusWarn
			}
			if r2.resp.Status != status || r2.resp.Context != want ||
				len(r2.report.Items) != wantItems {
				t.Fatalf("status %s context %q, report items %d", r2.resp.Status, r2.resp.Context,
					len(r2.report.Items))
			}
			if r2.report.Window.Until != iso(newer.Add(-time.Minute)) {
				t.Fatalf("the older run replaced the newer report: %+v", r2.report.Window)
			}
		})
	}

	t.Run(
		"a failed rename answers warn latest-not-written and keeps the old report",
		func(t *testing.T) {
			w := newWorld(t)
			w.turn("p/s.jsonl", "work", "task", "done", "fix alpha")
			w.write()
			first := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
			old := renameFile
			renameFile = func(string, string) error { return errors.New("boom") }
			t.Cleanup(func() { renameFile = old })
			later := testNow.Add(time.Hour)
			req := w.request()
			setNow(req, later)
			obs := readFile(t, filepath.Join(w.root, "observe.json"))
			w.writeFile("observe.json", strings.Replace(obs, iso(testNow.Add(-time.Minute)),
				iso(later.Add(-time.Minute)), 1))
			r := w.run(t, Env{Transport: &fake{p: byPrefix}}, req)
			if r.resp.Status != jev.StatusWarn ||
				!slices.ContainsFunc(r.resp.Findings, func(f jev.Finding) bool {
					return f.Kind == kindNotWritten
				}) ||
				!strings.Contains(r.resp.Context, "report not written") {
				t.Fatalf("%+v", r.resp)
			}
			if r.report.Window != first.report.Window {
				t.Fatalf("the old report changed: %+v", r.report.Window)
			}
		},
	)
}
