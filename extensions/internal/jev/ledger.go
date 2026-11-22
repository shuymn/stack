package jev

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// errLedgerSlow reports an append that did not finish within its wait.
var errLedgerSlow = errors.New("append abandoned after its wait")

// LedgerPath is the usage ledger under home: .local/state/stack/jev/usage.jsonl.
func LedgerPath(home string) string {
	return filepath.Join(home, ".local", "state", "stack", "jev", "usage.jsonl")
}

// RunID is a random per-process ID for ledger lines.
func RunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Ledger appends one line per live attempt (§3.3). Appends are serialised; an I/O error is one
// stderr line and changes no status.
type Ledger struct {
	Path      string
	Run       string
	Extension string
	// Stderr receives the error line; nil discards it.
	Stderr io.Writer
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Wait bounds each append; zero means LedgerWait.
	Wait time.Duration

	mu    sync.Mutex
	outMu sync.Mutex
}

// ledgerLine is one line of usage.jsonl (design §3.3).
//
//nolint:tagliatelle // the ledger format of the design, in snake_case.
type ledgerLine struct {
	Run         string `json:"run"`
	Time        string `json:"time"`
	Extension   string `json:"extension"`
	Model       string `json:"model"`
	Outcome     string `json:"outcome"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	InputTokens *int64 `json:"input_tokens"`
}

// Append writes the attempt's line. It waits at most Wait (LedgerWait when zero): a ledger
// that does not answer cannot hold the response, and is reported in one stderr line.
func (l *Ledger) Append(o Outcome, elapsed time.Duration) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	line := ledgerLine{
		Run: l.Run, Time: now().UTC().Format(time.RFC3339Nano), Extension: l.Extension,
		Model: Pin, Outcome: o.String(), ElapsedMS: elapsed.Milliseconds(),
	}
	if o.Usage != nil {
		line.InputTokens = o.Usage.InputTokens
	}
	data, err := Marshal(line)
	if err != nil {
		l.fail(err)
		return
	}
	var abandoned atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.mu.Lock()
		defer l.mu.Unlock()
		if err := appendLine(l.Path, append(data, '\n')); err != nil && !abandoned.Load() {
			l.fail(err)
		}
	}()
	wait := l.Wait
	if wait <= 0 {
		wait = LedgerWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		abandoned.Store(true)
		l.fail(fmt.Errorf("%w (%v)", errLedgerSlow, wait))
	}
}

func (l *Ledger) fail(err error) {
	l.outMu.Lock()
	defer l.outMu.Unlock()
	if l.Stderr != nil {
		_, _ = fmt.Fprintf(l.Stderr, "jev: usage ledger: %v\n", err)
	}
}

func appendLine(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ledger directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("append ledger: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("close ledger: %w", cerr)
	}
	return nil
}
