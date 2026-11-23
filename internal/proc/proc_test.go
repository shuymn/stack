package proc_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestMain(m *testing.M) {
	proctest.Main()
	os.Exit(m.Run())
}

const (
	testWaitDelay = 500 * time.Millisecond
	// grandchildLife is far above every bound asserted below, so a test that
	// waited for the grandchild instead of WaitDelay fails clearly.
	grandchildLife = 20 * time.Second
)

func one(r proctest.Response) proctest.Scenario {
	return proctest.Scenario{Responses: []proctest.Response{r}}
}

func TestRunFakeGh(t *testing.T) {
	t.Parallel()

	gh := proctest.Install(t, "gh", proctest.Scenario{Responses: []proctest.Response{
		{Args: []string{"pr", "view", "1"}, Stdout: "open\n", Stderr: "note\n"},
		{Stderr: "unexpected\n", Exit: 3},
	}})
	ctx := context.Background()
	runner := proc.Exec{}

	res, err := runner.Run(ctx, proc.Cmd{
		Name:  gh,
		Args:  []string{"pr", "view", "1"},
		Stdin: []byte("request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "open\n" || string(res.Stderr) != "note\n" || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.TimedOut || res.WaitDelayExceeded || res.Truncated || res.StderrTruncated {
		t.Fatalf("unexpected flags: %+v", res)
	}

	res, err = runner.Run(ctx, proc.Cmd{Name: gh, Args: []string{"pr", "merge", "1", "--squash"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || string(res.Stderr) != "unexpected\n" {
		t.Fatalf("result = %+v", res)
	}

	calls := proctest.Calls(t, gh)
	want := [][]string{{"pr", "view", "1"}, {"pr", "merge", "1", "--squash"}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i, c := range calls {
		if !slices.Equal(c.Args, want[i]) {
			t.Errorf("call %d args = %q, want %q", i, c.Args, want[i])
		}
	}
	if calls[0].Stdin != "request" || calls[1].Stdin != "" {
		t.Errorf("stdin = %q, %q", calls[0].Stdin, calls[1].Stdin)
	}
}

func TestFakeEmptyArgsMatchesOnlyNoArgs(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", proctest.Scenario{Responses: []proctest.Response{
		{Args: []string{}, Stdout: "no args"},
		{Args: []string{"view"}, Stdout: "view"},
	}})
	for _, tc := range []struct {
		args []string
		want string
	}{{nil, "no args"}, {[]string{"view"}, "view"}} {
		res, err := proc.Exec{}.Run(context.Background(), proc.Cmd{Name: fake, Args: tc.args})
		if err != nil || string(res.Stdout) != tc.want {
			t.Errorf("args %q: stdout = %q, err = %v, want %q", tc.args, res.Stdout, err, tc.want)
		}
	}
}

// TestInstallCleanupWaitsForGrandchildExit checks that Install's cleanup
// returns only after the grandchild process has exited. Under -race the
// race detector delays the grandchild's exit, which exposes a cleanup that
// returns early; on Windows the subtest's TempDir removal would then fail.
func TestInstallCleanupWaitsForGrandchildExit(t *testing.T) {
	var pid int
	t.Run("run", func(t *testing.T) {
		fake := proctest.Install(t, "fake", one(proctest.Response{Grandchild: grandchildLife}))
		runner := proc.Exec{WaitDelay: testWaitDelay}
		if _, err := runner.Run(context.Background(), proc.Cmd{Name: fake}); err != nil {
			t.Fatal(err)
		}
		calls := proctest.Calls(t, fake)
		if len(calls) != 1 || calls[0].GrandchildPID == 0 {
			t.Fatalf("calls = %+v", calls)
		}
		pid = calls[0].GrandchildPID
	})
	if pid == 0 || runtime.GOOS == "windows" {
		return
	}
	p, err := os.FindProcess(pid)
	if err == nil && p.Signal(syscall.Signal(0)) == nil {
		t.Fatalf("grandchild %d still running after cleanup", pid)
	}
}

// TestLookPathExeRunsFake changes PATH, so it must not run in parallel.
func TestLookPathExeRunsFake(t *testing.T) {
	dir := t.TempDir()
	fake := proctest.InstallAt(t, dir, "stack-demo", one(proctest.Response{Stdout: "pass"}))
	t.Setenv("PATH", dir)

	path, err := proc.LookPathExe("stack-demo")
	if err != nil || path != fake {
		t.Fatalf("path = %q, err = %v, want %q", path, err, fake)
	}
	res, err := proc.Exec{}.Run(
		context.Background(),
		proc.Cmd{Name: path, Args: []string{"describe"}},
	)
	if err != nil || string(res.Stdout) != "pass" {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	calls := proctest.Calls(t, fake)
	if len(calls) != 1 || !slices.Equal(calls[0].Args, []string{"describe"}) {
		t.Fatalf("calls = %+v", calls)
	}
	if _, err := proc.LookPathExe("stack-missing"); !errors.Is(err, proc.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRunDirAndEnv(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", one(proctest.Response{Stdout: "ok"}))
	dir := t.TempDir()
	// Env replaces the environment and Dir is applied; the fake still finds
	// its scenario because detection does not depend on either.
	res, err := proc.Exec{}.Run(
		context.Background(),
		proc.Cmd{Name: fake, Dir: dir, Env: []string{"A=1"}},
	)
	if err != nil || string(res.Stdout) != "ok" {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
}

func TestRunTimeout(t *testing.T) {
	t.Parallel()

	const childSleep = time.Minute
	fake := proctest.Install(
		t,
		"fake",
		one(proctest.Response{Stdout: "partial", Sleep: childSleep}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	res, err := proc.Exec{WaitDelay: testWaitDelay}.Run(ctx, proc.Cmd{Name: fake})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("result = %+v, want TimedOut and exit -1", res)
	}
	// The child was killed at the deadline rather than waited for: a run that waited would
	// last the child's whole sleep. The bound is far from both, so a slow scheduler cannot
	// flip it.
	if took := time.Since(start); took >= childSleep/2 {
		t.Fatalf("took %s, want the child killed long before its %s sleep ends", took, childSleep)
	}
}

// TestRunTimeoutGrandchildHoldsPipe covers a child that leaves a grandchild
// holding the inherited stdout: Run returns once WaitDelay cuts the pipe after
// the context's deadline, not when the grandchild exits. The check is that it
// returns before the grandchild could have exited, 5 s after the deadline.
func TestRunTimeoutGrandchildHoldsPipe(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", one(proctest.Response{
		Stdout: "partial", Sleep: time.Minute, Grandchild: grandchildLife,
	}))
	// Long enough for the fake to start its grandchild on a slow runner.
	const deadline = 15 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	end, _ := ctx.Deadline()

	res, err := proc.Exec{WaitDelay: testWaitDelay}.Run(ctx, proc.Cmd{Name: fake})
	returned := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	calls := proctest.Calls(t, fake)
	if len(calls) != 1 || calls[0].GrandchildPID == 0 {
		t.Fatalf("grandchild not started before the deadline: calls = %+v", calls)
	}
	if !res.TimedOut {
		t.Fatalf("result = %+v, want TimedOut", res)
	}
	// The grandchild starts after the context does, so it outlives the context's deadline by
	// at least grandchildLife-deadline (5 s): a Run that returned only when the grandchild
	// exited would return that long after the deadline. That is the bound, not WaitDelay, so
	// scheduling delay cannot flip the check.
	if took, limit := returned.Sub(end), grandchildLife-deadline; took >= limit {
		t.Fatalf(
			"returned %s after the context's deadline, want under %s, before the grandchild "+
				"could exit",
			took,
			limit,
		)
	}
}

// TestRunWaitDelayAfterChildExit checks that a pipe a grandchild holds after
// the child exits is cut after WaitDelay and reported independently of the
// exit status, so a caller cannot take the output it received as complete.
func TestRunWaitDelayAfterChildExit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		stdout string
		exit   int
	}{
		{name: "exit 0", stdout: "done"},
		{name: "exit 7", stdout: `{"status":"pass"}`, exit: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := proctest.Install(t, "fake", one(proctest.Response{
				Stdout: tc.stdout, Exit: tc.exit, Grandchild: grandchildLife,
			}))
			start := time.Now()
			res, err := proc.Exec{
				WaitDelay: testWaitDelay,
			}.Run(
				context.Background(),
				proc.Cmd{Name: fake},
			)
			if err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); took > grandchildLife/2 {
				t.Fatalf("took %s, waited for the grandchild", took)
			}
			if !res.WaitDelayExceeded || res.TimedOut || res.ExitCode != tc.exit ||
				string(res.Stdout) != tc.stdout {
				t.Fatalf("result = %+v, want exit %d, WaitDelayExceeded and stdout", res, tc.exit)
			}
		})
	}
}

// TestRunStderrWriter checks that a caller-supplied stderr writer receives
// all of stderr, past the 64 KiB cap that applies only to captured stderr.
func TestRunStderrWriter(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 64<<10) + "Z"
	fake := proctest.Install(t, "fake", one(proctest.Response{Stderr: long}))
	var stderr bytes.Buffer
	res, err := proc.Exec{}.Run(context.Background(), proc.Cmd{Name: fake, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if stderr.String() != long || len(res.Stderr) != 0 || res.StderrTruncated {
		t.Fatalf("streamed %d bytes, captured %d, truncated %v; want all %d streamed",
			stderr.Len(), len(res.Stderr), res.StderrTruncated, len(long))
	}

	res, err = proc.Exec{}.Run(context.Background(), proc.Cmd{Name: fake})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stderr) != 64<<10 || !res.StderrTruncated {
		t.Fatalf(
			"captured %d bytes, truncated %v; want the 64 KiB cap",
			len(res.Stderr),
			res.StderrTruncated,
		)
	}
}

var errWriterClosed = errors.New("writer closed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWriterClosed }

// TestRunStderrWriteError checks that a failing stderr writer is reported
// rather than silently dropping stderr, and that stdout is unaffected.
func TestRunStderrWriteError(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", proctest.Scenario{Responses: []proctest.Response{
		{Stdout: "ok", Stderr: "diagnostic"},
	}})
	cmd := proc.Cmd{Name: fake, Stderr: failingWriter{}}
	res, err := proc.Exec{}.Run(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(res.StderrErr, errWriterClosed) || string(res.Stdout) != "ok" ||
		res.ExitCode != 0 {
		t.Fatalf("result = %+v, want StderrErr with stdout intact", res)
	}
	err = proc.Exec{}.Stream(context.Background(), cmd, func(io.Reader) error { return nil })
	if !errors.Is(err, errWriterClosed) {
		t.Fatalf("stream err = %v, want the stderr write error", err)
	}
}

// TestRunStdinNotRead checks that stdin the child never reads, held open by a
// grandchild, cannot keep Run from returning.
func TestRunStdinNotRead(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", one(proctest.Response{
		Stdout: "ok", NoStdin: true, Grandchild: grandchildLife,
	}))
	start := time.Now()
	res, err := proc.Exec{WaitDelay: testWaitDelay}.Run(context.Background(), proc.Cmd{
		Name:  fake,
		Stdin: bytes.Repeat([]byte("x"), 4<<20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > grandchildLife/2 {
		t.Fatalf("took %s", took)
	}
	if string(res.Stdout) != "ok" || !res.WaitDelayExceeded {
		t.Fatalf("result = %+v, want stdout and WaitDelayExceeded", res)
	}
}

func TestRunCanceled(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", one(proctest.Response{Sleep: time.Minute}))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := proc.Exec{WaitDelay: testWaitDelay}.Run(ctx, proc.Cmd{Name: fake})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunOutputCap(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", one(proctest.Response{Stdout: "0123456789"}))
	cases := []struct {
		limit     int64
		want      string
		truncated bool
	}{
		{limit: 4, want: "0123", truncated: true},
		{limit: 10, want: "0123456789"},
		{limit: 0, want: "0123456789"},
	}
	for _, tc := range cases {
		res, err := proc.Exec{}.Run(context.Background(), proc.Cmd{Name: fake, MaxStdout: tc.limit})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Stdout) != tc.want || res.Truncated != tc.truncated {
			t.Errorf("limit %d: stdout = %q truncated = %v, want %q %v",
				tc.limit, res.Stdout, res.Truncated, tc.want, tc.truncated)
		}
	}
}

func TestRunDefaultCap(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(
		t,
		"fake",
		one(proctest.Response{Stdout: strings.Repeat("x", 1<<20+1)}),
	)
	res, err := proc.Exec{}.Run(context.Background(), proc.Cmd{Name: fake})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Stdout) != 1<<20 || res.ExitCode != 0 {
		t.Fatalf("len = %d truncated = %v exit = %d", len(res.Stdout), res.Truncated, res.ExitCode)
	}
}

func TestRunStartFailure(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := (proc.Exec{}).Run(context.Background(), proc.Cmd{Name: missing}); err == nil {
		t.Fatal("want start error")
	}
}

func TestStream(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", proctest.Scenario{Responses: []proctest.Response{
		{Args: []string{"ok"}, Stdout: "a\nb\nc\n"},
		{Args: []string{"fail"}, Stdout: "a\n", Stderr: "bad range\n", Exit: 2},
		{Args: []string{"hang"}, Stdout: "a\n", Sleep: time.Minute},
		{Args: []string{"leak"}, Stdout: "a\n", Grandchild: grandchildLife},
	}})
	runner := proc.Exec{WaitDelay: testWaitDelay}
	lines := func(got *[]string) func(io.Reader) error {
		return func(r io.Reader) error {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				*got = append(*got, sc.Text())
			}
			return sc.Err()
		}
	}

	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		var got []string
		if err := runner.Stream(
			context.Background(),
			proc.Cmd{Name: fake, Args: []string{"ok"}},
			lines(&got),
		); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("early return discards the rest", func(t *testing.T) {
		t.Parallel()
		consume := func(io.Reader) error { return nil }
		if err := runner.Stream(
			context.Background(),
			proc.Cmd{Name: fake, Args: []string{"ok"}},
			consume,
		); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("exit status", func(t *testing.T) {
		t.Parallel()
		var got []string
		err := runner.Stream(
			context.Background(),
			proc.Cmd{Name: fake, Args: []string{"fail"}},
			lines(&got),
		)
		if !errors.Is(err, proc.ErrExit) || !strings.Contains(err.Error(), "bad range") {
			t.Fatalf("err = %v, want ErrExit with stderr", err)
		}
	})
	t.Run("consumer error stops the child", func(t *testing.T) {
		t.Parallel()
		errStop := errors.New("stop")
		start := time.Now()
		err := runner.Stream(context.Background(), proc.Cmd{Name: fake, Args: []string{"hang"}},
			func(io.Reader) error { return errStop })
		if !errors.Is(err, errStop) {
			t.Fatalf("err = %v, want consumer error", err)
		}
		if took := time.Since(start); took > grandchildLife/2 {
			t.Fatalf("took %s", took)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var got []string
		err := runner.Stream(ctx, proc.Cmd{Name: fake, Args: []string{"hang"}}, lines(&got))
		if !errors.Is(err, proc.ErrTimedOut) {
			t.Fatalf("err = %v, want ErrTimedOut", err)
		}
	})
	t.Run("wait delay", func(t *testing.T) {
		t.Parallel()
		var got []string
		start := time.Now()
		err := runner.Stream(
			context.Background(),
			proc.Cmd{Name: fake, Args: []string{"leak"}},
			lines(&got),
		)
		if !errors.Is(err, proc.ErrWaitDelay) {
			t.Fatalf("err = %v, want ErrWaitDelay", err)
		}
		if took := time.Since(start); took > grandchildLife/2 {
			t.Fatalf("took %s, waited for the grandchild", took)
		}
	})
}
