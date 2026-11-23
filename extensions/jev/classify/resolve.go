package classify

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shuymn/stack/extensions/internal/t3loc"
)

// Reasons an item is unresolved (§3.2 Resolution).
const (
	ReasonRecordChanged   = "record changed"
	ReasonAmbiguous       = "ambiguous locator"
	ReasonIntervening     = "intervening prompt"
	ReasonNoAssistantText = "no assistant text"
	ReasonSourceGone      = "source gone"
	ReasonSQLiteFailed    = "sqlite3 failed"
	ReasonBadLocator      = "unknown locator"
)

// Openers of a turn: what the walk met where the turn began.
const (
	OpenerOwner      = "owner"
	OpenerPrompt     = "prompt"
	OpenerCompaction = "compaction"
	OpenerNone       = "none"
)

// kind is how the turn walk sees a record.
type kind int

const (
	// recIgnored is passed over everywhere: a record of another type, a malformed or skipped
	// line.
	recIgnored kind = iota
	// recToolResult is any user record that is not a prompt, or a T3 agent-created row.
	recToolResult
	// recOwner is an owner turn, under observe's owner rule.
	recOwner
	// recPrompt is any other prompt: a typed command, a task notification, a server-created row,
	// a prompt of another origin.
	recPrompt
	// recCompaction is a prompt that marks compaction: a compaction summary or a T3 compaction row.
	recCompaction
	// recText is an assistant record with text.
	recText
	// recToolCall is any other assistant record.
	recToolCall
)

// record is one source record as the walk needs it.
type record struct {
	kind kind
	text string
	ts   string
	cwd  string
}

// turn is what the walk attributes to a reply.
type turn struct {
	owner, assistant, opener string
	reason                   string
}

// walk attributes the reply at recs[at] to its turn (§3.2 Turn boundary): back to the first
// assistant record passing over only tool results and owner turns, then back to the turn's
// opener, taking the turn's last run of text records as its closing message.
func walk(recs []record, at int) turn {
	i := at - 1
	for ; i >= 0; i-- {
		k := recs[i].kind
		if k == recPrompt || k == recCompaction {
			return turn{reason: ReasonIntervening}
		}
		if k == recText || k == recToolCall {
			break
		}
	}
	if i < 0 {
		return turn{reason: ReasonNoAssistantText}
	}
	const (
		seeking = iota
		inRun
		closed
	)
	var texts []string
	state := seeking
	t := turn{opener: OpenerNone}
walk:
	for ; i >= 0; i-- {
		r := recs[i]
		switch r.kind {
		case recText:
			if state != closed {
				texts = append(texts, r.text)
				state = inRun
			}
		case recToolCall, recToolResult:
			if state == inRun {
				state = closed
			}
		case recOwner:
			t.opener, t.owner = OpenerOwner, r.text
			break walk
		case recPrompt:
			t.opener = OpenerPrompt
			break walk
		case recCompaction:
			t.opener = OpenerCompaction
			break walk
		case recIgnored:
		}
	}
	if len(texts) == 0 {
		return turn{reason: ReasonNoAssistantText}
	}
	slices.Reverse(texts)
	t.assistant = strings.Join(texts, "\n\n")
	return t
}

// Item is one owner-reply item through resolution and classification.
type Item struct {
	Obs ObserveItem
	// Index is the item's position among observe's items.
	Index int
	// Resolved is set when the walk attributed the reply.
	Resolved bool
	// Reason explains an unresolved item.
	Reason                          string
	Owner, Assistant, Reply, Opener string
	// Cwd is the session's working directory; CwdUnknown marks a T3 cwd query failure.
	Cwd        string
	CwdUnknown bool
	ts         time.Time
}

// check compares the record now at the locator with observe's item: an owner turn whose text
// cuts to the item's excerpt and whose time is the item's ts.
func check(it *Item, r record) bool {
	if r.kind != recOwner || excerpt(r.text) != it.Obs.Excerpt {
		return false
	}
	t, ok := parseTime(r.ts)
	return ok && iso(t) == it.Obs.TS
}

// attribute fills a resolved or unresolved item from the record at recs[at].
func attribute(it *Item, recs []record, at int) {
	if !check(it, recs[at]) {
		it.Reason = ReasonRecordChanged
		return
	}
	t := walk(recs, at)
	if t.reason != "" {
		it.Reason = t.reason
		return
	}
	it.Resolved = true
	it.Owner, it.Assistant, it.Opener = t.owner, t.assistant, t.opener
	it.Reply = recs[at].text
}

// splitPathLine splits a transcript locator on its last ':'.
func splitPathLine(where string) (string, int, bool) {
	i := strings.LastIndexByte(where, ':')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(where[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return where[:i], n, true
}

// resolver reads the sources of the items, each once.
type resolver struct {
	// base resolves relative locator paths and cwds (the extension's cwd).
	base string
	t3   T3
}

func (r *resolver) abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(r.base, filepath.FromSlash(p))
}

// errPrepDeadline is the work deadline reached while the sources are read.
var errPrepDeadline = errors.New("preparation deadline")

// resolve resolves every item in place. It returns errPrepDeadline when the work deadline comes
// first; items not yet resolved then stay unresolved without a reason.
func (r *resolver) resolve(ctx context.Context, items []*Item, since, until time.Time) error {
	files := map[string][]*Item{}
	var order []string
	var t3Items []*Item
	for _, it := range items {
		if strings.HasPrefix(it.Obs.Where, t3loc.Prefix) {
			t3Items = append(t3Items, it)
			continue
		}
		path, _, ok := splitPathLine(it.Obs.Where)
		if !ok {
			it.Reason = ReasonBadLocator
			continue
		}
		if _, seen := files[path]; !seen {
			order = append(order, path)
		}
		files[path] = append(files[path], it)
	}
	for _, path := range order {
		if ctx.Err() != nil {
			return errPrepDeadline
		}
		if err := r.transcript(ctx, path, files[path]); err != nil {
			return err
		}
	}
	if len(t3Items) > 0 {
		return r.resolveT3(ctx, t3Items, since, until)
	}
	return nil
}

func (r *resolver) transcript(ctx context.Context, path string, items []*Item) error {
	upTo := 0
	for _, it := range items {
		_, n, _ := splitPathLine(it.Obs.Where)
		upTo = max(upTo, n)
	}
	recs, err := readTranscript(ctx, r.abs(path), upTo)
	if ctx.Err() != nil {
		return errPrepDeadline
	}
	for _, it := range items {
		_, n, _ := splitPathLine(it.Obs.Where)
		if err != nil || n > len(recs) {
			it.Reason = ReasonSourceGone
			continue
		}
		attribute(it, recs, n-1)
		if it.Resolved {
			it.Cwd = r.abs(recs[n-1].cwd)
		}
	}
	return nil
}

// resolveT3 resolves the T3 items: the window query, each matched thread, then the cwds.
//
//nolint:gocyclo // one outcome per query and per deadline (§3.2 Resolution)
func (r *resolver) resolveT3(ctx context.Context, items []*Item, since, until time.Time) error {
	fail := func(reason string) {
		for _, it := range items {
			it.Reason = reason
		}
	}
	if r.t3 == nil {
		fail(ReasonSourceGone)
		return nil
	}
	rows, err := r.t3.Users(ctx, since, until)
	if ctx.Err() != nil || errors.Is(err, errSQLiteTimeout) {
		return errPrepDeadline
	}
	if errors.Is(err, errNoDatabase) {
		fail(ReasonSourceGone)
		return nil
	}
	if err != nil {
		// A failed locator query marks every T3 item; the run goes on.
		fail(ReasonSQLiteFailed)
		return nil //nolint:nilerr // recorded on the items
	}
	count := map[string]int{}
	thread := map[string]string{}
	// message is the SQL message_id of each locator's row in the window: the thread's rows outside
	// the window can share the locator (a null payload id).
	message := map[string]string{}
	for _, row := range rows {
		p, ok := row.payload()
		if !ok {
			continue
		}
		loc := t3loc.Locator(row.ThreadID, p["id"])
		count[loc]++
		thread[loc], message[loc] = row.ThreadID, row.MessageID
	}
	byThread := map[string][]*Item{}
	var threads []string
	for _, it := range items {
		switch count[it.Obs.Where] {
		case 0:
			it.Reason = ReasonSourceGone
			continue
		case 1:
		default:
			it.Reason = ReasonAmbiguous
			continue
		}
		th := thread[it.Obs.Where]
		if _, seen := byThread[th]; !seen {
			threads = append(threads, th)
		}
		byThread[th] = append(byThread[th], it)
	}
	for _, th := range threads {
		if ctx.Err() != nil {
			return errPrepDeadline
		}
		rows, err := r.t3.Thread(ctx, th)
		if ctx.Err() != nil || errors.Is(err, errSQLiteTimeout) {
			return errPrepDeadline
		}
		recs := make([]record, len(rows))
		at := map[string]int{}
		for i, row := range rows {
			p, ok := row.payload()
			recs[i] = record{kind: recIgnored}
			if ok {
				recs[i] = row.record(p)
			}
			if !ok || row.Role != "user" {
				continue
			}
			if loc := t3loc.Locator(th, p["id"]); message[loc] == row.MessageID {
				at[loc] = i
			}
		}
		for _, it := range byThread[th] {
			i, ok := at[it.Obs.Where]
			switch {
			case err != nil:
				it.Reason = ReasonSQLiteFailed
			case !ok:
				it.Reason = ReasonSourceGone
			default:
				attribute(it, recs, i)
			}
		}
	}
	if len(threads) == 0 {
		return nil
	}
	cwds, err := r.t3.Cwds(ctx, threads)
	if ctx.Err() != nil || errors.Is(err, errSQLiteTimeout) {
		return errPrepDeadline
	}
	for _, th := range threads {
		for _, it := range byThread[th] {
			if !it.Resolved {
				continue
			}
			cwd, ok := cwds[th]
			if err != nil || !ok || cwd == "" {
				it.CwdUnknown = true
				continue
			}
			it.Cwd = r.abs(cwd)
		}
	}
	return nil
}

// T3Row is one message row of the T3 Code state database.
type T3Row struct {
	ThreadID  string `json:"thread_id,omitempty"` //nolint:tagliatelle // SQL column
	MessageID string `json:"message_id"`          //nolint:tagliatelle // SQL column
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`   //nolint:tagliatelle // SQL column
	Payload   string `json:"payload_json"` //nolint:tagliatelle // SQL column
}

func (row T3Row) payload() (map[string]any, bool) {
	var p map[string]any
	if json.Unmarshal([]byte(row.Payload), &p) != nil || p == nil {
		return nil, false
	}
	return p, true
}

// record sorts a T3 row into the walk's kinds with observe's owner rule (sources.go): a user row
// created by the user from web or mobile, not an idle compaction or a /compact command, is an
// owner turn; another user-created row a prompt (a compaction row a compaction prompt); an
// agent-created row a tool result. An assistant row is text when its text is non-empty.
// p is the row's decoded payload.
func (row T3Row) record(p map[string]any) record {
	text, _ := p["text"].(string)
	r := record{text: text, ts: row.CreatedAt}
	switch row.Role {
	case "user":
		src, _ := p["creationSource"].(string)
		switch {
		case p["createdBy"] != "user":
			r.kind = recToolResult
		case p["idleCompaction"] != nil ||
			strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "/compact"):
			r.kind = recCompaction
		case src == "web" || src == "mobile":
			r.kind = recOwner
		default:
			r.kind = recPrompt
		}
	case "assistant":
		r.kind = recToolCall
		if text != "" {
			r.kind = recText
		}
	default:
		r.kind = recIgnored
	}
	return r
}

// errNoDatabase reports a T3 database that does not exist.
var errNoDatabase = errors.New("t3 database missing")

// T3 reads the T3 Code rows classify needs: the user rows of observe's window, each matched
// thread's rows in order, and each thread's cwd.
type T3 interface {
	Users(ctx context.Context, since, until time.Time) ([]T3Row, error)
	Thread(ctx context.Context, thread string) ([]T3Row, error)
	Cwds(ctx context.Context, threads []string) (map[string]string, error)
}

// ReplayThread is one thread of a fixture's jev.t3: its cwd and its rows.
type ReplayThread struct {
	Cwd  string  `json:"cwd"`
	Rows []T3Row `json:"rows"`
}

// replayT3 answers from a fixture's jev.t3 in place of sqlite3 (§3.4).
type replayT3 map[string]ReplayThread

// t3Time formats a bound as T3 stores created_at (observe's t3Time).
func t3Time(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

func (r replayT3) Users(_ context.Context, since, until time.Time) ([]T3Row, error) {
	lo, hi := t3Time(since), t3Time(until)
	var out []T3Row
	for _, th := range slices.Sorted(maps.Keys(r)) {
		for _, row := range r[th].Rows {
			if row.Role == "user" && row.CreatedAt >= lo && row.CreatedAt < hi {
				row.ThreadID = th
				out = append(out, row)
			}
		}
	}
	return out, nil
}

func (r replayT3) Thread(_ context.Context, thread string) ([]T3Row, error) {
	rows := slices.Clone(r[thread].Rows)
	slices.SortStableFunc(rows, func(a, b T3Row) int {
		if c := strings.Compare(a.CreatedAt, b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.MessageID, b.MessageID)
	})
	return rows, nil
}

func (r replayT3) Cwds(_ context.Context, threads []string) (map[string]string, error) {
	out := map[string]string{}
	for _, th := range threads {
		if t, ok := r[th]; ok {
			out[th] = t.Cwd
		}
	}
	return out, nil
}
