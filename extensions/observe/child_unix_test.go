//go:build !windows

package observe

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

// gone polls until pid no longer exists (an orphan is reaped by init shortly after it dies).
func gone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if p, err := os.FindProcess(pid); err != nil || p.Signal(syscall.Signal(0)) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestKillGroup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		resp     proctest.Response
		timeout  time.Duration
		timedOut bool
	}{
		// Long enough for the fake to start its grandchild on a loaded runner.
		{
			"timeout kills the group",
			proctest.Response{Sleep: 2 * time.Minute, Grandchild: 2 * time.Minute},
			5 * time.Second, true,
		},
		{"parent exits first", proctest.Response{Grandchild: 2 * time.Minute}, time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := proctest.Install(
				t,
				"child",
				proctest.Scenario{Responses: []proctest.Response{tc.resp}},
			)
			start := time.Now()
			out := run(context.Background(), command{name: fake, timeout: tc.timeout})
			if took := time.Since(start); took > tc.timeout+20*time.Second {
				t.Fatalf("run took %s: it waited for the grandchild", took)
			}
			if out.timedOut != tc.timedOut {
				t.Fatalf("outcome = %+v", out)
			}
			calls := proctest.Calls(t, fake)
			if len(calls) != 1 || calls[0].GrandchildPID == 0 {
				t.Fatalf("calls = %+v", calls)
			}
			if !gone(calls[0].GrandchildPID, 3*time.Second) {
				t.Fatalf("grandchild %d survived the call", calls[0].GrandchildPID)
			}
		})
	}
}
