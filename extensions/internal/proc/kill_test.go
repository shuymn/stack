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

	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

var errKill = errors.New("injected kill failure")

// killWaitDelay is far above a gated exit, so os/exec never retries the kill
// with the real one.
const killWaitDelay = 10 * time.Second

// TestDeadlineKillFails covers a deadline kill that fails: when the child had
// already exited the run reports the child's own exit, otherwise the kill
// error. The kill hook fails without killing and opens the child's gate, so
// the child is still running when the kill is tried and then exits 0 with its
// output. Not parallel: it swaps the runner's kill hooks.
func TestDeadlineKillFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exited bool
	}{
		{name: "child exited", exited: true},
		{name: "child running", exited: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "gate")
			fake := proctest.Install(t, "fake", one(proctest.Response{Stdout: "out\n", Gate: gate}))
			var kills atomic.Int32
			proc.SetKillHooks(t, func(*os.Process) error {
				kills.Add(1)
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Error(err)
				}
				return errKill
			}, func(*os.Process) bool { return tc.exited })
			runner := proc.Exec{WaitDelay: killWaitDelay}
			deadline := func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			}

			t.Run("run", func(t *testing.T) {
				kills.Store(0)
				res, err := runner.Run(deadline(t), proc.Cmd{Name: fake})
				if kills.Load() != 1 {
					t.Fatalf("kill hook ran %d times, want 1", kills.Load())
				}
				if !tc.exited {
					if !errors.Is(err, errKill) {
						t.Fatalf("err = %v, want the kill error", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if string(res.Stdout) != "out\n" || res.ExitCode != 0 || res.TimedOut {
					t.Fatalf("result = %+v, want the child's own exit 0 with its stdout", res)
				}
			})
			t.Run("stream", func(t *testing.T) {
				kills.Store(0)
				_ = os.Remove(gate)
				var got []byte
				err := runner.Stream(deadline(t), proc.Cmd{Name: fake}, func(r io.Reader) error {
					var err error
					if got, err = io.ReadAll(r); err != nil {
						return fmt.Errorf("read stdout: %w", err)
					}
					return nil
				})
				if kills.Load() != 1 {
					t.Fatalf("kill hook ran %d times, want 1", kills.Load())
				}
				if !tc.exited {
					if !errors.Is(err, errKill) {
						t.Fatalf("err = %v, want the kill error", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != "out\n" {
					t.Fatalf("stdout = %q", got)
				}
			})
		})
	}
}
