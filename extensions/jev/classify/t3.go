package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
)

// maxSQLiteOutput bounds one query's stdout; a thread's rows can reach tens of MiB.
const maxSQLiteOutput = 256 << 20

var (
	errSQLite = errors.New("sqlite3 failed")
	// errSQLiteTimeout marks a query its child deadline ended (wrapped with errSQLite).
	errSQLiteTimeout = errors.New("timed out")
)

// SQLite reads the T3 Code state database through `sqlite3 -readonly -json`, as observe opens it.
type SQLite struct {
	Runner proc.Runner
	// Name is the sqlite3 executable; "" means sqlite3 on PATH.
	Name string
	// DB is the database file.
	DB  string
	Env []string
}

// t3Database is the T3 state database under home, at the path observe reads.
func t3Database(home string) string {
	return filepath.Join(home, ".t3", "userdata", "statev2.sqlite")
}

// sqlQuote quotes s as an SQL string literal.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// usersSQL reads the user rows of observe's window, with observe's bounds.
func usersSQL(since, until time.Time) string {
	return "select thread_id, message_id, created_at, payload_json " +
		"from orchestration_v2_projection_messages where role = 'user' and created_at >= " +
		sqlQuote(t3Time(since)) + " and created_at < " + sqlQuote(t3Time(until))
}

// threadSQL reads one thread's rows in order.
func threadSQL(thread string) string {
	return "select message_id, role, created_at, payload_json " +
		"from orchestration_v2_projection_messages where thread_id = " + sqlQuote(thread) +
		" order by created_at, message_id"
}

// cwdSQL reads each thread's cwd: its worktreePath, else its project's workspace_root
// (**assumption**, the observed schema).
func cwdSQL(threads []string) string {
	quoted := make([]string, 0, len(threads))
	for _, th := range threads {
		quoted = append(quoted, sqlQuote(th))
	}
	return "select t.thread_id as thread_id, coalesce(nullif(case when json_valid(t.payload_json) " +
		"then json_extract(t.payload_json, '$.worktreePath') end, ''), p.workspace_root) as cwd " +
		"from orchestration_v2_projection_threads t left join projection_projects p " +
		"on p.project_id = t.project_id where t.thread_id in (" + strings.Join(
		quoted,
		", ",
	) + ")"
}

// Users implements T3.
func (s SQLite) Users(ctx context.Context, since, until time.Time) ([]T3Row, error) {
	var rows []T3Row
	return rows, s.query(ctx, usersSQL(since, until), &rows)
}

// Thread implements T3.
func (s SQLite) Thread(ctx context.Context, thread string) ([]T3Row, error) {
	var rows []T3Row
	return rows, s.query(ctx, threadSQL(thread), &rows)
}

// Cwds implements T3.
func (s SQLite) Cwds(ctx context.Context, threads []string) (map[string]string, error) {
	var rows []struct {
		ThreadID string  `json:"thread_id"` //nolint:tagliatelle // SQL column
		Cwd      *string `json:"cwd"`
	}
	if err := s.query(ctx, cwdSQL(threads), &rows); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		if r.Cwd != nil {
			out[r.ThreadID] = *r.Cwd
		}
	}
	return out, nil
}

// query runs one read-only query; empty output is no rows. A non-zero exit, a timeout, cut
// output or a diagnostic on stderr fails it.
func (s SQLite) query(ctx context.Context, query string, into any) error {
	info, err := os.Stat(s.DB)
	if errors.Is(err, fs.ErrNotExist) {
		return errNoDatabase
	}
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: database unreadable", errSQLite)
	}
	name := s.Name
	if name == "" {
		name = "sqlite3"
	}
	cctx, cancel := childContext(ctx)
	defer cancel()
	res, err := s.Runner.Run(cctx, proc.Cmd{
		Name: name, Args: []string{"-readonly", "-json", s.DB, query}, Env: s.Env,
		MaxStdout: maxSQLiteOutput,
	})
	switch {
	case res.TimedOut || err != nil && cctx.Err() != nil:
		return fmt.Errorf("%w: %w", errSQLite, errSQLiteTimeout)
	case err != nil:
		return fmt.Errorf("%w: %w", errSQLite, err)
	case res.ExitCode != 0 || res.WaitDelayExceeded || res.Truncated:
		return fmt.Errorf("%w: exit %d", errSQLite, res.ExitCode)
	case len(bytes.TrimSpace(res.Stderr)) > 0:
		return fmt.Errorf("%w: error on stderr", errSQLite)
	}
	out := bytes.TrimSpace(res.Stdout)
	if len(out) == 0 {
		return nil
	}
	if out[0] != '[' || json.Unmarshal(out, into) != nil {
		return fmt.Errorf("%w: unexpected output", errSQLite)
	}
	return nil
}

// childContext ends a child ChildMargin before the work deadline, so killing it and reaping its
// pipes fit inside the budget (§3.1).
func childContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if d, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, d.Add(-jev.ChildMargin))
	}
	return context.WithCancel(ctx)
}
