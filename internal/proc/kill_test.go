package proc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

var errKill = errors.New("injected kill failure")

const (
	// killWaitDelay is far above a gated exit, so os/exec never retries the kill
	// with the real one.
	killWaitDelay = 10 * time.Second
	// startBound bounds a fake's start, far above a slow one under -race.
	startBound = 30 * time.Second
)

// gatedFake installs a fake that writes started once running, then writes
// "out\n" and holds its exit 0 until gate exists. Its env turns off the race
// detector's exit sleep.
func gatedFake(t *testing.T) (proc.Cmd, string, string) {
	t.Helper()
	dir := t.TempDir()
	gate, started := filepath.Join(dir, "gate"), filepath.Join(dir, "started")
	fake := proctest.Install(t, "fake", one(proctest.Response{
		Stdout: "out\n", Gate: gate, WriteFiles: map[string]string{started: ""},
	}))
	c := proc.Cmd{Name: fake, Env: proc.QuietExitEnv()}
	return c, gate, started
}

// cancelOnStart returns a context canceled once started exists, so the child
// is running when the runner kills it; after startBound it is canceled with a
// test error.
func cancelOnStart(t *testing.T, started string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		defer cancel()
		deadline := time.Now().Add(startBound)
		for {
			if _, err := os.Stat(started); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("fake did not start within %s", startBound)
				return
			}
			select {
			case <-ctx.Done(): // The test ended first, e.g. the fake failed to start.
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return ctx
}

// TestCancelKillFails covers a cancel whose kill fails: when the child had
// already exited the run reports the child's own exit, otherwise the kill
// error. The kill hook fails without killing and opens the child's gate, so
// the child is running when the kill is tried and then exits 0 with its
// output. Not parallel: it swaps the runner's kill hooks.
func TestCancelKillFails(t *testing.T) {
	runner := proc.Exec{WaitDelay: killWaitDelay}
	for _, tc := range []struct {
		name   string
		exited bool
	}{
		{name: "child exited", exited: true},
		{name: "child running", exited: false},
	} {
		// hook installs the kill hooks for the fake's gate and counts kills.
		hook := func(t *testing.T, gate string) *atomic.Int32 {
			t.Helper()
			var kills atomic.Int32
			proc.SetKillHooks(t, func(*os.Process) error {
				kills.Add(1)
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Error(err)
				}
				return errKill
			}, func(*os.Process) bool { return tc.exited })
			return &kills
		}
		check := func(t *testing.T, kills *atomic.Int32, err error) {
			t.Helper()
			if n := kills.Load(); n != 1 {
				t.Fatalf("kill hook ran %d times, want 1", n)
			}
			if tc.exited && err != nil {
				t.Fatal(err)
			}
			if !tc.exited && !errors.Is(err, errKill) {
				t.Fatalf("err = %v, want the kill error", err)
			}
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Run("run", func(t *testing.T) {
				c, gate, started := gatedFake(t)
				kills := hook(t, gate)
				res, err := runner.Run(cancelOnStart(t, started), c)
				check(t, kills, err)
				if tc.exited &&
					(string(res.Stdout) != "out\n" || res.ExitCode != 0 || res.TimedOut) {
					t.Fatalf("result = %+v, want the child's own exit 0 with its stdout", res)
				}
			})
			t.Run("stream", func(t *testing.T) {
				c, gate, started := gatedFake(t)
				kills := hook(t, gate)
				var got []byte
				err := runner.Stream(cancelOnStart(t, started), c, func(r io.Reader) error {
					var err error
					if got, err = io.ReadAll(r); err != nil {
						return fmt.Errorf("read stdout: %w", err)
					}
					return nil
				})
				check(t, kills, err)
				if tc.exited && string(got) != "out\n" {
					t.Fatalf("stdout = %q", got)
				}
			})
		})
	}
}
