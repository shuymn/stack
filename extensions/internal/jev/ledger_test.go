package jev

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLedger(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("ledger line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestLedger(t *testing.T) {
	t.Run("one serialised line per live attempt with known or null usage", func(t *testing.T) {
		home := t.TempDir()
		var n int
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			n++
			i := n
			mu.Unlock()
			switch {
			case i%3 == 0:
				w.WriteHeader(529)
			case i%2 == 0:
				_, _ = w.Write(
					[]byte(`{"model":"jev-1.14.0","answers":{"q":{"type":"noul","noul":0.5}}}`),
				)
			default:
				_, _ = w.Write([]byte(noulBody(Pin, "0.5")))
			}
		}))
		defer srv.Close()
		l := &Ledger{
			Path: LedgerPath(home), Run: "r1", Extension: "jev-test",
			Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		}
		live := Live{Endpoint: srv.URL, Key: "k", Ledger: l}
		batches := make([]Batch, 0, 12)
		for range 12 {
			batches = append(batches, testBatch(noulQ))
		}
		Do(t.Context(), live, batches)
		lines := readLedger(t, LedgerPath(home))
		if len(lines) != 12 {
			t.Fatalf("%d ledger lines, want 12", len(lines))
		}
		counts := map[string]int{}
		nulls := 0
		for _, m := range lines {
			if m["run"] != "r1" || m["extension"] != "jev-test" || m["model"] != Pin ||
				m["time"] != "2026-01-02T03:04:05Z" {
				t.Fatalf("line %v", m)
			}
			if _, ok := m["elapsed_ms"].(float64); !ok {
				t.Fatalf("elapsed_ms missing: %v", m)
			}
			outcome, _ := m["outcome"].(string)
			counts[outcome]++
			if m["input_tokens"] == nil {
				nulls++
			} else if m["input_tokens"] != float64(10) {
				t.Fatalf("input_tokens %v", m["input_tokens"])
			}
		}
		if counts["ok"] != 8 || counts["transient"] != 4 {
			t.Fatalf("outcomes %v", counts)
		}
		if nulls != 4+4 {
			t.Fatalf("%d null usages, want 8 (4 without usage, 4 failed)", nulls)
		}
		info, err := os.Stat(LedgerPath(home))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 && filepath.Separator == '/' {
			t.Fatalf("ledger mode %v", mode)
		}
	})

	t.Run("an I/O error is one stderr line and the outcome is kept", func(t *testing.T) {
		home := t.TempDir()
		// A file where the ledger directory should be.
		blocker := filepath.Join(home, ".local", "state", "stack")
		if err := os.MkdirAll(blocker, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blocker, "jev"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(noulBody(Pin, "0.9")))
		}))
		defer srv.Close()
		var stderr bytes.Buffer
		l := &Ledger{Path: LedgerPath(home), Run: "r", Extension: "x", Stderr: &stderr}
		o := Live{Endpoint: srv.URL, Key: "k", Ledger: l}.Send(t.Context(), testBatch(noulQ))
		if o.Class != OK || o.Answers["q"].Noul != 0.9 {
			t.Fatalf("outcome %+v", o)
		}
		if got := strings.Count(stderr.String(), "\n"); got != 1 ||
			!strings.HasPrefix(stderr.String(), "jev: usage ledger: ") {
			t.Fatalf("stderr %q", stderr.String())
		}
	})

	t.Run("replay writes none", func(t *testing.T) {
		home := t.TempDir()
		b := testBatch(noulQ)
		r := Replay{Records: map[string]Record{b.Hash: {Answer: json.RawMessage(noulBody(Pin,
			"0.5"))}}}
		if o := r.Send(context.Background(), b); o.Class != OK {
			t.Fatalf("outcome %+v", o)
		}
		entries, err := os.ReadDir(home)
		if err != nil || len(entries) != 0 {
			t.Fatalf("home has %v (%v)", entries, err)
		}
	})
}
