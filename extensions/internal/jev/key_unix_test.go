//go:build unix

package jev

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestKeyFileFIFO(t *testing.T) {
	home := t.TempDir()
	p := KeyPath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		key string
		err error
	}
	done := make(chan result, 1)
	go func() {
		key, err := Key(envOf(""), home)
		done <- result{key, err}
	}()
	select {
	case r := <-done:
		wantKeyError(t, r.key, r.err, KeyNotRegular)
	case <-time.After(5 * time.Second):
		t.Fatal("Key blocked on a FIFO")
	}
}

func TestKeyFileShared(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o602, 0o610, 0o601} {
		home := t.TempDir()
		writeKey(t, home, keySentinel, mode)
		key, err := Key(envOf(""), home)
		wantKeyError(t, key, err, KeyShared)
	}
}

func TestKeyFileNotOwned(t *testing.T) {
	home := t.TempDir()
	writeKey(t, home, keySentinel, 0o600)
	uid := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { currentUID = uid })
	key, err := Key(envOf(""), home)
	wantKeyError(t, key, err, KeyNotOwned)
}

func TestKeyFileUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	home := t.TempDir()
	writeKey(t, home, keySentinel, 0o000)
	key, err := Key(envOf(""), home)
	wantKeyError(t, key, err, KeyUnreadable)
}
