package proc_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// TestExitedProbe covers the Windows exit probe on a real child: it reports a
// gated child as running, and as exited once the child ends with status 259
// (STILL_ACTIVE), before Wait reaps it. The real kill then fails on the exited
// child, and the run reports the child's own exit. Not parallel: it swaps the
// runner's kill hook.
func TestExitedProbe(t *testing.T) {
	const stillActive = 259
	gate := filepath.Join(t.TempDir(), "gate")
	fake := proctest.Install(t, "fake", one(proctest.Response{
		Stdout: "out\n", Exit: stillActive, Gate: gate,
	}))
	var probed atomic.Bool
	proc.SetKillHooks(t, func(p *os.Process) error {
		probed.Store(true)
		if proc.Exited(p) {
			t.Error("probe reports the gated child as exited")
		}
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Error(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for !proc.Exited(p) {
			if time.Now().After(deadline) {
				t.Error("probe did not report the released child as exited")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return p.Kill()
	}, proc.Exited)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	res, err := proc.Exec{WaitDelay: killWaitDelay}.Run(ctx, proc.Cmd{Name: fake})
	if !probed.Load() {
		t.Fatal("kill hook did not run")
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "out\n" || res.ExitCode != stillActive || res.TimedOut {
		t.Fatalf("result = %+v, want the child's own exit %d with its stdout", res, stillActive)
	}
}
