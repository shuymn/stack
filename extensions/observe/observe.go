// Package observe is the stack-observe extension (docs/design/phase3-cycle.md §3.3): it recomputes a
// fixed 7-day window of evidence on every run, writes ~/.local/state/stack/observe/latest.json and
// answers the cycle stage with pass or warn, never block.
//
// Sources run in order (transcripts, t3, codex, ledgers, gh, replay, gh-version); each is
// independent, so a failure marks only that source. Observation reads only: nothing here writes
// except latest.json.
package observe

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: item ids are legacy SHA-1 prefixes, not a security boundary.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
)

// Statuses of a response and of a source.
const (
	StatusPass = "pass"
	StatusWarn = "warn"

	srcOK      = "ok"
	srcFail    = "fail"
	srcOff     = "off"
	srcTimeout = "timeout"
)

// Finding kinds.
const (
	kindGhAuth       = "gh-auth"
	kindReplayChange = "replay-change"
	// kindReplayHookMissing: stack-guard is not on PATH, so the replay ran through stack only.
	kindReplayHookMissing = "replay-hook-missing"
	kindGhVersion         = "gh-version"
	kindLatestMissing     = "latest-not-written"
	kindInternal          = "internal-error"
)

const (
	window = 7 * 24 * time.Hour
	// defaultBudget and defaultReserve are the §3.9 observe deadline and its reserve for
	// latest.json and the response.
	defaultBudget  = 5 * time.Minute
	defaultReserve = 20 * time.Second

	excerptLen   = 160
	detailLen    = 300
	contextLen   = 4000
	shortReply   = 300
	maxExcerpts  = 3
	latestSchema = 1

	// §3.9 limits: gh calls 60 s each, the hook's own 5 s settings timeout, 4 replays at a time,
	// 100,000 payloads.
	defaultGhTimeout     = 60 * time.Second
	defaultGitTimeout    = 10 * time.Second
	defaultReplayTimeout = 5 * time.Second
	defaultReplayCap     = 100000
	defaultReplayWorkers = 4
	// reserveShare bounds a reserve that does not fit a short test budget.
	reserveShare    = 4
	shownUnreadable = 5
	contextError    = 120
)

// Item kinds per signal (legacy observe.py:37-38).
var signals = []struct{ name, kind string }{
	{"owner-reply", "miss"},
	{"revert", "miss"},
	{"fix-pr", "miss"},
	{"ci-fail-main", "miss"},
	{"ledger-miss", "miss"},
	{"meta-effort", "waste"},
	{"hiden-in-worker", "waste"},
	{"reference-unread", "waste"},
}

// sourceNames is the run order of the sources that have a status in latest.json.
var sourceNames = []string{"transcripts", "t3", "codex", "ledgers", "gh", "replay"}

// Options configure one observe run. Zero values take the defaults.
type Options struct {
	// Home is the user's home directory; every source root and latest.json derive from it.
	Home string
	// Env is the environment children start with (repository-location variables are dropped);
	// nil means os.Environ().
	Env []string
	// LookPath finds gh, git, sqlite3, stack and stack-guard; nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Now is the clock of the window; nil means time.Now.
	Now func() time.Time
	// Location is the zone of ledger days; nil means time.Local.
	Location *time.Location
	// Budget is the time from start to the deadline; Reserve is the end of it kept for writing
	// latest.json and the response.
	Budget, Reserve time.Duration
	// Stderr receives progress lines; nil discards them.
	Stderr io.Writer

	// Test seams and limits; zero means the §3.9 values.
	ghTimeout     time.Duration
	gitTimeout    time.Duration
	replayTimeout time.Duration
	replayCap     int
	replayWorkers int
}

// Finding is one owner-actionable line of the response.
type Finding struct {
	Kind   string `json:"kind"`
	Where  string `json:"where"`
	Detail string `json:"detail"`
}

// Response is the extension protocol answer.
type Response struct {
	Status   string    `json:"status"`
	Context  string    `json:"context"`
	Findings []Finding `json:"findings"`
}

// Item is one signal occurrence in latest.json.
type Item struct {
	ID         string `json:"id"`
	Signal     string `json:"signal"`
	Kind       string `json:"kind"`
	Where      string `json:"where"`
	Excerpt    string `json:"excerpt"`
	Provenance string `json:"provenance"`
	Repo       string `json:"repo"`
	TS         string `json:"ts"`
}

// Source is the status of one source in latest.json.
//
//nolint:tagliatelle // latest.json uses snake_case (design §3.3).
type Source struct {
	Status  string `json:"status"`
	Files   int    `json:"files"`
	Records int    `json:"records"`
	Error   string `json:"error"`
	// ReposRead is set for gh only: the repositories read completely before the first error.
	ReposRead  *int     `json:"repos_read,omitempty"`
	unreadable []string `json:"-"`
	// skipped counts transcript lines longer than maxLine.
	skipped int `json:"-"`
}

// Tally counts T3 and transcript user turns.
type Tally struct {
	Owner     int `json:"owner"`
	Synthetic int `json:"synthetic"`
	Unknown   int `json:"unknown"`
}

// ReplayTotals counts the replay of recorded PreToolUse decisions.
//
//nolint:tagliatelle // latest.json uses snake_case (design §3.3).
type ReplayTotals struct {
	Compared      int `json:"compared"`
	Changed       int `json:"changed"`
	NotComparable int `json:"not_comparable"`
	Unrecoverable int `json:"unrecoverable"`
	TruncatedCap  int `json:"truncated_cap"`
	TruncatedTime int `json:"truncated_time"`
	// ChangedByKind splits changed by the three change kinds of §3.3 replay step 3.
	ChangedByKind ChangeKinds `json:"changed_by_kind"`
}

// ChangeKinds counts replay changes per kind.
//
//nolint:tagliatelle // latest.json uses snake_case (design §3.3).
type ChangeKinds struct {
	NewDeny     int `json:"new_deny"`
	LostDeny    int `json:"lost_deny"`
	RuleChanged int `json:"rule_changed"`
}

// Latest is the latest.json document (schema version 1).
type Latest struct {
	Version int `json:"version"`
	Window  struct {
		Since string `json:"since"`
		Until string `json:"until"`
	} `json:"window"`
	Sources map[string]*Source `json:"sources"`
	Tally   Tally              `json:"tally"`
	Items   []Item             `json:"items"`
	Replay  ReplayTotals       `json:"replay"`
	Gh      struct {
		Installed string `json:"installed"`
		Model     string `json:"model"`
	} `json:"gh"`
}

// state is one run.
type state struct {
	o            Options
	since, until time.Time
	// work ends at the deadline minus the reserve.
	work context.Context //nolint:containedctx // one run's deadline, shared by every step of it

	latest   Latest
	findings []Finding
	seen     map[string]bool

	// collected by the transcripts and codex sources for later sources
	cwds       map[string]int
	refs       map[string]bool
	t3Sessions map[string]bool
	calls      []*toolCall
	// strs interns the cwd, session id and tool name strings of calls.
	strs map[string]string
}

// Run observes the window ending now and returns the protocol response. It never answers block.
func Run(ctx context.Context, o Options) Response {
	o = o.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, o.Budget)
	defer cancel()
	work, cancelWork := context.WithTimeout(ctx, o.Budget-o.Reserve)
	defer cancelWork()

	until := o.Now().UTC().Truncate(time.Second)
	s := &state{
		o: o, since: until.Add(-window), until: until, work: work,
		seen: map[string]bool{}, cwds: map[string]int{}, refs: map[string]bool{},
		t3Sessions: map[string]bool{}, strs: map[string]string{},
	}
	s.latest.Version = latestSchema
	s.latest.Window.Since, s.latest.Window.Until = iso(s.since), iso(s.until)
	s.latest.Sources = map[string]*Source{}
	s.latest.Items = []Item{}
	s.latest.Gh.Installed, s.latest.Gh.Model = unknown, unknown

	s.source("transcripts", s.transcripts)
	s.source("t3", s.t3)
	s.source("codex", s.codex)
	s.source("ledgers", s.ledgers)
	if st := s.latest.Sources["t3"]; st.Status != srcOK {
		// Owner turns of T3 sessions are read only from the database: unread, they are unknown.
		s.latest.Tally.Unknown += len(s.t3Sessions)
	}
	s.source("gh", s.gh)
	// Before replay: replay uses whatever time is left, and its workers run until the deadline.
	s.guarded("gh-version", func() { s.ghVersion() })
	s.source("replay", s.replay)

	path := latestPath(o.Home)
	return s.response(path, s.publish(ctx, path))
}

// publish writes latest.json unless the deadline has passed before the write starts, checked
// before encoding and again right before writing; a write already started runs to its end (a
// local file's sync and rename are not interruptible). The finding names the failed step only:
// the error itself quotes local paths.
func (s *state) publish(ctx context.Context, path string) bool {
	const late = "deadline passed before writing latest.json"
	if ctx.Err() != nil {
		s.finding(kindLatestMissing, path, late)
		return false
	}
	data, err := marshalLatest(s.latest)
	if err != nil {
		s.finding(kindLatestMissing, path, "cannot encode latest.json")
		return false
	}
	if ctx.Err() != nil {
		s.finding(kindLatestMissing, path, late)
		return false
	}
	if err := writeLatest(path, data); err != nil {
		detail := "home directory unknown"
		if we, ok := errors.AsType[*writeError](err); ok {
			detail = "cannot " + we.step
		}
		_, _ = fmt.Fprintf(s.o.Stderr, "latest.json: %v\n", err)
		s.finding(kindLatestMissing, path, detail)
		return false
	}
	return true
}

func (o Options) withDefaults() Options {
	if o.Env == nil {
		o.Env = os.Environ()
	}
	o.Env = dropRepoVars(o.Env)
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local //nolint:gosmopolitan // ledger days are the owner's local days (design §2)
	}
	if o.Budget <= 0 {
		o.Budget = defaultBudget
	}
	if o.Reserve <= 0 || o.Reserve >= o.Budget {
		o.Reserve = min(defaultReserve, o.Budget/reserveShare)
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.ghTimeout <= 0 {
		o.ghTimeout = defaultGhTimeout
	}
	if o.gitTimeout <= 0 {
		o.gitTimeout = defaultGitTimeout
	}
	if o.replayTimeout <= 0 {
		o.replayTimeout = defaultReplayTimeout
	}
	if o.replayCap <= 0 {
		o.replayCap = defaultReplayCap
	}
	if o.replayWorkers <= 0 {
		o.replayWorkers = defaultReplayWorkers
	}
	return o
}

// errOff marks a source whose input does not exist on this machine.
var errOff = errors.New("off")

// source runs one source under the work deadline and records its status. A source not started
// or not finished by the deadline is timeout, even when it returns without an error after it; an
// error is fail; unreadable files fail it too.
func (s *state) source(name string, fn func(*Source) error) {
	st := &Source{Status: srcOK}
	if name == "gh" {
		st.ReposRead = new(int)
	}
	s.latest.Sources[name] = st
	start := time.Now()
	// The replay still runs past the deadline: it starts nothing then, and counts what it skipped
	// as truncated_time.
	if s.work.Err() != nil && name != "replay" {
		st.Status = srcTimeout
		s.progress(name, st, start)
		return
	}
	err := s.call(name, func() error { return fn(st) })
	switch {
	case errors.Is(err, errOff):
		st.Status = srcOff
	case isDeadline(err) || s.work.Err() != nil:
		st.Status = srcTimeout
		st.Error = "deadline passed"
	case err != nil:
		st.Status = srcFail
		st.Error = cut(err.Error(), detailLen)
	case len(st.unreadable) > 0:
		st.Status = srcFail
		shown := st.unreadable[:min(len(st.unreadable), shownUnreadable)]
		st.Error = cut(
			fmt.Sprintf("%d unreadable: %s", len(st.unreadable), strings.Join(shown, ", ")),
			detailLen,
		)
	}
	s.progress(name, st, start)
}

// call runs fn, turning a panic into an error.
func (s *state) call(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %s panicked: %v", errInternal, name, r)
			_, _ = fmt.Fprintf(s.o.Stderr, "%s panicked: %v\n%s", name, r, debug.Stack())
		}
	}()
	return fn()
}

var errInternal = errors.New("internal error")

// guarded runs a step that has no source status; a panic becomes an internal-error finding.
func (s *state) guarded(name string, fn func()) {
	if err := s.call(name, func() error { fn(); return nil }); err != nil {
		s.finding(kindInternal, name, err.Error())
	}
}

func (s *state) progress(name string, st *Source, start time.Time) {
	msg := fmt.Sprintf(
		"%s: %s (%d files, %d records, %.1fs)",
		name,
		st.Status,
		st.Files,
		st.Records,
		time.Since(start).Seconds(),
	)
	if st.skipped > 0 {
		msg += fmt.Sprintf(" [%d lines over %d MiB skipped]", st.skipped, maxLine/mib)
	}
	if st.Error != "" {
		msg += ": " + st.Error
	}
	if name == "replay" {
		k := s.latest.Replay.ChangedByKind
		msg += fmt.Sprintf(" [changed: new_deny %d, lost_deny %d, rule_changed %d]",
			k.NewDeny, k.LostDeny, k.RuleChanged)
	}
	_, _ = fmt.Fprintln(s.o.Stderr, msg)
}

func (s *state) finding(kind, where, detail string) {
	s.findings = append(
		s.findings,
		Finding{Kind: kind, Where: where, Detail: cut(detail, detailLen)},
	)
}

// add appends an item unless one with the same id exists (legacy observe.py:73-76,409-413).
func (s *state) add(signal, where, excerpt string, ts time.Time, provenance, repo string) {
	id := itemID(signal, where)
	if s.seen[id] {
		return
	}
	s.seen[id] = true
	kind := ""
	for _, sg := range signals {
		if sg.name == signal {
			kind = sg.kind
		}
	}
	s.latest.Items = append(s.latest.Items, Item{
		ID: id, Signal: signal, Kind: kind, Where: where, Excerpt: cut(excerpt, excerptLen),
		Provenance: provenance, Repo: repo, TS: iso(ts),
	})
}

func itemID(signal, where string) string {
	sum := sha1.Sum([]byte(signal + "\x00" + where)) //nolint:gosec // G401: see the import.
	return hex.EncodeToString(sum[:])[:12]
}

// child runs a tool under the work deadline with the run's environment.
func (s *state) child(c command) outcome {
	if c.env == nil {
		c.env = s.o.Env
	}
	return run(s.work, c)
}

// response builds the protocol answer.
func (s *state) response(path string, written bool) Response {
	status := StatusPass
	for _, name := range sourceNames {
		if st := s.latest.Sources[name]; st.Status == srcFail || st.Status == srcTimeout {
			status = StatusWarn
		}
	}
	if len(s.findings) > 0 {
		// Every finding kind observe emits is owner-actionable (gh-auth, replay-change,
		// replay-hook-missing, gh-version, latest-not-written, internal-error).
		status = StatusWarn
	}
	findings := s.findings
	if findings == nil {
		findings = []Finding{}
	}
	return Response{Status: status, Context: s.digest(path, written), Findings: findings}
}

// digest is the English context. The counts come first (window, sources, tally, per-signal
// counts, replay totals, gh versions, latest.json); the excerpts, up to 3 per signal, fill what
// is left of the 4,000 characters.
func (s *state) digest(path string, written bool) string {
	var b strings.Builder
	l := &s.latest
	fmt.Fprintf(&b, "observe window %s .. %s\n", l.Window.Since, l.Window.Until)
	b.WriteString("sources:")
	for i, name := range sourceNames {
		st := l.Sources[name]
		sep := ";"
		if i == 0 {
			sep = ""
		}
		fmt.Fprintf(&b, "%s %s %s", sep, name, st.Status)
		if st.Status != srcOff {
			fmt.Fprintf(&b, " (files %d, records %d", st.Files, st.Records)
			if st.ReposRead != nil {
				fmt.Fprintf(&b, ", repos read %d", *st.ReposRead)
			}
			if st.skipped > 0 {
				fmt.Fprintf(&b, ", long lines skipped %d", st.skipped)
			}
			b.WriteString(")")
		}
		if st.Error != "" {
			fmt.Fprintf(&b, " [%s]", cut(st.Error, contextError))
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "owner tally: owner %d, synthetic %d, unknown %d\n",
		l.Tally.Owner, l.Tally.Synthetic, l.Tally.Unknown)
	counts := map[string]int{}
	excerpts := map[string][]string{}
	for _, it := range l.Items {
		counts[it.Signal]++
		if len(excerpts[it.Signal]) < maxExcerpts {
			excerpts[it.Signal] = append(excerpts[it.Signal], fmt.Sprintf("%q", it.Excerpt))
		}
	}
	b.WriteString("signals:")
	for i, sg := range signals {
		sep := ","
		if i == 0 {
			sep = ""
		}
		fmt.Fprintf(&b, "%s %s %d", sep, sg.name, counts[sg.name])
	}
	b.WriteString("\n")
	r := l.Replay
	k := r.ChangedByKind
	fmt.Fprintf(
		&b,
		"replay: compared %d, changed %d (new_deny %d, lost_deny %d, rule_changed %d), "+
			"not_comparable %d, unrecoverable %d, truncated_cap %d, truncated_time %d\n",
		r.Compared,
		r.Changed,
		k.NewDeny,
		k.LostDeny,
		k.RuleChanged,
		r.NotComparable,
		r.Unrecoverable,
		r.TruncatedCap,
		r.TruncatedTime,
	)
	fmt.Fprintf(&b, "gh: installed %s, modelled %s\n", l.Gh.Installed, l.Gh.Model)
	if written {
		fmt.Fprintf(&b, "latest.json: %s", path)
	} else {
		fmt.Fprintf(&b, "latest.json: not written (%s)", path)
	}
	text := truncate(b.String(), contextLen)
	room := contextLen - utf8.RuneCountInString(text)
	var ex strings.Builder
	for _, sg := range signals {
		quoted := excerpts[sg.name]
		if len(quoted) == 0 {
			continue
		}
		line := fmt.Sprintf("\n  %s: %s", sg.name, strings.Join(quoted, " | "))
		if n := utf8.RuneCountInString(line); n > room {
			if room > len("\n  ")+len(sg.name)+len(": …") {
				ex.WriteString(truncate(line, room))
			}
			break
		}
		ex.WriteString(line)
		room -= utf8.RuneCountInString(line)
	}
	return text + ex.String()
}

const unknown = "unknown"

func iso(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// parseTime reads an ISO 8601 timestamp; one without a zone is UTC (legacy observe.py:60-65).
func parseTime(v string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999", v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func (s *state) inWindow(t time.Time) bool {
	return !t.Before(s.since) && t.Before(s.until)
}

// cut collapses whitespace and keeps at most n characters, marking a cut with an ellipsis
// (legacy observe.py:68-70).
func cut(text string, n int) string {
	return truncate(strings.Join(strings.Fields(text), " "), n)
}

// latestPath is ~/.local/state/stack/observe/latest.json, or "" without a home.
func latestPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "stack", "observe", "latest.json")
}

// truncate keeps at most n characters of text, marking a cut with an ellipsis; unlike cut it keeps
// line breaks.
func truncate(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n-1]) + "…"
}
