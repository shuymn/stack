package improve

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Outcomes recorded in the stamp.
const (
	outStarted        = "started"
	outNoChange       = "no-change"
	outPushedNoPR     = "pushed-no-pr"
	outPublishUnknown = "publish-unknown"
	prefixPR          = "pr "
)

// stamp is the week file ~/.local/state/stack/improve/<YYYY>-W<ww> (design §3.4).
//
//nolint:tagliatelle // the stamp uses snake_case (design §3.4).
type stamp struct {
	Week        string   `json:"week"`
	Started     string   `json:"started"`
	Finished    string   `json:"finished,omitempty"`
	Repo        string   `json:"repo"`
	BaseOID     string   `json:"base_oid"`
	Outcome     string   `json:"outcome"`
	CostUSD     any      `json:"cost_usd,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Watch       string   `json:"watch,omitempty"`
	OID         string   `json:"oid,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Title       string   `json:"title,omitempty"`
	Body        string   `json:"body,omitempty"`
	PR          string   `json:"pr,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Patch       string   `json:"patch,omitempty"`
	CheckHits   string   `json:"check_hits,omitempty"`
	CodexHigh   []string `json:"codex_high,omitempty"`
	CodexRuns   int      `json:"codex_runs,omitempty"`
}

// weekName is the ISO week of t's date, e.g. 2026-W47.
func weekName(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// redacted returns s with every string passed through red.
func (s stamp) redacted(red func(string) string) stamp {
	for _, p := range []*string{
		&s.Week, &s.Started, &s.Finished, &s.Repo, &s.BaseOID, &s.Outcome, &s.Summary, &s.Watch,
		&s.OID, &s.Branch, &s.Title, &s.Body, &s.PR, &s.Reason, &s.Patch, &s.CheckHits,
	} {
		*p = red(*p)
	}
	s.EvidenceIDs = redactAll(s.EvidenceIDs, red)
	s.CodexHigh = redactAll(s.CodexHigh, red)
	return s
}

func redactAll(in []string, red func(string) string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = red(v)
	}
	return out
}

func encodeStamp(s stamp) ([]byte, error) {
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return nil, fmt.Errorf("encode stamp: %w", err)
	}
	return append(data, '\n'), nil
}

// claimStamp creates the stamp with O_CREATE|O_EXCL (0600).
func claimStamp(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create stamp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write stamp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync stamp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close stamp: %w", err)
	}
	return nil
}

// replaceFile replaces path with data through a 0600 temp file in the same directory and a
// rename, so a reader sees the old stamp or the new one.
func replaceFile(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".stamp-*")
	if err != nil {
		return fmt.Errorf("create temp stamp: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp stamp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temp stamp: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp stamp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp stamp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace stamp: %w", err)
	}
	return nil
}

var errStampUnreadable = errors.New("stamp unreadable")

// readStamp reads an existing stamp. An empty or unparsable file is errStampUnreadable.
func readStamp(path string) (stamp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return stamp{}, fmt.Errorf("%w: %w", errStampUnreadable, err)
	}
	var s stamp
	if len(strings.TrimSpace(string(data))) == 0 {
		return s, fmt.Errorf("%w: empty", errStampUnreadable)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		// Keep whatever fields decode, for the report.
		var loose map[string]any
		if json.Unmarshal(data, &loose) == nil {
			s = looseStamp(loose)
		}
		return s, fmt.Errorf("%w: not a stamp", errStampUnreadable)
	}
	return s, nil
}

func looseStamp(m map[string]any) stamp {
	str := func(k string) string {
		v, _ := m[k].(string)
		return v
	}
	return stamp{
		Outcome: str("outcome"), OID: str("oid"), Branch: str("branch"),
		Title: str("title"), Body: str("body"),
	}
}

// finalWarn reports whether a final outcome needs the owner (design §3.4 step 5).
func finalWarn(s stamp) bool {
	switch {
	case s.Outcome == outNoChange, strings.HasPrefix(s.Outcome, prefixPR):
		return s.CostUSD == unknownCost
	default:
		return true
	}
}

// recorded lists the publication fields a stamp holds, for "attempt outcome unknown".
func recorded(s stamp) string {
	var parts []string
	for _, f := range []struct{ name, v string }{
		{"oid", s.OID}, {"branch", s.Branch}, {"title", s.Title}, {"body", s.Body},
	} {
		if f.v != "" {
			v := f.v
			if f.name == "body" {
				v = fmt.Sprintf("%d bytes", len(v))
			}
			parts = append(parts, f.name+" "+v)
		}
	}
	if len(parts) == 0 {
		return "no oid, branch, title or body recorded"
	}
	return strings.Join(parts, "; ")
}
