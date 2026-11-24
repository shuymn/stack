package proc_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc"
)

// TestCancelKillExiting covers a cancel whose kill fails while the child may be
// exiting (issue #103): TerminateProcess denies access to a terminating child
// before its handle is signaled, so the runner waits up to ExitGrace for the
// exit after an access denied, and only then. The wait hook scripts the
// handle: the immediate probe always sees the child running, and the gated
// child is released where the case says, so neither the probe nor a released
// handle can confirm the exit early. Not parallel: it swaps the runner's hooks.
func TestCancelKillExiting(t *testing.T) {
	accessDenied := &os.SyscallError{Syscall: "TerminateProcess", Err: syscall.ERROR_ACCESS_DENIED}
	graceMillis := uint32(proc.ExitGrace / time.Millisecond)
	startMillis := uint32(startBound / time.Millisecond)
	runner := proc.Exec{WaitDelay: killWaitDelay}
	for _, tc := range []struct {
		name    string
		killErr error
		// exits releases the child during the grace wait; otherwise the grace
		// wait runs on the gated child and releases it after.
		exits bool
		// waits are the timeouts the runner waits on the handle with.
		waits []uint32
	}{
		{
			name: "exits within the grace", killErr: accessDenied, exits: true,
			waits: []uint32{0, graceMillis},
		},
		{name: "running past the grace", killErr: accessDenied, waits: []uint32{0, graceMillis}},
		{name: "other kill error", killErr: errKill, waits: []uint32{0}},
	} {
		// hook installs the hooks for the fake's gate and records kills and waits.
		hook := func(t *testing.T, gate string) (*atomic.Int32, func() []uint32) {
			t.Helper()
			var kills atomic.Int32
			proc.SetKillHooks(t, func(*os.Process) error {
				kills.Add(1)
				return tc.killErr
			}, nil)
			release := func() {
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Error(err)
				}
			}
			var mu sync.Mutex
			var waits []uint32
			proc.SetWaitHook(t, func(h syscall.Handle, millis uint32) (uint32, error) {
				mu.Lock()
				waits = append(waits, millis)
				mu.Unlock()
				switch {
				case millis == 0:
					if len(tc.waits) == 1 {
						release()
					}
					return syscall.WAIT_TIMEOUT, nil
				case tc.exits:
					release()
					return syscall.WaitForSingleObject(h, startMillis)
				default:
					defer release()
					return syscall.WaitForSingleObject(h, millis)
				}
			})
			return &kills, func() []uint32 {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(waits)
			}
		}
		check := func(t *testing.T, kills *atomic.Int32, waits []uint32, err error) {
			t.Helper()
			if n := kills.Load(); n != 1 {
				t.Fatalf("kill hook ran %d times, want 1", n)
			}
			if !slices.Equal(waits, tc.waits) {
				t.Fatalf("waited on the handle with %v ms, want %v ms", waits, tc.waits)
			}
			if tc.exits && err != nil {
				t.Fatal(err)
			}
			if !tc.exits && !errors.Is(err, tc.killErr) {
				t.Fatalf("err = %v, want the kill error %v", err, tc.killErr)
			}
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Run("run", func(t *testing.T) {
				c, gate, started := gatedFake(t)
				kills, waits := hook(t, gate)
				res, err := runner.Run(cancelOnStart(t, started), c)
				check(t, kills, waits(), err)
				if tc.exits &&
					(string(res.Stdout) != "out\n" || res.ExitCode != 0 || res.TimedOut) {
					t.Fatalf("result = %+v, want the child's own exit 0 with its stdout", res)
				}
			})
			t.Run("stream", func(t *testing.T) {
				c, gate, started := gatedFake(t)
				kills, waits := hook(t, gate)
				var got []byte
				err := runner.Stream(cancelOnStart(t, started), c, func(r io.Reader) error {
					var err error
					if got, err = io.ReadAll(r); err != nil {
						return fmt.Errorf("read stdout: %w", err)
					}
					return nil
				})
				check(t, kills, waits(), err)
				if tc.exits && string(got) != "out\n" {
					t.Fatalf("stdout = %q", got)
				}
			})
		})
	}
}
