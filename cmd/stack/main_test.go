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

	"github.com/shuymn/stack/internal/github"
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
		"run", "outbound", "--ext", "demo", "--title-file", "/dev/stdin")
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

// TestClosedStderrKeepsAutoCleanup: with stderr a pipe whose reader is gone (`2>&1 | head -1`), the
// waiting line of pr merge --auto fails instead of ending stack by SIGPIPE, so a stop still
// disables auto-merge.
func TestClosedStderrKeepsAutoCleanup(t *testing.T) {
	skipWithoutSIGTERM(t)
	const head = "0123456789abcdef0123456789abcdef01234567"
	repo := github.Repo{Owner: "octo", Name: "demo"}
	resp := func(args []string, stdout string) proctest.Response {
		return proctest.Response{Args: args, Stdout: stdout}
	}
	autoRead := []string{
		"pr",
		"view",
		"7",
		"--json",
		"state,headRefOid,mergeCommit,autoMergeRequest",
	}
	disable := []string{"pr", "merge", "7", "--disable-auto"}
	r := gittest.New(t)
	gh := proctest.Install(t, "gh", proctest.Scenario{Responses: []proctest.Response{
		resp([]string{
			"pr", "view", "7", "--json",
			"headRefOid,baseRefName,state,isDraft,mergeable,autoMergeRequest",
		}, `{"headRefOid":"`+head+`","state":"OPEN","isDraft":false,"mergeable":"MERGEABLE",`+
			`"autoMergeRequest":null}`),
		resp([]string{
			"repo", "view", "--json",
			"nameWithOwner,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed",
		}, `{"nameWithOwner":"octo/demo","mergeCommitAllowed":true}`),
		resp([]string{"pr", "view", "7", "--json", "commits"}, `{"commits":[]}`),
		resp(github.ChecksArgs(repo, head, ""), `{"data":{"repository":{"object":{"oid":"`+head+
			`","statusCheckRollup":null}}}}`),
		resp(github.MergeQueueArgs(repo, 7),
			`{"data":{"repository":{"pullRequest":{"isMergeQueueEnabled":false}}}}`),
		resp([]string{"pr", "merge", "7", "--merge", "--auto", "--match-head-commit", head}, ""),
		resp(autoRead, `{"state":"OPEN","headRefOid":"`+head+`","mergeCommit":null,`+
			`"autoMergeRequest":{"mergeMethod":"MERGE"}}`),
		resp(disable, ""),
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
	cmd, _ := stack(t, r.Dir, env, "pr", "merge", "7", head, "--merge", "--auto",
		"--allow-no-checks")
	cmd.Stderr = nil
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stderr.Close() // the consumer is gone before stack writes anything
	read := func() bool {
		return slices.ContainsFunc(proctest.Calls(t, gh), func(c proctest.Call) bool {
			return slices.Equal(c.Args, autoRead)
		})
	}
	deadline := time.Now().Add(30 * time.Second)
	for !read() {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("the first read-back never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // past the waiting line, into the 20 s wait
	// Before the fix SIGPIPE has already ended stack; the signal then fails or finds it gone.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	code := waitExit(t, cmd, 30*time.Second)
	disabled := slices.ContainsFunc(proctest.Calls(t, gh), func(c proctest.Call) bool {
		return slices.Equal(c.Args, disable)
	})
	if code != 1 || !disabled {
		t.Errorf("exit code %d, disable called %v; want 1 and the disable", code, disabled)
	}
}
