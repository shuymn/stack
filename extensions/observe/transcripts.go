package observe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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

// lineBuffer is the transcript read buffer; longer lines are assembled up to maxLine.
const lineBuffer = mib

// maxLine bounds one transcript line, like the Codex first line: a longer line is skipped and
// counted, so one huge record cannot hold the run's memory.
const maxLine = 16 * mib

const mib = 1 << 20

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

// toolCall is one in-window call of a guard-wired tool, with its recorded decision. It keeps no
// copy of the input: digest identifies the payload, and the replay re-reads the line at offset
// for the one call of each payload it runs.
type toolCall struct {
	path      string
	line      int
	offset    int64
	id        string
	sessionID string
	cwd       string
	tool      string
	ts        time.Time
	recorded  string
	// inputOK: the input holds what the guard reads for the tool (guardInput).
	inputOK bool
	digest  [sha256.Size]byte
}

var errNotDir = errors.New("not a directory")

// walkRoot resolves a source root: absent is errOff; a symlinked root (a dotfiles layout) is
// followed; anything but a directory fails the source.
func walkRoot(root string) (string, error) {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return "", errOff
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", errors.New("root unreadable") //nolint:err113 // a fixed per-cause summary
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root is %w", errNotDir)
	}
	return root, nil
}

// transcripts reads ~/.claude/projects/*/*.jsonl and …/subagents/**/*.jsonl whose mtime is at or
// after since (observe.py:187-201).
func (s *state) transcripts(st *Source) error {
	root, err := walkRoot(filepath.Join(s.o.Home, ".claude", "projects"))
	if err != nil {
		return fmt.Errorf("transcripts %w", err)
	}
	// Links below the root are not followed.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if werr := s.work.Err(); werr != nil {
			return fmt.Errorf("transcripts: %w", werr)
		}
		if err != nil {
			if path == root {
				return errors.New("transcripts root unreadable") //nolint:err113 // fixed summary
			}
			st.unreadable = append(st.unreadable, unreadable(root, path, err))
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
			st.unreadable = append(st.unreadable, unreadable(root, path, err))
			return nil
		}
		if !info.Mode().IsRegular() {
			st.unreadable = append(st.unreadable, unreadable(root, path, errNotRegular))
			return nil
		}
		if info.ModTime().Before(s.since) {
			return nil
		}
		st.Files++
		if err := s.scanTranscript(path, subagent, st); err != nil {
			if isDeadline(err) {
				return err
			}
			st.unreadable = append(st.unreadable, unreadable(root, path, err))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk transcripts: %w", err)
	}
	if len(st.unreadable) == 0 {
		// Part of the step: a deadline here makes transcripts timeout, never ok with the signal
		// missing (reference-unread needs every transcript read, design §3.3).
		if err := s.referenceUnread(); err != nil {
			return err
		}
	}
	return nil
}

func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

var errNotRegular = errors.New("not a regular file")

// unreadable names a file that could not be read by its location under the source root, so no
// local path reaches latest.json or the context, and by the cause without the path in it.
func unreadable(root, path string, err error) string {
	rel, rerr := filepath.Rel(root, path)
	if rerr != nil {
		rel = filepath.Base(path)
	}
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		err = pe.Err
	}
	return filepath.ToSlash(rel) + ": " + err.Error()
}

// completeLines calls fn with each newline-terminated line of r, its 1-based number and its byte
// offset, until fn returns false. An unterminated last line is still being written and is left
// out; a line longer than maxLine is skipped and counted in skipped. The line is valid only during
// the call. The work deadline is checked before every line.
func (s *state) completeLines(
	r io.Reader,
	fn func(n int, off int64, line []byte) bool,
) (int, error) {
	skipped := 0
	br := bufio.NewReaderSize(r, lineBuffer)
	var acc []byte
	var off, start int64
	n, long := 0, false
	for {
		if werr := s.work.Err(); werr != nil {
			return skipped, fmt.Errorf("read: %w", werr)
		}
		chunk, err := br.ReadSlice('\n')
		off += int64(len(chunk))
		if errors.Is(err, bufio.ErrBufferFull) {
			if !long && len(acc)+len(chunk) > maxLine {
				long, acc = true, acc[:0]
			}
			if !long {
				acc = append(acc, chunk...)
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return skipped, nil
		}
		if err != nil {
			return skipped, fmt.Errorf("read: %w", err)
		}
		n++
		line := chunk
		if len(acc) > 0 {
			acc = append(acc, chunk...)
			line = acc
		}
		switch {
		case long || len(line)-1 > maxLine:
			skipped++
		case !fn(n, start, line[:len(line)-1]):
			return skipped, nil
		}
		acc, long, start = acc[:0], false, off
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
	// T3 is decided from the first lines before any line is read: a first pass over them, then
	// the file from the start, so no line is held.
	_, err = s.completeLines(f, func(n int, _ int64, line []byte) bool {
		sc.t3 = sc.t3 || reT3Entry.Match(line)
		return n < t3Scan && !sc.t3
	})
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	skipped, err := s.completeLines(f, func(n int, off int64, line []byte) bool {
		sc.line(n, off, line)
		return true
	})
	st.skipped += skipped
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
	// metaCount counts the meta tool calls; metaFirst is the first, metaText its excerpt.
	metaCount        int
	metaText         string
	metaFirst, hiden *toolCall
	pending          map[string]*toolCall
}

func (sc *scanner) line(n int, off int64, line []byte) {
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
		return // a malformed line is skipped, not a failure of the source
	}
	t, ok := parseTime(r.Timestamp)
	in := ok && sc.s.inWindow(t)
	if in {
		sc.st.Records++
	}
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
				sc.toolUse(n, off, r, b, t)
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

func (sc *scanner) toolUse(n int, off int64, r record, b block, t time.Time) {
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
			sc.metaFirst = &toolCall{line: n, ts: t}
			sc.metaText = cut(b.Name+" "+blob, excerptLen)
		}
		sc.metaCount++
	}
	if sc.subagent && sc.hiden == nil && ((b.Name == "Skill" && args["skill"] == "hiden") ||
		(b.Name == "Read" && strings.Contains(blob, "skills/hiden/"))) {
		sc.hiden = &toolCall{line: n, tool: cut(b.Name+" "+blob, excerptLen), ts: t}
	}
	if wiredTools[b.Name] && b.ID != "" {
		s := sc.s
		call := &toolCall{
			path: sc.path, line: n, offset: off, id: b.ID, sessionID: s.intern(r.SessionID),
			cwd: s.intern(r.CWD), tool: s.intern(b.Name), ts: t, recorded: decUnknown,
			inputOK: guardInput(b.Name, b.Input),
			digest:  payloadDigest(r.CWD, b.Name, b.Input),
		}
		sc.pending[b.ID] = call
		s.calls = append(s.calls, call)
	}
}

// intern returns one shared copy of v, so thousands of calls in one cwd keep one string.
func (s *state) intern(v string) string {
	if w, ok := s.strs[v]; ok {
		return w
	}
	s.strs[v] = v
	return v
}

// payloadDigest identifies a replay payload: identical (cwd, tool, compact input) run once.
func payloadDigest(cwd, tool string, input json.RawMessage) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(cwd))
	h.Write([]byte{0})
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(compact(input))
	var d [sha256.Size]byte
	h.Sum(d[:0])
	return d
}

func compact(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return raw
	}
	return buf.Bytes()
}

// guardInput reports whether input holds what the guard reads for tool. The guard decodes the
// same fields the same way and returns no decision, without a diagnostic, when it cannot or when
// the field it judges is empty (internal/guard guard.go Decide, secrets.go secretsFileTool,
// pencil.go pencil): such a replay says nothing about the recorded decision.
func guardInput(tool string, raw json.RawMessage) bool {
	if !opens(raw, '{') {
		return false
	}
	switch tool {
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		return json.Unmarshal(raw, &in) == nil && in.Command != ""
	case "Read", "NotebookRead", "Grep", "Glob":
		//nolint:tagliatelle // Claude Code defines the tool input keys in snake_case.
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
			Path         string `json:"path"`
			Glob         string `json:"glob"`
			Pattern      string `json:"pattern"`
		}
		if json.Unmarshal(raw, &in) != nil {
			return false
		}
		switch tool {
		case "Read":
			return in.FilePath != ""
		case "NotebookRead":
			return in.NotebookPath != "" || in.FilePath != ""
		case "Glob":
			return in.Pattern != ""
		}
		return true // Grep: path and glob are optional; the guard judges what is there
	case "mcp__pencil__execute":
		var in struct {
			Input string `json:"input"`
			Edits []struct {
				Find    string `json:"find"`
				Replace string `json:"replace"`
			} `json:"edits"`
		}
		return json.Unmarshal(raw, &in) == nil && (in.Input != "" || len(in.Edits) > 0)
	}
	return false
}

// finish adds the per-transcript items and the cwd weight.
func (sc *scanner) finish() {
	if sc.metaFirst != nil {
		sc.s.add(
			"meta-effort",
			fmt.Sprintf("%s:%d", sc.path, sc.metaFirst.line),
			fmt.Sprintf("%d meta tool calls; first: %s", sc.metaCount, sc.metaText),
			sc.metaFirst.ts,
			"synthetic",
			"",
		)
	}
	if sc.hiden != nil {
		sc.s.add("hiden-in-worker", fmt.Sprintf("%s:%d", sc.path, sc.hiden.line), sc.hiden.tool,
			sc.hiden.ts, "synthetic", "")
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
// meta, a summary, a tool result or a command (observe.py:117-122). A null or missing content is
// no input.
func typedText(r record, content json.RawMessage) (string, bool) {
	var text string
	if !opens(content, '"') || json.Unmarshal(content, &text) != nil {
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
	if t, ok := parseTime(r.Timestamp); ok && s.inWindow(t) {
		st.Records++
	}
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
		if text, ok := resultText(b.Content); ok {
			call.recorded = recordedDecision(call.tool, text, r)
		} // else a missing or malformed result stays unknown
	}
}

// opens reports whether the JSON value raw starts with c: '"' a string, '{' an object, '[' a
// list. Missing and null values open with none of them.
func opens(raw []byte, c byte) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == c
}

// resultText is a tool_result content: a string, or the text blocks of a list joined; false when
// the content is missing, null or neither.
func resultText(raw json.RawMessage) (string, bool) {
	var text string
	if opens(raw, '"') {
		return text, json.Unmarshal(raw, &text) == nil
	}
	if !opens(raw, '[') {
		return "", false
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n"), true
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
	if !ok || !reRuleID.MatchString(rule) {
		return decOtherDeny
	}
	return denyPrefix + rule
}

// reRuleID is the shape of the guard's rule ids (internal/guard Rule*); anything else in the
// parentheses is not a stack guard rule and is never copied into a finding.
var reRuleID = regexp.MustCompile(`^[a-z0-9-]+$`)

// referenceUnread reports each live hiden reference no in-window Read named (observe.py:204-207).
func (s *state) referenceUnread() error {
	if err := s.work.Err(); err != nil {
		return fmt.Errorf("reference-unread: %w", err)
	}
	dir := filepath.Join(s.o.Home, ".claude", "skills", "hiden", "references")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil //nolint:nilerr // no live references: nothing can be unread (legacy behaviour)
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
	return nil
}
