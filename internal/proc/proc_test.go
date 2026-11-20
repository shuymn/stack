package proc_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	// slack absorbs scheduling noise on slow CI runners.
	slack = 1500 * time.Millisecond
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
		Stdin: strings.NewReader("request"),
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

	fake := proctest.Install(
		t,
		"fake",
		one(proctest.Response{Stdout: "partial", Sleep: time.Minute}),
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
	if took := time.Since(start); took > 2*time.Second+testWaitDelay+slack {
		t.Fatalf("took %s", took)
	}
}

// TestRunTimeoutGrandchildHoldsPipe covers a child that leaves a grandchild
// holding the inherited stdout: Run must return within WaitDelay of the
// context ending, not when the grandchild exits.
func TestRunTimeoutGrandchildHoldsPipe(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(t, "fake", one(proctest.Response{
		Stdout: "partial", Sleep: time.Minute, Grandchild: grandchildLife,
	}))
	// Long enough for the fake to start its grandchild on a slow runner.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ended := make(chan time.Time, 1)
	go func() {
		<-ctx.Done()
		ended <- time.Now()
	}()

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
	if took := returned.Sub(<-ended); took > testWaitDelay+slack {
		t.Fatalf(
			"returned %s after the context ended, want within WaitDelay %s",
			took,
			testWaitDelay,
		)
	}
}

func TestRunWaitDelayAfterChildExit(t *testing.T) {
	t.Parallel()

	fake := proctest.Install(
		t,
		"fake",
		one(proctest.Response{Stdout: "done", Grandchild: grandchildLife}),
	)
	start := time.Now()
	res, err := proc.Exec{WaitDelay: testWaitDelay}.Run(context.Background(), proc.Cmd{Name: fake})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > grandchildLife/2 {
		t.Fatalf("took %s, waited for the grandchild", took)
	}
	if !res.WaitDelayExceeded || res.TimedOut || res.ExitCode != 0 || string(res.Stdout) != "done" {
		t.Fatalf("result = %+v, want WaitDelayExceeded with stdout", res)
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
		one(proctest.Response{Stdout: strings.Repeat("x", proc.DefaultMaxStdout+1)}),
	)
	res, err := proc.Exec{}.Run(context.Background(), proc.Cmd{Name: fake})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Stdout) != proc.DefaultMaxStdout || res.ExitCode != 0 {
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
