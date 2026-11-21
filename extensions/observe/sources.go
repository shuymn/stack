package observe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- T3

// t3Time formats a window bound as T3 stores created_at (legacy observe.py:213-214).
func t3Time(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

// t3 reads the T3 Code state database through `sqlite3 -readonly -json` with the legacy queries
// (observe.py:223-260); it is off when ~/.t3 does not exist at all.
func (s *state) t3(st *Source) error {
	if _, err := os.Lstat(filepath.Join(s.o.Home, ".t3")); errors.Is(err, fs.ErrNotExist) {
		return errOff
	}
	db := filepath.Join(s.o.Home, ".t3", "userdata", "statev2.sqlite")
	info, err := os.Lstat(db)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("t3 database missing") //nolint:err113 // a fixed per-cause summary
	}
	if err != nil {
		return errors.New("t3 database unreadable") //nolint:err113 // a fixed per-cause summary
	}
	if !info.Mode().IsRegular() {
		//nolint:err113 // a fixed per-cause summary
		return errors.New("t3 database is not a regular file")
	}
	st.Files = 1
	sqlite, err := s.o.LookPath("sqlite3")
	if err != nil {
		return errors.New("sqlite3 not found") //nolint:err113 // a fixed per-cause summary
	}
	var threads []struct {
		ThreadID string `json:"thread_id"`    //nolint:tagliatelle // SQL column
		Payload  string `json:"payload_json"` //nolint:tagliatelle // SQL column
	}
	if err := s.sqlite(sqlite, db, t3ThreadsSQL, &threads); err != nil {
		return err
	}
	roots := map[string]bool{}
	for _, th := range threads {
		var payload any
		if json.Unmarshal([]byte(th.Payload), &payload) != nil {
			return fmt.Errorf("sqlite3 %w: a thread payload is not JSON", errOutput)
		}
		if rootThread(payload) {
			roots[th.ThreadID] = true
		}
	}
	var rows []struct {
		ThreadID  string `json:"thread_id"`    //nolint:tagliatelle // SQL column
		CreatedAt string `json:"created_at"`   //nolint:tagliatelle // SQL column
		Payload   string `json:"payload_json"` //nolint:tagliatelle // SQL column
		Before    int    `json:"before"`
		Assistant int    `json:"assistant"`
	}
	if err := s.sqlite(sqlite, db, t3MessagesSQL(s.since, s.until), &rows); err != nil {
		return err
	}
	for _, row := range rows {
		st.Records++
		var d map[string]any
		if json.Unmarshal([]byte(row.Payload), &d) != nil || d == nil {
			continue
		}
		text, _ := d["text"].(string)
		idle, hasIdle := d["idleCompaction"]
		if d["createdBy"] != "user" || (hasIdle && idle != nil) ||
			strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "/compact") {
			s.latest.Tally.Synthetic++
			continue
		}
		if src, _ := d["creationSource"].(string); (src != "web" && src != "mobile") ||
			!roots[row.ThreadID] {
			s.latest.Tally.Unknown++
			continue
		}
		s.latest.Tally.Owner++
		if len([]rune(text)) <= shortReply && row.Before > 0 && row.Assistant > 0 {
			t, _ := parseTime(row.CreatedAt)
			s.add(
				"owner-reply",
				fmt.Sprintf("t3:%s:%v", row.ThreadID, jsonScalar(d["id"])),
				text,
				t,
				"owner",
				"",
			)
		}
	}
	return nil
}

const t3ThreadsSQL = "select thread_id, payload_json from orchestration_v2_projection_threads"

// t3MessagesSQL is the legacy user-row query with the legacy per-row count of earlier rows and
// earlier assistant rows folded in as correlated subqueries. The literals are formatted here.
func t3MessagesSQL(since, until time.Time) string {
	return "select m.thread_id, m.created_at, m.payload_json, " +
		"(select count(*) from orchestration_v2_projection_messages b " +
		"where b.thread_id = m.thread_id and b.created_at < m.created_at) as before, " +
		"(select coalesce(sum(b.role = 'assistant'), 0) from orchestration_v2_projection_messages b " +
		"where b.thread_id = m.thread_id and b.created_at < m.created_at) as assistant " +
		"from orchestration_v2_projection_messages m where m.role = 'user' and m.created_at >= '" +
		t3Time(since) + "' and m.created_at < '" + t3Time(until) + "' order by m.created_at"
}

func jsonScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// rootThread: no lineage, or a lineage without a parent (observe.py:217-220).
func rootThread(payload any) bool {
	p, ok := payload.(map[string]any)
	if !ok {
		return true
	}
	lineage, ok := p["lineage"]
	if !ok || lineage == nil {
		return true
	}
	l, ok := lineage.(map[string]any)
	return ok && l["relationshipToParent"] == nil && l["parentThreadId"] == nil
}

// errOutput marks a child that exited 0 with output observe cannot use.
var errOutput = errors.New("unexpected output")

// sqlite runs one read-only query; empty output is no rows (real sqlite3 prints nothing for zero
// rows and exits non-zero on an error). A diagnostic on stderr fails the query even at exit 0.
func (s *state) sqlite(bin, db, query string, into any) error {
	out := s.child(command{
		name: bin, args: []string{"-readonly", "-json", db, query}, timeout: s.o.ghTimeout,
	})
	if out.timedOut && s.work.Err() != nil {
		return fmt.Errorf("sqlite3: %w", s.work.Err())
	}
	if !out.ok() {
		return fmt.Errorf("sqlite3 failed: %s", out.describe()) //nolint:err113 // fixed summary
	}
	if len(bytes.TrimSpace(out.stderr)) > 0 {
		return errors.New("sqlite3 failed: error on stderr") //nolint:err113 // fixed summary
	}
	if len(bytes.TrimSpace(out.stdout)) == 0 {
		return nil
	}
	if !opens(out.stdout, '[') || json.Unmarshal(out.stdout, into) != nil {
		return fmt.Errorf("sqlite3 %w", errOutput)
	}
	return nil
}

// ---------------------------------------------------------------- Codex

const maxFirstLine = 16 << 20

// codex adds the cwd of each Codex session started in the window (observe.py:266-297).
func (s *state) codex(st *Source) error {
	root, err := walkRoot(filepath.Join(s.o.Home, ".codex", "sessions"))
	if err != nil {
		return fmt.Errorf("codex sessions %w", err)
	}
	// Links below the root are not followed.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if werr := s.work.Err(); werr != nil {
			return fmt.Errorf("codex: %w", werr)
		}
		if err != nil {
			if path == root {
				return errors.New("codex sessions root unreadable") //nolint:err113 // summary
			}
			st.unreadable = append(st.unreadable, unreadable(root, path, err))
			return nil
		}
		name := d.Name()
		if !d.IsDir() && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
			s.codexSession(root, path, st)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk codex sessions: %w", err)
	}
	return nil
}

// codexSession reads the first line of one rollout file.
func (s *state) codexSession(root, path string, st *Source) {
	info, err := os.Lstat(path)
	if err != nil {
		st.unreadable = append(st.unreadable, unreadable(root, path, err))
		return
	}
	if !info.Mode().IsRegular() {
		st.unreadable = append(st.unreadable, unreadable(root, path, errNotRegular))
		return
	}
	if info.ModTime().Before(s.since) {
		return
	}
	first, err := firstLine(path)
	if err != nil {
		st.unreadable = append(st.unreadable, unreadable(root, path, err))
		return
	}
	st.Files++
	var rec struct {
		Timestamp string `json:"timestamp"`
		Payload   *struct {
			Timestamp string `json:"timestamp"`
			CWD       string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(first, &rec) != nil || rec.Payload == nil {
		return
	}
	st.Records++
	ts := rec.Payload.Timestamp
	if ts == "" {
		ts = rec.Timestamp
	}
	if t, ok := parseTime(ts); ok && s.inWindow(t) {
		cwd := rec.Payload.CWD
		if cwd == "" {
			cwd = "?"
		}
		s.cwds[cwd]++
	}
}

func firstLine(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	return readFirstLine(f)
}

// readFirstLine reads up to the first newline, at most about maxFirstLine bytes; a read error
// other than EOF fails the file.
func readFirstLine(f io.Reader) ([]byte, error) {
	r := bufio.NewReader(f)
	var line []byte
	for len(line) < maxFirstLine {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		switch {
		case err == nil, errors.Is(err, io.EOF):
			return line, nil
		case !errors.Is(err, bufio.ErrBufferFull):
			return nil, fmt.Errorf("read: %w", err)
		}
	}
	return line, nil
}

// ---------------------------------------------------------------- ledgers

var (
	reLedgerLine = regexp.MustCompile(`^\s*-\s*(\d{4}-\d{2}-\d{2})\b`)
	//nolint:gosmopolitan // the review ledgers record a miss as 見逃し (observe.py:47)
	reNoMiss = regexp.MustCompile(`見逃し(?:\s|\x{3000})*(?:0|なし|無し)`)
)

const missWord = "見逃し" //nolint:gosmopolitan // the ledger word for a miss (observe.py:314)

// ledgers reports review-ledger lines that record a miss on a day whose local start lies in the
// window (observe.py:300-321).
func (s *state) ledgers(st *Source) error {
	root := filepath.Join(s.o.Home, ".claude", "review-ledgers")
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return errOff
	}
	paths, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		return errors.New("review-ledgers unreadable") //nolint:err113 // a fixed per-cause summary
	}
	if _, err := os.ReadDir(root); err != nil {
		return errors.New("review-ledgers unreadable") //nolint:err113 // a fixed per-cause summary
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := s.work.Err(); err != nil {
			return fmt.Errorf("ledgers: %w", err)
		}
		st.Files++
		info, err := os.Lstat(path)
		if err == nil && !info.Mode().IsRegular() {
			st.unreadable = append(st.unreadable, unreadable(root, path, errNotRegular))
			continue
		}
		var data []byte
		if err == nil {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			st.unreadable = append(st.unreadable, unreadable(root, path, err))
			continue
		}
		for i, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			if err := s.work.Err(); err != nil {
				return fmt.Errorf("ledgers: %w", err)
			}
			if line == "" {
				continue
			}
			st.Records++
			m := reLedgerLine.FindStringSubmatch(line)
			if m == nil || !strings.Contains(line, missWord) || reNoMiss.MatchString(line) {
				continue
			}
			day, err := time.ParseInLocation(time.DateOnly, m[1], s.o.Location)
			if err != nil || !s.inWindow(day) {
				continue
			}
			s.add("ledger-miss", fmt.Sprintf("%s:%d", path, i+1), line, day, "synthetic", "")
		}
	}
	return nil
}
