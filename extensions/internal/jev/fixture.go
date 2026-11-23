package jev

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Fixture is a `--replay` file of fixtures/jev (§3.4). Core reads only request and expect.
type Fixture struct {
	// Placeholder, when set, marks a cal case awaiting its first live recording (PendingPrefix).
	Placeholder string `json:"placeholder,omitempty"`
	// Synthetic, when set, says why a fixed fixture's records are written by hand.
	Synthetic string          `json:"synthetic,omitempty"`
	Request   json.RawMessage `json:"request"`
	Expect    *Expect         `json:"expect,omitempty"`
	Want      *Want           `json:"want,omitempty"`
	Repo      json.RawMessage `json:"repo,omitempty"`
	Label     json.RawMessage `json:"label,omitempty"`
	// Observe, Files and Memory are classify's disk inputs (§3.4): the observe file's content,
	// files by path relative to the cwd (a string, or an array of transcript records written one
	// per line) and memory index text by directory, written where classify's lookup places it.
	Observe json.RawMessage            `json:"observe,omitempty"`
	Files   map[string]json.RawMessage `json:"files,omitempty"`
	Memory  map[string]string          `json:"memory,omitempty"`
	// Build names the test build a fixed classify fixture runs under when the default one (both
	// decisions enabled at the test cutoff) does not fit: "no-qualification".
	Build string `json:"build,omitempty"`
	// Samples maps a body hash to each question's three scores (nil for an unscored attempt).
	Samples map[string]map[string][]*float64 `json:"samples,omitempty"`
}

// PendingPrefix starts the placeholder of a cal fixture with no live records yet: it has no
// expect or want until jev-calibrate runs.
const PendingPrefix = "pending:"

// Pending reports a fixture awaiting its first calibration run.
func (f *Fixture) Pending() bool { return strings.HasPrefix(f.Placeholder, PendingPrefix) }

// Expect is the aggregate status core compares.
type Expect struct {
	Status string `json:"status"`
}

// Want is the extension result a fixture expects.
type Want struct {
	Findings []Finding `json:"findings"`
	Context  string    `json:"context,omitempty"`
	// Report is classify's artifact: a fixed fixture holds the matched parts (§3.5), a cal
	// fixture the whole normalised report.
	Report json.RawMessage `json:"report,omitempty"`
	// Sent lists, for classify's resolution fixtures, the exact owner and assistant texts each
	// reply is sent with.
	Sent []Sent `json:"sent,omitempty"`
}

// Sent is the owner and assistant text a reply is sent with.
type Sent struct {
	Reply     string `json:"reply"`
	Owner     string `json:"owner"`
	Assistant string `json:"assistant"`
}

// ReadFixture reads and decodes a fixture.
func ReadFixture(path string) (Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("read fixture: %w", err)
	}
	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture %s: %w", path, err)
	}
	return f, nil
}

// WriteFixture writes f as indented JSON.
func WriteFixture(path string, f Fixture) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return fmt.Errorf("encode fixture: %w", err)
	}
	// A committed fixture, world-readable like the rest of the checkout.
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil { //nolint:gosec // G306
		return fmt.Errorf("write fixture: %w", err)
	}
	return nil
}

// RequestRecords returns the replay records of a fixture's request.
func (f *Fixture) RequestRecords() (Replay, error) {
	var req struct {
		Jev json.RawMessage `json:"jev"`
	}
	if err := json.Unmarshal(f.Request, &req); err != nil {
		return Replay{}, fmt.Errorf("fixture request: %w", err)
	}
	return ParseReplay(req.Jev)
}

// SetRecords replaces the replay records of the fixture's request, keeping its other fields and
// the other keys of its jev object (classify's now, observe, claude, t3; §3.4).
func (f *Fixture) SetRecords(records map[string]Record) error {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(f.Request, &req); err != nil {
		return fmt.Errorf("fixture request: %w", err)
	}
	obj := map[string]json.RawMessage{}
	if raw := bytes.TrimSpace(req["jev"]); len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("fixture request jev: %w", err)
		}
	}
	rec, err := Marshal(records)
	if err != nil {
		return err
	}
	obj["records"] = rec
	j, err := Marshal(obj)
	if err != nil {
		return err
	}
	req["jev"] = j
	if f.Request, err = Marshal(req); err != nil {
		return err
	}
	return nil
}
