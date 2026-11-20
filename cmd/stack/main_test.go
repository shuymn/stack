package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// runMainEnv makes the re-executed test binary run main with its arguments.
const runMainEnv = "STACK_MAIN_TEST_RUN"

func TestMain(m *testing.M) {
	proctest.Main()
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	os.Exit(gittest.Main(m))
}

// stack starts this test binary as `stack args...` in dir with env.
func stack(t *testing.T, dir string, env []string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	//nolint:gosec // G702: re-executes this test binary with test-chosen arguments.
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(env), runMainEnv+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd, &stderr
}

// waitExit waits up to limit for cmd and returns its exit code; it fails the test on a hang.
func waitExit(t *testing.T, cmd *exec.Cmd, limit time.Duration) int {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return cmd.ProcessState.ExitCode()
	case <-time.After(limit):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("stack did not exit within %s of SIGTERM", limit)
		return 0
	}
}

func skipWithoutSIGTERM(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM cannot be sent to a process on Windows")
	}
}

// TestSignalEndsOtherCommands: a command other than pr merge keeps the default exit on SIGTERM,
// also while it blocks reading a --title-file that never ends.
func TestSignalEndsOtherCommands(t *testing.T) {
	skipWithoutSIGTERM(t)
	home := t.TempDir()
	cmd, _ := stack(t, t.TempDir(), []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")},
		"check", "public", "a..b", "--title-file", "/dev/stdin")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // a signal before main starts ends it the same way
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := waitExit(t, cmd, 10*time.Second); code != -1 {
		t.Errorf("exit code %d, want -1 (ended by the signal)", code)
	}
}

// TestSignalStopsPrMerge: SIGTERM during pr merge kills the running gh and refuses (exit 1) instead
// of ending stack.
func TestSignalStopsPrMerge(t *testing.T) {
	skipWithoutSIGTERM(t)
	r := gittest.New(t)
	gh := proctest.Install(t, "gh", proctest.Scenario{Responses: []proctest.Response{
		{Sleep: 30 * time.Second},
	}})
	env := []string{"HOME=" + t.TempDir()}
	for _, kv := range r.Env {
		if name, value, _ := strings.Cut(kv, "="); name == "PATH" {
			kv = "PATH=" + filepath.Dir(gh) + string(os.PathListSeparator) + value
		} else if name == "HOME" {
			continue
		}
		env = append(env, kv)
	}
	cmd, stderr := stack(t, r.Dir, env, "pr", "merge", "7", "0123456789abc", "--merge")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Signal only once gh runs, so the handler is in place.
	deadline := time.Now().Add(30 * time.Second)
	for len(proctest.Calls(t, gh)) == 0 {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("gh never ran; stderr %q", stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code := waitExit(t, cmd, 10*time.Second)
	if code != 1 || !strings.Contains(stderr.String(), "could not read PR 7") {
		t.Errorf("exit code %d, stderr %q; want 1 and a refusal", code, stderr)
	}
}
