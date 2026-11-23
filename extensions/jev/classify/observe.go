package classify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// observeVersion is the latest.json schema classify reads.
const observeVersion = 1

// Freshness bounds of the observe file (§3.2 Input).
const (
	staleAfter  = 24 * time.Hour
	futureAfter = 5 * time.Minute
)

// excerptLen is observe's excerpt bound, in characters.
const excerptLen = 160

// signalOwnerReply is the observe signal classify labels.
const signalOwnerReply = "owner-reply"

// ObserveItem is one item of observe's latest.json.
type ObserveItem struct {
	ID      string `json:"id"`
	Signal  string `json:"signal"`
	Where   string `json:"where"`
	Excerpt string `json:"excerpt"`
	TS      string `json:"ts"`
}

// ObserveSource is one source's status in latest.json.
type ObserveSource struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// Failed reports a source observe could not read completely.
func (s *ObserveSource) Failed() bool {
	return s != nil && (s.Status == "fail" || s.Status == "timeout")
}

// Window is observe's window.
type Window struct {
	Since string `json:"since"`
	Until string `json:"until"`
}

// Observe is the part of observe's latest.json classify reads.
type Observe struct {
	Version int                       `json:"version"`
	Window  Window                    `json:"window"`
	Sources map[string]*ObserveSource `json:"sources"`
	Items   []ObserveItem             `json:"items"`

	since, until time.Time
}

// observeError is an observe file classify cannot use: missing or stale ones pass, the rest
// warn.
type observeError struct {
	warn   bool
	detail string
	line   string
}

func (e *observeError) Error() string { return e.detail }

// observePath is .local/state/stack/observe/latest.json under home.
func observePath(home string) string {
	return filepath.Join(home, ".local", "state", "stack", "observe", "latest.json")
}

// readObserve reads and checks the observe file against the clock (§3.2 Input).
func readObserve(path string, now time.Time) (*Observe, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &observeError{
			detail: "no observe file",
			line:   "no observe file; run observe first",
		}
	}
	if err != nil {
		return nil, &observeError{
			warn: true, detail: "observe file unreadable",
			line: "the observe file cannot be read",
		}
	}
	var o Observe
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, &observeError{
			warn: true, detail: "observe file unparsable",
			line: "the observe file is not valid JSON",
		}
	}
	if o.Version != observeVersion {
		return nil, &observeError{
			warn: true, detail: fmt.Sprintf("observe file version %d", o.Version),
			line: fmt.Sprintf("the observe file has version %d, not %d; update stack-jev-classify",
				o.Version, observeVersion),
		}
	}
	var ok1, ok2 bool
	o.since, ok1 = parseTime(o.Window.Since)
	o.until, ok2 = parseTime(o.Window.Until)
	if !ok1 || !ok2 {
		return nil, &observeError{
			warn: true, detail: "observe file window unparsable",
			line: "the observe file's window is not valid",
		}
	}
	switch {
	case o.until.After(now.Add(futureAfter)):
		return nil, &observeError{
			warn: true, detail: "observe window in the future",
			line: "the observe file's window ends after the clock; check the clock",
		}
	case o.until.Before(now.Add(-staleAfter)):
		return nil, &observeError{
			detail: "observe file stale",
			line:   "the observe file is more than 24 h old; run observe first",
		}
	}
	return &o, nil
}

// parseTime reads an ISO 8601 timestamp as observe does; one without a zone is UTC.
func parseTime(v string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999", v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// iso is observe's item timestamp: UTC, truncated to the second.
func iso(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// excerpt cuts a reply as observe cuts it: whitespace collapsed, at most 160 characters with an
// ellipsis marking a cut.
func excerpt(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(s) <= excerptLen {
		return s
	}
	r := []rune(s)
	return string(r[:excerptLen-1]) + "…"
}
