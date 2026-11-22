//go:build unix

package jev

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLedgerStuck: a ledger whose file never opens (a FIFO with no reader) cannot hold the
// answer; the append is abandoned after its bounded wait with one stderr line.
func TestLedgerStuck(t *testing.T) {
	home := t.TempDir()
	path := LedgerPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Open the reading end so the abandoned append can finish.
		if f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			time.Sleep(50 * time.Millisecond)
			_ = f.Close()
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(noulBody(Pin, "0.9")))
	}))
	defer srv.Close()
	var stderr bytes.Buffer
	l := &Ledger{Path: path, Run: "r", Extension: "x", Stderr: &stderr}
	start := time.Now()
	o := Live{Endpoint: srv.URL, Key: "k", Ledger: l}.Send(t.Context(), testBatch(noulQ))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the stuck ledger held the answer for %v", elapsed)
	}
	if o.Class != OK || o.Answers["q"].Noul != 0.9 {
		t.Fatalf("outcome %+v", o)
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 ||
		!strings.HasPrefix(got, "jev: usage ledger: ") {
		t.Fatalf("stderr %q", got)
	}
}

// TestLedgerAbandonedWaiter: an append abandoned while it waits for the ledger's mutex drops
// its line instead of writing it late.
func TestLedgerAbandonedWaiter(t *testing.T) {
	home := t.TempDir()
	path := LedgerPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	l := &Ledger{Path: path, Run: "r", Extension: "x", Stderr: &stderr}
	l.Append(Outcome{Class: OK}, 0) // holds the mutex, blocked opening the FIFO
	l.Append(Outcome{Class: OK}, 0) // abandoned while waiting for the mutex
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var got []byte
	buf := make([]byte, 4096)
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
		n, _ := f.Read(buf)
		got = append(got, buf[:n]...)
		time.Sleep(10 * time.Millisecond)
	}
	if n := bytes.Count(got, []byte("\n")); n != 1 {
		t.Fatalf("%d ledger lines written, want only the first: %q", n, got)
	}
}
