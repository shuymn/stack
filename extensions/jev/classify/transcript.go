package classify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Transcript line bounds, as observe reads them (extensions/observe/transcripts.go).
const (
	lineBuffer = 1 << 20
	maxLine    = 16 << 20
)

// typedPrefixes start a typed command or a notification, never an owner turn (observe's
// typedText).
var typedPrefixes = []string{"<command-", "<task-notification>", "<local-command"}

type tblock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type tline struct {
	Type             string          `json:"type"`
	Timestamp        string          `json:"timestamp"`
	CWD              string          `json:"cwd"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// readTranscript returns the records of lines 1..upTo of a Claude transcript, numbered as observe
// numbers them: from 1, newline-terminated, a line over 16 MiB skipped but counted, a final
// unterminated line not counted. The work deadline is checked before every line.
func readTranscript(ctx context.Context, path string, upTo int) ([]record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	var recs []record
	br := bufio.NewReaderSize(f, lineBuffer)
	var acc []byte
	long := false
	for len(recs) < upTo {
		if err := ctx.Err(); err != nil {
			return recs, fmt.Errorf("read transcript: %w", err)
		}
		chunk, err := br.ReadSlice('\n')
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
			return recs, nil
		}
		if err != nil {
			return recs, fmt.Errorf("read transcript: %w", err)
		}
		line := chunk
		if len(acc) > 0 {
			acc = append(acc, chunk...)
			line = acc
		}
		if long || len(line)-1 > maxLine {
			recs = append(recs, record{kind: recIgnored})
		} else {
			recs = append(recs, transcriptRecord(line[:len(line)-1]))
		}
		acc, long = acc[:0], false
	}
	return recs, nil
}

// transcriptRecord sorts one transcript line (§3.2 Resolution): a user record with string content
// that is not a tool result is a compaction when it is a summary (marked meta or not), else a
// prompt unless it is meta; a prompt is an owner turn when it also passes observe's typed-command
// filter and its origin is human. Any other user record is a tool result. An assistant record is text when it has text blocks, else a tool call.
func transcriptRecord(line []byte) record {
	var l tline
	if json.Unmarshal(line, &l) != nil {
		return record{kind: recIgnored}
	}
	r := record{ts: l.Timestamp, cwd: l.CWD}
	var content json.RawMessage
	if l.Message != nil {
		content = l.Message.Content
	}
	switch l.Type {
	case "user":
		var text string
		if !opens(content, '"') || json.Unmarshal(content, &text) != nil ||
			len(l.ToolUseResult) > 0 {
			r.kind = recToolResult
			return r
		}
		r.text = text
		trimmed := strings.TrimLeft(text, " \t\r\n")
		human := l.Origin != nil && l.Origin.Kind == "human"
		switch {
		// A compaction summary is a boundary even when it is also marked meta.
		case l.IsCompactSummary:
			r.kind = recCompaction
		case l.IsMeta:
			r.kind = recToolResult
		case hasAnyPrefix(trimmed, typedPrefixes) || !human:
			r.kind = recPrompt
		default:
			r.kind = recOwner
		}
	case "assistant":
		r.kind = recToolCall
		var blocks []tblock
		if json.Unmarshal(content, &blocks) != nil {
			return r
		}
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		if len(texts) > 0 {
			r.kind, r.text = recText, strings.Join(texts, "\n\n")
		}
	default:
		r.kind = recIgnored
	}
	return r
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func opens(raw []byte, c byte) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == c
}
