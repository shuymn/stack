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
	"strings"
	"time"
)

// Byte patterns that decide whether a transcript line is parsed at all (legacy observe.py:51-53).
var (
	reT3Entry    = regexp.MustCompile(`"entrypoint":\s*"t3-code"`)
	reUser       = regexp.MustCompile(`"type":\s*"user"`)
	reAssistant  = regexp.MustCompile(`"type":\s*"assistant"`)
	reToolUse    = regexp.MustCompile(`"type":\s*"tool_use"`)
	reToolUseID  = regexp.MustCompile(`"tool_use_id":\s*"([^"\\]+)"`)
	reReference  = regexp.MustCompile(`skills/hiden/references/([\w.-]+\.md)`)
	metaNames    = []string{"review-ledgers/", "retro.md"}
	typedPrefixs = []string{"<command-", "<task-notification>", "<local-command"}
)

// wiredTools are the tools stack hook claude pre-tool-use is wired for (phase2-core.md §4.3).
var wiredTools = map[string]bool{
	"Bash": true, "Read": true, "NotebookRead": true, "Grep": true, "Glob": true,
	"mcp__pencil__execute": true,
}

// t3Scan is how many leading lines decide whether T3 Code ran a session (observe.py:128).
const t3Scan = 50

// lineBuffer is the transcript read buffer; longer lines grow past it.
const lineBuffer = 1 << 20

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"` //nolint:tagliatelle // transcript format
	Content   json.RawMessage `json:"content"`
}

type record struct {
	Type             string          `json:"type"`
	Timestamp        string          `json:"timestamp"`
	CWD              string          `json:"cwd"`
	SessionID        string          `json:"sessionId"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolDenialKind     string `json:"toolDenialKind"`
	PermissionDecision *struct {
		Source string `json:"source"`
	} `json:"permissionDecision"`
}

// toolCall is one in-window call of a guard-wired tool, with its recorded decision.
type toolCall struct {
	path      string
	line      int
	sessionID string
	cwd       string
	tool      string
	input     json.RawMessage
	ts        string
	recorded  string
}

// transcripts reads ~/.claude/projects/*/*.jsonl and …/subagents/**/*.jsonl whose mtime is at or
// after since (observe.py:187-201).
func (s *state) transcripts(st *Source) error {
	root := filepath.Join(s.o.Home, ".claude", "projects")
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return errOff
	}
	// A symlinked root (a dotfiles layout) is walked; links below it are not followed.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if werr := s.work.Err(); werr != nil {
			return fmt.Errorf("transcripts: %w", werr)
		}
		if err != nil {
			st.unreadable = append(st.unreadable, unreadable(path, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		subagent := slices.Contains(parts[:len(parts)-1], "subagents")
		if len(parts) != 2 && !subagent {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			st.unreadable = append(st.unreadable, unreadable(path, err))
			return nil
		}
		if !info.Mode().IsRegular() {
			st.unreadable = append(st.unreadable, path+": not a regular file")
			return nil
		}
		if info.ModTime().Before(s.since) {
			return nil
		}
		st.Files++
		if err := s.scanTranscript(path, subagent, st); err != nil {
			st.unreadable = append(st.unreadable, unreadable(path, err))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}
	return nil
}

func unreadable(path string, err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return path + ": " + pe.Err.Error()
	}
	return path + ": " + err.Error()
}

// completeLines calls fn with each newline-terminated line of r; an unterminated last line is
// still being written and is left out.
func completeLines(r io.Reader, fn func(n int, line []byte)) error {
	br := bufio.NewReaderSize(r, lineBuffer)
	n := 0
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		n++
		fn(n, line[:len(line)-1])
	}
}

// scanTranscript reads one transcript (observe.py:125-184) and collects guard-wired tool calls
// with their recorded decisions for the replay.
func (s *state) scanTranscript(path string, subagent bool, st *Source) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := &scanner{s: s, st: st, path: path, subagent: subagent, pending: map[string]*toolCall{}}
	// T3 is decided from the first lines, so they are held back until it is known.
	var held [][]byte
	flush := func() {
		for i, l := range held {
			sc.line(i+1, l)
		}
		held = nil
	}
	err = completeLines(f, func(n int, line []byte) {
		if n <= t3Scan {
			sc.t3 = sc.t3 || reT3Entry.Match(line)
			held = append(held, bytes.Clone(line))
			if n == t3Scan {
				flush()
			}
			return
		}
		sc.line(n, line)
	})
	flush()
	if err != nil {
		return err
	}
	sc.finish()
	return nil
}

// scanner is the state of one transcript scan.
type scanner struct {
	s                           *state
	st                          *Source
	path                        string
	subagent                    bool
	t3, seenUser, seenAssistant bool
	cwd                         string
	meta                        []string
	metaFirst, hiden            *toolCall
	pending                     map[string]*toolCall
}

func (sc *scanner) line(n int, line []byte) {
	isUser, isAssistant := reUser.Match(line), reAssistant.Match(line)
	if isAssistant && !reToolUse.Match(line) {
		sc.seenAssistant = true // a text turn: nothing else to read from it
		return
	}
	if !isUser && !isAssistant {
		return
	}
	if isUser && reToolUseID.Match(line) {
		if len(sc.pending) > 0 {
			sc.s.recordResults(line, sc.pending, sc.st)
		}
		return
	}
	var r record
	if json.Unmarshal(line, &r) != nil {
		return
	}
	sc.st.Records++
	t, ok := parseTime(r.Timestamp)
	in := ok && sc.s.inWindow(t)
	if sc.cwd == "" {
		sc.cwd = r.CWD
	}
	var content json.RawMessage
	if r.Message != nil {
		content = r.Message.Content
	}
	switch r.Type {
	case "user":
		sc.user(n, r, content, t, in)
	case "assistant":
		var blocks []block
		if json.Unmarshal(content, &blocks) != nil {
			return
		}
		sc.seenAssistant = true
		if !in {
			return
		}
		for _, b := range blocks {
			if b.Type == "tool_use" {
				sc.toolUse(n, r, b, t)
			}
		}
	}
}

func (sc *scanner) user(n int, r record, content json.RawMessage, t time.Time, in bool) {
	text, isText := typedText(r, content)
	if !isText {
		return
	}
	human := r.Origin != nil && r.Origin.Kind == "human"
	first := !sc.seenUser
	sc.seenUser = sc.seenUser || human || sc.t3
	switch {
	case in && !sc.subagent && !sc.t3 && human:
		sc.s.latest.Tally.Owner++
		if len([]rune(text)) <= shortReply && !first && sc.seenAssistant {
			sc.s.add("owner-reply", fmt.Sprintf("%s:%d", sc.path, n), text, t, "owner", "")
		}
	case in && !sc.subagent && sc.t3:
		sc.s.t3Sessions[strings.TrimSuffix(filepath.Base(sc.path), ".jsonl")] = true
	}
}

func (sc *scanner) toolUse(n int, r record, b block, t time.Time) {
	var args map[string]any
	_ = json.Unmarshal(b.Input, &args)
	blob := string(b.Input)
	if b.Name == "Read" {
		fp, _ := args["file_path"].(string)
		if m := reReference.FindStringSubmatch(fp); m != nil {
			sc.s.refs[m[1]] = true
		}
	}
	if containsAny(blob, metaNames) {
		if sc.metaFirst == nil {
			sc.metaFirst = &toolCall{line: n, ts: iso(t)}
		}
		sc.meta = append(sc.meta, b.Name+" "+blob)
	}
	if sc.subagent && sc.hiden == nil && ((b.Name == "Skill" && args["skill"] == "hiden") ||
		(b.Name == "Read" && strings.Contains(blob, "skills/hiden/"))) {
		sc.hiden = &toolCall{line: n, tool: b.Name + " " + blob, ts: iso(t)}
	}
	if wiredTools[b.Name] && b.ID != "" {
		call := &toolCall{
			path: sc.path, line: n, sessionID: r.SessionID, cwd: r.CWD, tool: b.Name,
			input: b.Input, ts: iso(t), recorded: decUnknown,
		}
		sc.pending[b.ID] = call
		sc.s.calls = append(sc.s.calls, call)
	}
}

// finish adds the per-transcript items and the cwd weight.
func (sc *scanner) finish() {
	if sc.metaFirst != nil {
		sc.s.add(
			"meta-effort",
			fmt.Sprintf("%s:%d", sc.path, sc.metaFirst.line),
			fmt.Sprintf(
				"%d meta tool calls; first: %s",
				len(sc.meta),
				sc.meta[0],
			),
			mustTime(sc.metaFirst.ts),
			"synthetic",
			"",
		)
	}
	if sc.hiden != nil {
		sc.s.add("hiden-in-worker", fmt.Sprintf("%s:%d", sc.path, sc.hiden.line), sc.hiden.tool,
			mustTime(sc.hiden.ts), "synthetic", "")
	}
	if sc.cwd != "" {
		sc.s.cwds[sc.cwd]++
	}
}

func mustTime(v string) time.Time {
	t, _ := parseTime(v)
	return t
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// typedText returns the text of a user record shaped like typed input: string content, not
// meta, a summary, a tool result or a command (observe.py:117-122).
func typedText(r record, content json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(content, &text) != nil {
		return "", false
	}
	if r.IsMeta || r.IsCompactSummary || len(r.ToolUseResult) > 0 {
		return "", false
	}
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, p := range typedPrefixs {
		if strings.HasPrefix(trimmed, p) {
			return "", false
		}
	}
	return text, true
}

// recordResults sets the recorded decision of each pending call answered by this user record.
func (s *state) recordResults(line []byte, pending map[string]*toolCall, st *Source) {
	var r record
	if json.Unmarshal(line, &r) != nil || r.Message == nil {
		return
	}
	st.Records++
	var blocks []block
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		call := pending[b.ToolUseID]
		if b.Type != "tool_result" || call == nil {
			continue
		}
		delete(pending, b.ToolUseID)
		call.recorded = recordedDecision(call.tool, resultText(b.Content), r)
	}
}

// resultText is a tool_result content: a string, or the text blocks of a list joined.
func resultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// Recorded and replayed decisions.
const (
	decNoDeny        = "no-deny"
	decOtherDeny     = "other-deny"
	decUnknown       = "unknown"
	decNotComparable = "not-comparable"
	denyPrefix       = "deny:"
	guardPrefix      = "stack guard ("
	hookErrorMark    = " hook error: "
)

// reBracketed is the "[<hook command>]: " that some Claude Code versions put between the hook
// error prefix and the hook's message.
var reBracketed = regexp.MustCompile(`^\[(?:[^\[\]]|\[[^\[\]]*\])*\]: \s*`)

// recordedDecision classifies the tool_result of a guard-wired call (design §3.3 replay step 1).
func recordedDecision(tool, text string, r record) string {
	switch r.ToolDenialKind {
	case "user-rejected", "cancelled", "interrupted":
		return decUnknown
	}
	if r.PermissionDecision != nil {
		switch r.PermissionDecision.Source {
		case "user_reject", "user_abort":
			return decUnknown
		}
	}
	prefix := "PreToolUse:" + tool + hookErrorMark
	if !strings.HasPrefix(text, prefix) {
		if strings.HasPrefix(text, "PreToolUse:") && strings.Contains(text, hookErrorMark) {
			return decOtherDeny
		}
		return decNoDeny
	}
	rest := reBracketed.ReplaceAllString(text[len(prefix):], "")
	// Several hooks: a second hook error in the same result. Only a lone stack guard deny counts.
	if strings.Contains(rest, hookErrorMark) || !strings.HasPrefix(rest, guardPrefix) {
		return decOtherDeny
	}
	rule, _, ok := strings.Cut(rest[len(guardPrefix):], ")")
	if !ok || rule == "" || strings.ContainsAny(rule, " \n") {
		return decOtherDeny
	}
	return denyPrefix + rule
}

// referenceUnread reports each live hiden reference no in-window Read named (observe.py:204-207).
func (s *state) referenceUnread() {
	dir := filepath.Join(s.o.Home, ".claude", "skills", "hiden", "references")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || s.refs[name] {
			continue
		}
		s.add(
			"reference-unread",
			filepath.Join(dir, name),
			"references/"+name+" not read in the window",
			s.until,
			"synthetic",
			"",
		)
	}
}
