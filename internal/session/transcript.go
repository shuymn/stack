package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

var (
	// errNoPrompt reports a transcript without a user prompt.
	errNoPrompt = errors.New("no user prompt in transcript")
	// errContent reports a user record whose message content is neither a string nor an array.
	errContent = errors.New("user message content is neither a string nor an array")
)

// record holds the transcript fields the first-prompt selection reads.
type record struct {
	Type             string `json:"type"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsSidechain      bool   `json:"isSidechain"`
	Message          struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// FirstPrompt returns the first user prompt of the transcript JSONL at path
// (`orchestration-context.py:258-273`). It skips records that are not `type: "user"` or are meta,
// compaction summaries or sidechain records. A string content is the prompt; in a block array the
// text blocks joined by blank lines are; an array without text but with a tool_result block is
// skipped, and any other array yields "". Lines have no length limit and lose a trailing `\r`;
// blank lines are skipped, and a malformed line ends the read with an error.
func FirstPrompt(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	return firstPrompt(bufio.NewReader(f))
}

func firstPrompt(r *bufio.Reader) (string, error) {
	for n := 1; ; n++ {
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read transcript: %w", err)
		}
		if s := strings.TrimSpace(line); s != "" {
			prompt, ok, perr := promptOf(s)
			if perr != nil {
				return "", fmt.Errorf("transcript line %d: %w", n, perr)
			}
			if ok {
				return prompt, nil
			}
		}
		if err != nil {
			return "", errNoPrompt
		}
	}
}

// promptOf returns the prompt of one record, or ok=false when the record is skipped.
func promptOf(line string) (string, bool, error) {
	var rec record
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return "", false, fmt.Errorf("decode: %w", err)
	}
	if rec.Type != "user" || rec.IsMeta || rec.IsCompactSummary || rec.IsSidechain {
		return "", false, nil
	}
	content := rec.Message.Content
	if len(content) == 0 || string(content) == "null" { // json.Unmarshal accepts null into a string
		return "", false, errContent
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, true, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", false, errContent
	}
	var texts []string
	toolResult := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_result":
			toolResult = true
		}
	}
	if len(texts) == 0 && toolResult {
		return "", false, nil
	}
	return strings.Join(texts, "\n\n"), true, nil
}
