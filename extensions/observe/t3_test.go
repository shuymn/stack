package observe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

type t3Row struct {
	thread, role string
	ago          time.Duration
	payload      map[string]any
}

// legacyT3Rows are the rows of the legacy T3 test (dotfiles tests/test_kaizen_cycle.py:196-216):
// owner 2, unknown 2 (server source, child thread), synthetic 1 (/compact), one owner-reply, and a
// row at until that is outside the window.
var legacyT3Rows = []t3Row{
	{
		"root",
		"user",
		50 * time.Minute,
		map[string]any{"createdBy": "user", "creationSource": "web", "text": "task", "id": "1"},
	},
	{"root", "assistant", 40 * time.Minute, map[string]any{"text": "done"}},
	{
		"root",
		"user",
		30 * time.Minute,
		map[string]any{"createdBy": "user", "creationSource": "web", "text": "not that", "id": "2"},
	},
	{
		"root",
		"user",
		20 * time.Minute,
		map[string]any{"createdBy": "user", "creationSource": "server", "text": "q?", "id": "3"},
	},
	{
		"root",
		"user",
		15 * time.Minute,
		map[string]any{"createdBy": "user", "creationSource": "web", "text": "/compact", "id": "4"},
	},
	{
		"child",
		"user",
		10 * time.Minute,
		map[string]any{"createdBy": "user", "creationSource": "web", "text": "hi", "id": "5"},
	},
	{
		"root",
		"user",
		0,
		map[string]any{"createdBy": "user", "creationSource": "web", "text": "at until", "id": "6"},
	},
}

var legacyT3Threads = map[string]string{
	"root":  "{}",
	"child": `{"lineage":{"parentThreadId":"root","relationshipToParent":"child"}}`,
}

func TestT3(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	db := f.write(".t3/userdata/statev2.sqlite", "")
	since := now.Add(-window)
	threads := `[{"thread_id":"root","payload_json":"{}"},` +
		`{"thread_id":"child","payload_json":"{\"lineage\":{\"parentThreadId\":\"root\",\"relationshipToParent\":\"child\"}}"}]`
	// What sqlite3 answers for the messages query: the in-window user rows with the counts.
	var rows []map[string]any
	for _, r := range legacyT3Rows {
		if r.role != "user" || r.ago == 0 {
			continue
		}
		created := now.Add(-r.ago)
		before, assistantRows := 0, 0
		for _, o := range legacyT3Rows {
			if o.thread == r.thread && now.Add(-o.ago).Before(created) {
				before++
				if o.role == "assistant" {
					assistantRows++
				}
			}
		}
		payload := mustJSON(t, r.payload)
		rows = append(rows, map[string]any{
			"thread_id": r.thread, "created_at": t3Time(created), "payload_json": string(payload),
			"before": before, "assistant": assistantRows,
		})
	}
	msgs := mustJSON(t, rows)
	sqlite := f.fake(
		"sqlite3",
		proctest.Response{Args: []string{"-readonly", "-json", db, t3ThreadsSQL}, Stdout: threads},
		proctest.Response{
			Args:   []string{"-readonly", "-json", db, t3MessagesSQL(since, now)},
			Stdout: string(msgs),
		},
	)

	_, l := f.run()
	if st := l.Sources["t3"]; st.Status != srcOK || st.Records != 5 {
		t.Fatalf("t3 = %+v", st)
	}
	if l.Tally != (Tally{Owner: 2, Synthetic: 1, Unknown: 2}) {
		t.Errorf("tally = %+v", l.Tally)
	}
	items := signalItems(l, "owner-reply")
	if len(items) != 1 || items[0].Excerpt != "not that" || items[0].Where != "t3:root:2" {
		t.Errorf("owner-reply = %+v", items)
	}
	calls := proctest.Calls(t, sqlite)
	if len(calls) != 2 {
		t.Fatalf("sqlite3 calls = %+v", calls)
	}
	query := calls[1].Args[3]
	for _, want := range []string{
		"-readonly", "-json", "'2026-11-14T00:00:00.000Z'", "'2026-11-21T00:00:00.000Z'", "m.role = 'user'",
		"created_at >= ", "created_at < ", "sum(b.role = 'assistant')",
	} {
		if !slices.Contains(calls[1].Args, want) && !strings.Contains(query, want) {
			t.Errorf("sqlite3 argv lacks %q: %q", want, calls[1].Args)
		}
	}
}

// TestT3RealSQLite runs the legacy fixture database through the real sqlite3 with observe's SQL.
func TestT3RealSQLite(t *testing.T) {
	t.Parallel()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not installed")
	}
	f := newFixture(t)
	f.tools["sqlite3"] = sqlite
	db := filepath.Join(f.home, ".t3", "userdata", "statev2.sqlite")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	sql.WriteString(
		"create table orchestration_v2_projection_threads (thread_id text, payload_json text);\n" +
			"create table orchestration_v2_projection_messages (thread_id text, role text, created_at text, payload_json text);\n",
	)
	for _, id := range []string{"root", "child"} {
		fmt.Fprintf(
			&sql,
			"insert into orchestration_v2_projection_threads values ('%s', '%s');\n",
			id,
			legacyT3Threads[id],
		)
	}
	for _, r := range legacyT3Rows {
		payload := mustJSON(t, r.payload)
		fmt.Fprintf(
			&sql,
			"insert into orchestration_v2_projection_messages values ('%s', '%s', '%s', '%s');\n",
			r.thread,
			r.role,
			t3Time(now.Add(-r.ago)),
			payload,
		)
	}
	cmd := exec.CommandContext(context.Background(), sqlite, db)
	cmd.Stdin = strings.NewReader(sql.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, out)
	}
	before, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}

	_, l := f.run()
	if st := l.Sources["t3"]; st.Status != srcOK {
		t.Fatalf("t3 = %+v", st)
	}
	if l.Tally != (Tally{Owner: 2, Synthetic: 1, Unknown: 2}) {
		t.Errorf("tally = %+v, want the legacy owner 2, synthetic 1, unknown 2", l.Tally)
	}
	if got := excerpts(signalItems(l, "owner-reply")); !slices.Equal(got, []string{"not that"}) {
		t.Errorf("owner-reply = %q", got)
	}
	after, err := os.ReadFile(db)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("the database changed (err %v)", err)
	}
}

func TestT3Unavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		setup   func(f *fixture)
		status  string
		unknown int
	}{
		{"no .t3: off", func(*fixture) {}, srcOff, 1},
		{"sqlite3 missing", func(f *fixture) { f.write(".t3/userdata/statev2.sqlite", "") }, srcFail, 1},
		{"sqlite3 error at exit 0", func(f *fixture) {
			f.write(".t3/userdata/statev2.sqlite", "")
			f.fake("sqlite3", proctest.Response{Stderr: "Error: query failed\n"})
		}, srcFail, 1},
		{"database missing", func(f *fixture) {
			f.write(".t3/other", "")
			f.fake("sqlite3", proctest.Response{Stdout: "[]"})
		}, srcFail, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tc.setup(f)
			f.fromTestdata("t3-session.jsonl", ".claude/projects/-p/t3s.jsonl", f.cwd)
			resp, l := f.run()
			if st := l.Sources["t3"]; st.Status != tc.status {
				t.Fatalf("t3 = %+v, want %s", st, tc.status)
			}
			// The T3 session's owner turns are unread: counted unknown, never owner-reply.
			if l.Tally.Unknown != tc.unknown || l.Tally.Owner != 0 ||
				len(signalItems(l, "owner-reply")) != 0 {
				t.Errorf("tally = %+v, items %+v", l.Tally, l.Items)
			}
			if tc.status == srcFail && resp.Status != StatusWarn {
				t.Errorf("status = %q", resp.Status)
			}
		})
	}
}
