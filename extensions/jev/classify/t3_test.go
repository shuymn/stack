package classify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/internal/t3loc"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// TestT3 runs the queries through a fake sqlite3: its argv, a failure, a diagnostic at exit 0,
// unusable output and a timeout.
func TestT3(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.sqlite")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(7 * 24 * time.Hour)
	rows := `[{"thread_id":"t:1","message_id":"m1","created_at":"2026-01-02T00:00:00.000Z",` +
		`"payload_json":"{\"id\":\"x\"}"}]`
	use := func(t *testing.T, r proctest.Response) (SQLite, string) {
		t.Helper()
		path := proctest.Install(t, "sqlite3", proctest.Scenario{Responses: []proctest.Response{r}})
		return SQLite{Runner: proc.Exec{WaitDelay: time.Second}, Name: path, DB: db}, path
	}

	t.Run("argv", func(t *testing.T) {
		s, path := use(t, proctest.Response{Stdout: rows})
		got, err := s.Users(t.Context(), since, until)
		if err != nil || len(got) != 1 || got[0].ThreadID != "t:1" {
			t.Fatalf("rows %+v (%v)", got, err)
		}
		calls := proctest.Calls(t, path)
		if len(calls) != 1 || !slices.Equal(calls[0].Args[:3], []string{"-readonly", "-json", db}) {
			t.Fatalf("calls %+v", calls)
		}
		q := calls[0].Args[3]
		for _, want := range []string{
			"role = 'user'", "created_at >= '2026-01-01T00:00:00.000Z'",
			"created_at < '2026-01-08T00:00:00.000Z'",
		} {
			if !strings.Contains(q, want) {
				t.Fatalf("query %q lacks %q", q, want)
			}
		}
	})
	t.Run("a thread ID is quoted as a literal", func(t *testing.T) {
		s, path := use(t, proctest.Response{Stdout: "[]"})
		if _, err := s.Thread(t.Context(), "a'b"); err != nil {
			t.Fatal(err)
		}
		q := proctest.Calls(t, path)[0].Args[3]
		if !strings.Contains(q, "thread_id = 'a''b' order by created_at, message_id") {
			t.Fatalf("query %q", q)
		}
	})
	t.Run("no rows is empty output", func(t *testing.T) {
		s, _ := use(t, proctest.Response{})
		got, err := s.Users(t.Context(), since, until)
		if err != nil || len(got) != 0 {
			t.Fatalf("rows %+v (%v)", got, err)
		}
	})
	for _, c := range []struct {
		name string
		r    proctest.Response
	}{
		{"a non-zero exit", proctest.Response{Exit: 1, Stderr: "Error: boom"}},
		{"a diagnostic on stderr at exit 0", proctest.Response{Stdout: "[]", Stderr: "warn"}},
		{"output that is not a list", proctest.Response{Stdout: "boom"}},
		{"a list that is not rows", proctest.Response{Stdout: `[1,2]`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := use(t, c.r)
			if _, err := s.Users(t.Context(), since, until); !errors.Is(err, errSQLite) {
				t.Fatalf("err %v", err)
			}
		})
	}
	t.Run("a hung child is killed at the deadline", func(t *testing.T) {
		s, _ := use(t, proctest.Response{Stdout: "[]", Sleep: 30 * time.Second})
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		start := time.Now()
		_, err := s.Users(ctx, since, until)
		if !errors.Is(err, errSQLite) || time.Since(start) > 10*time.Second {
			t.Fatalf("err %v after %v", err, time.Since(start))
		}
	})
	t.Run("a child holding its pipe open", func(t *testing.T) {
		s, _ := use(t, proctest.Response{Stdout: "[]", Grandchild: 20 * time.Second})
		start := time.Now()
		_, err := s.Users(t.Context(), since, until)
		if time.Since(start) > 10*time.Second {
			t.Fatalf("waited %v for the pipe (%v)", time.Since(start), err)
		}
	})
	t.Run("no database", func(t *testing.T) {
		s, _ := use(t, proctest.Response{Stdout: "[]"})
		s.DB = filepath.Join(t.TempDir(), "none.sqlite")
		if _, err := s.Users(t.Context(), since, until); !errors.Is(err, errNoDatabase) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a missing executable", func(t *testing.T) {
		s := SQLite{Runner: proc.Exec{}, Name: filepath.Join(t.TempDir(), "nope"), DB: db}
		if _, err := s.Users(t.Context(), since, until); !errors.Is(err, errSQLite) {
			t.Fatalf("err %v", err)
		}
	})
}

const t3Schema = `
create table orchestration_v2_projection_messages (
  message_id text primary key, thread_id text not null, role text not null,
  created_at text not null, payload_json text not null);
create table orchestration_v2_projection_threads (
  thread_id text primary key, project_id text not null, payload_json text not null);
create table projection_projects (project_id text primary key, workspace_root text not null);
`

// sqlQ quotes a test value as an SQL literal.
func sqlQ(s string) string { return sqlQuote(s) }

// realDB builds a database with the real sqlite3 and returns its path and the sqlite3 path.
func realDB(t *testing.T, statements ...string) (string, string) {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		// Under CI the design requires sqlite3 on the ubuntu and macOS jobs only
		if os.Getenv("CI") == "true" && runtime.GOOS != "windows" {
			t.Fatalf("sqlite3 not found on a CI runner: %v", err)
		}
		t.Skip("sqlite3 not found")
	}
	if out, err := exec.CommandContext(t.Context(), bin, "-version").Output(); err == nil {
		t.Logf("sqlite3 --version: %s", strings.TrimSpace(string(out)))
	}
	db := filepath.Join(t.TempDir(), "statev2.sqlite")
	cmd := exec.CommandContext(t.Context(), bin, db)
	cmd.Env = gittest.CleanEnv(os.Environ())
	cmd.Stdin = strings.NewReader(t3Schema + strings.Join(statements, ";\n") + ";\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the database: %v: %s", err, out)
	}
	return db, bin
}

func msg(thread, id, role, created string, payload map[string]any) string {
	b, _ := jev.Marshal(payload)
	return fmt.Sprintf(
		"insert into orchestration_v2_projection_messages values (%s, %s, %s, %s, %s)",
		sqlQ(id),
		sqlQ(thread),
		sqlQ(role),
		sqlQ(created),
		sqlQ(string(b)),
	)
}

// TestT3RealSQLite runs the real SQL on a database built with the real sqlite3 (C9). The
// authoritative job is ubuntu-latest, whose image lists SQLite; with CI=true a missing sqlite3
// fails rather than skips.
func TestT3RealSQLite(t *testing.T) {
	user := func(id any, text string) map[string]any {
		return map[string]any{"createdBy": "user", "creationSource": "web", "text": text, "id": id}
	}
	at := func(s int) string {
		return time.Date(2026, 1, 5, 10, 0, s, 0, time.UTC).Format("2006-01-02T15:04:05.000") + "Z"
	}
	stmts := make([]string, 0, 20)
	// Thread with a colon; message IDs holding ':'; payload ids a string, a number and null.
	th := "th:ab:1"
	stmts = append(stmts,
		msg(th, "mm:1", "user", at(0), user("p:1", "task")),
		msg(th, "mm:2", "assistant", at(1), map[string]any{"text": "done"}),
		msg(th, "mm:3", "user", at(2), user(float64(7), "fix numeric")),
		msg(th, "mm:4", "assistant", at(3), map[string]any{"text": "again"}),
		msg(th, "mm:5", "user", at(4), user(nil, "fix null")),
		// After the window, the same thread and the same null payload id: not the reply.
		msg(th, "mm:6", "assistant", "2026-01-07T10:00:00.000Z", map[string]any{"text": "later"}),
		msg(th, "mm:7", "user", "2026-01-07T10:00:01.000Z", user(nil, "later null")),
		// Two rows forming one locator.
		msg("th:dup", "d1", "user", at(5), user("same", "fix one")),
		msg("th:dup", "d2", "user", at(6), user("same", "fix two")),
		// Tied created_at: ordered by message_id.
		msg("th:tie", "b", "assistant", at(8), map[string]any{"text": "second"}),
		msg("th:tie", "a", "assistant", at(8), map[string]any{"text": "first"}),
		msg("th:tie", "c", "user", at(9), user("tie", "fix tied")),
		// An intervening owner row and a server prompt.
		msg("th:srv", "s1", "user", at(10), user("s1", "task")),
		msg("th:srv", "s2", "assistant", at(11), map[string]any{"text": "said"}),
		msg("th:srv", "s3", "user", at(12), map[string]any{
			"createdBy": "user", "creationSource": "server", "text": "srv", "id": "s3",
		}),
		msg("th:srv", "s4", "user", at(13), user("s4", "fix after server")),
		// Cwd sources: a worktree path, a project root, and neither.
		"insert into projection_projects values ('p1', '/project/root')",
		"insert into orchestration_v2_projection_threads values ('th:ab:1', 'p1', "+
			sqlQ(`{"worktreePath":"/work/tree"}`)+")",
		"insert into orchestration_v2_projection_threads values ('th:dup', 'p1', '{}')",
		"insert into orchestration_v2_projection_threads values ('th:tie', 'none', '{}')",
	)
	db, bin := realDB(t, stmts...)
	s := SQLite{Runner: proc.Exec{}, Name: bin, DB: db, Env: gittest.CleanEnv(os.Environ())}
	since, until := time.Date(
		2026,
		1,
		5,
		0,
		0,
		0,
		0,
		time.UTC,
	), time.Date(
		2026,
		1,
		6,
		0,
		0,
		0,
		0,
		time.UTC,
	)

	users, err := s.Users(t.Context(), since, until)
	if err != nil {
		t.Fatal(err)
	}
	locs := map[string]int{}
	for _, r := range users {
		p, _ := r.payload()
		locs[t3loc.Locator(r.ThreadID, p["id"])]++
	}
	// The expected locators come from observe's formatter as oracle, over the decoded ids.
	for _, want := range []string{
		t3loc.Locator(th, "p:1"), t3loc.Locator(th, float64(7)), t3loc.Locator(th, nil),
		t3loc.Locator("th:tie", "tie"),
	} {
		if locs[want] != 1 {
			t.Fatalf("locator %q count %d in %v", want, locs[want], locs)
		}
	}
	if locs[t3loc.Locator("th:dup", "same")] != 2 {
		t.Fatalf("the two rows of one locator: %v", locs)
	}

	rows, err := s.Thread(t.Context(), "th:tie")
	if err != nil {
		t.Fatal(err)
	}
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		order = append(order, r.MessageID)
	}
	if !slices.Equal(order, []string{"a", "b", "c"}) {
		t.Fatalf("tied created_at order %v", order)
	}
	if rows, err := s.Thread(t.Context(), "th:gone"); err != nil || len(rows) != 0 {
		t.Fatalf("a deleted thread gave %v (%v)", rows, err)
	}
	cwds, err := s.Cwds(t.Context(), []string{"th:ab:1", "th:dup", "th:tie", "th:gone"})
	if err != nil {
		t.Fatal(err)
	}
	if cwds["th:ab:1"] != "/work/tree" || cwds["th:dup"] != "/project/root" ||
		cwds["th:tie"] != "" || cwds["th:gone"] != "" {
		t.Fatalf("cwds %v", cwds)
	}

	// End to end through the resolver: the real rows give the same outcomes as the fakes.
	items := []*Item{
		t3Item(t3loc.Locator(th, float64(7)), "fix numeric", at(2)),
		t3Item(t3loc.Locator(th, nil), "fix null", at(4)),
		t3Item(t3loc.Locator("th:dup", "same"), "fix one", at(5)),
		t3Item(t3loc.Locator("th:tie", "tie"), "fix tied", at(9)),
		t3Item(t3loc.Locator("th:srv", "s4"), "fix after server", at(13)),
		t3Item(t3loc.Locator("th:ab:1", "gone"), "x", at(2)),
	}
	res := &resolver{base: t.TempDir(), t3: s}
	if err := res.resolve(t.Context(), items, since, until); err != nil {
		t.Fatal(err)
	}
	type outcome struct{ reason, owner, assistant string }
	want := []outcome{
		{"", "task", "done"},
		{"", "fix numeric", "again"},
		{reason: ReasonAmbiguous},
		{"", "", "first\n\nsecond"},
		{reason: ReasonIntervening},
		{reason: ReasonSourceGone},
	}
	for i, it := range items {
		got := outcome{it.Reason, it.Owner, it.Assistant}
		if it.Resolved {
			got.reason = ""
		}
		if got != want[i] {
			t.Fatalf("item %d (%s): %+v, want %+v", i, it.Obs.Where, got, want[i])
		}
	}
	if items[0].Cwd != "/work/tree" && !strings.HasSuffix(items[0].Cwd, "work/tree") {
		t.Fatalf("worktree cwd %q", items[0].Cwd)
	}
	if !items[3].CwdUnknown {
		t.Fatal("a thread with no cwd source has a known cwd")
	}
}

func t3Item(where, text, created string) *Item {
	ts, _ := parseTime(created)
	return &Item{Obs: ObserveItem{
		ID: where, Signal: signalOwnerReply, Where: where, Excerpt: excerpt(text), TS: iso(ts),
	}, ts: ts}
}
