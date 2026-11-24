package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

// TestNativeKillAfterExit covers the Windows path of issue #103 on a real
// child, driving start and finish so that nothing calls Wait, and so releases
// the process handle, before the kills. The gated child is probed as running,
// then released and waited for on its own handle, probed as exited, and only
// then canceled: the real kill fails with access denied, Cancel confirms the
// exit, and os/exec retries the kill WaitDelay later, which fails the same way.
// finish runs after that retry. Exit 0 must come back without an error; exit
// 259 (STILL_ACTIVE) must be probed as exited and come back as that status.
// Not parallel: it swaps the runner's kill hook.
func TestNativeKillAfterExit(t *testing.T) {
	const (
		retryDelay = 200 * time.Millisecond
		// retrySettle is the margin past retryDelay for os/exec's retry to run
		// before finish's Wait takes the watcher's result. If a loaded runner
		// misses it, Wait takes the result before the retry, and the run still
		// passes.
		retrySettle = time.Second
		bound       = 30 * time.Second
		boundMillis = uint32(bound / time.Millisecond)
		// pipeBound bounds finish's wait for the pipes, apart from the WaitDelay
		// that times the retry.
		pipeBound = 10 * time.Second
	)
	for _, code := range []int{0, 259} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "gate")
			fake := proctest.Install(t, "fake", proctest.Scenario{Responses: []proctest.Response{{
				Stdout: "out\n", Exit: code, Gate: gate,
			}}})
			kills := make(chan error, 1)
			SetKillHooks(t, func(p *os.Process) error {
				err := p.Kill()
				select {
				case kills <- err:
				default:
				}
				if err != nil {
					return fmt.Errorf("native kill: %w", err)
				}
				return nil
			}, exited)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := Exec{WaitDelay: retryDelay}.start(ctx, Cmd{
				Name: fake, Env: QuietExitEnv(),
			}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			finished := false
			defer func() {
				if !finished {
					_ = os.WriteFile(gate, nil, 0o600)
					_ = ch.cmd.Process.Kill()
					_, _ = ch.finish(pipeBound)
				}
			}()
			p := ch.cmd.Process

			if exited(p) {
				t.Fatal("probe reports the gated child as exited")
			}
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var event uint32
			var waitErr error
			if err := p.WithHandle(func(h uintptr) {
				event, waitErr = syscall.WaitForSingleObject(syscall.Handle(h), boundMillis)
			}); err != nil || waitErr != nil || event != syscall.WAIT_OBJECT_0 {
				t.Fatalf(
					"wait for the child: event %d, handle error %v, wait error %v",
					event,
					err,
					waitErr,
				)
			}
			if !exited(p) {
				t.Fatal("probe reports the terminated child as running before Wait")
			}
			out, err := io.ReadAll(ch.stdout)
			if err != nil || string(out) != "out\n" {
				t.Fatalf("stdout = %q, err = %v", out, err)
			}

			cancel()
			var first error
			select {
			case first = <-kills:
			case <-time.After(bound):
				t.Fatal("the cancel did not call the kill hook")
			}
			time.Sleep(retryDelay + retrySettle)
			overrun, err := ch.finish(pipeBound)
			finished = true

			var native *os.SyscallError
			if !errors.As(first, &native) || native.Syscall != "TerminateProcess" ||
				!errors.Is(first, syscall.ERROR_ACCESS_DENIED) {
				t.Fatalf("first kill = %v, want TerminateProcess access denied", first)
			}
			if overrun || ch.killed.Load() || !ch.exitConfirmed.Load() {
				t.Fatalf("overrun %v, killed %v, exit confirmed %v; want only the exit confirmed",
					overrun, ch.killed.Load(), ch.exitConfirmed.Load())
			}
			if code == 0 {
				if err != nil {
					t.Fatalf("finish = %v, want the child's own exit 0", err)
				}
				return
			}
			if exitErr, ok := errors.AsType[*exec.ExitError](
				err,
			); !ok ||
				exitErr.ExitCode() != code {
				t.Fatalf("finish = %v, want exit status %d", err, code)
			}
		})
	}
}
