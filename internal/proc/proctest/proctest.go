// Package proctest installs fake executables for tests by copying the test
// binary itself.
//
// Install copies the running test binary to <dir>/<name> (<name>.exe on
// Windows) and writes the scenario next to it. When the copy runs, Main, which
// each package's TestMain must call before m.Run, finds that scenario beside
// its own executable, answers from it and exits instead of running tests.
// Detection does not depend on the environment, so a fake still works when the
// code under test replaces Cmd.Env or finds the fake through PATH.
//
// Each call is appended to a log next to the fake; Calls reads it. A response
// may spawn a grandchild that inherits stdout and stderr and outlives the fake.
// Install's cleanup asks every grandchild to exit and waits for it, so the
// test's temporary directory can be removed (on Windows a running image cannot
// be deleted). Tests that change PATH to find fakes must not run in parallel.
package proctest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	scenarioSuffix = ".proctest.json"
	logSuffix      = ".proctest.log"
	stopSuffix     = ".proctest.stop"
	doneSuffix     = ".proctest.done."
	grandchildEnv  = "STACK_PROCTEST_GRANDCHILD"

	// NoMatchExit is the exit status of a fake whose scenario has no response
	// for its argv.
	NoMatchExit = 97

	defaultGrandchildLife = 30 * time.Second
	grandchildPoll        = 10 * time.Millisecond
	cleanupWait           = 10 * time.Second
)

// Response is one scripted answer. The fake performs, in order: read stdin,
// spawn the grandchild, log the call, write stdout and stderr, sleep, exit.
type Response struct {
	// Args matches the arguments after argv[0] exactly; nil matches any call
	// and an empty non-nil slice matches only a call without arguments.
	// Serialised without omitempty so the two stay distinct.
	Args   []string `json:"args"`
	Stdout string   `json:"stdout,omitempty"`
	Stderr string   `json:"stderr,omitempty"`
	Exit   int      `json:"exit,omitempty"`
	// Sleep delays the exit after writing output.
	Sleep time.Duration `json:"sleep,omitempty"`
	// Grandchild, when positive, starts a process that holds the fake's
	// stdout and stderr open for at most this long, or until cleanup.
	Grandchild time.Duration `json:"grandchild,omitempty"`
}

// Scenario lists responses; the first whose Args match is used.
type Scenario struct {
	Responses []Response `json:"responses"`
}

// Call is one logged invocation of a fake.
type Call struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
	// GrandchildPID is the PID of the spawned grandchild, or zero.
	GrandchildPID int `json:"grandchildPid,omitempty"`
}

var mainCalled bool

// Main must be called from TestMain before m.Run. In a fake or grandchild it
// runs the scenario and exits; in the test binary itself it returns.
func Main() {
	mainCalled = true
	if life := os.Getenv(grandchildEnv); life != "" {
		os.Exit(runGrandchild(life))
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	base := trimExe(exe)
	data, err := os.ReadFile(base + scenarioSuffix)
	if err != nil {
		return
	}
	os.Exit(runFake(exe, base, data))
}

// Install copies the test binary as a fake named name into a new temporary
// directory and returns the fake's absolute path.
func Install(tb testing.TB, name string, s Scenario) string {
	tb.Helper()
	return InstallAt(tb, tb.TempDir(), name, s)
}

// InstallAt is Install into dir, so several fakes can share one PATH entry.
func InstallAt(tb testing.TB, dir, name string, s Scenario) string {
	tb.Helper()
	if !mainCalled {
		tb.Fatal("proctest: call proctest.Main from TestMain before m.Run")
	}
	exe, err := os.Executable()
	if err != nil {
		tb.Fatalf("proctest: %v", err)
	}
	dst, err := filepath.Abs(filepath.Join(dir, trimExe(name)))
	if err != nil {
		tb.Fatalf("proctest: %v", err)
	}
	base := dst
	if runtime.GOOS == "windows" {
		dst += ".exe"
	}
	data, err := json.Marshal(s)
	if err != nil {
		tb.Fatalf("proctest: %v", err)
	}
	if err := copyExecutable(exe, dst); err != nil {
		tb.Fatalf("proctest: %v", err)
	}
	if err := os.WriteFile(base+scenarioSuffix, data, 0o600); err != nil {
		tb.Fatalf("proctest: %v", err)
	}
	tb.Cleanup(func() { stopGrandchildren(tb, base) })
	return dst
}

// Calls returns the logged calls of the fake at path, oldest first.
func Calls(tb testing.TB, path string) []Call {
	tb.Helper()
	calls, err := readCalls(trimExe(path))
	if err != nil {
		tb.Fatalf("proctest: %v", err)
	}
	return calls
}

func readCalls(base string) ([]Call, error) {
	data, err := os.ReadFile(base + logSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read call log: %w", err)
	}
	var calls []Call
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("decode call log: %w", err)
		}
		calls = append(calls, c)
	}
	return calls, nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open test binary: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return fmt.Errorf("create fake: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy fake: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close fake: %w", err)
	}
	return nil
}

func trimExe(path string) string {
	if ext := filepath.Ext(path); strings.EqualFold(ext, ".exe") {
		return strings.TrimSuffix(path, ext)
	}
	return path
}

func runFake(exe, base string, data []byte) int {
	var s Scenario
	if err := json.Unmarshal(data, &s); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "proctest: scenario: %v\n", err)
		return NoMatchExit
	}
	args := os.Args[1:]
	stdin, _ := io.ReadAll(os.Stdin)
	call := Call{Args: args, Stdin: string(stdin)}
	resp, ok := match(s, args)
	if ok && resp.Grandchild > 0 {
		pid, err := spawnGrandchild(exe, resp.Grandchild)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "proctest: grandchild: %v\n", err)
			return NoMatchExit
		}
		call.GrandchildPID = pid
	}
	if err := appendCall(base, call); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "proctest: log: %v\n", err)
		return NoMatchExit
	}
	if !ok {
		_, _ = fmt.Fprintf(os.Stderr, "proctest: no response for %q\n", args)
		return NoMatchExit
	}
	_, _ = io.WriteString(os.Stdout, resp.Stdout)
	_, _ = io.WriteString(os.Stderr, resp.Stderr)
	time.Sleep(resp.Sleep)
	return resp.Exit
}

func match(s Scenario, args []string) (Response, bool) {
	for _, r := range s.Responses {
		if r.Args == nil || slices.Equal(r.Args, args) {
			return r, true
		}
	}
	return Response{}, false
}

func appendCall(base string, c Call) error {
	line, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode call: %w", err)
	}
	f, err := os.OpenFile(base+logSuffix, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open call log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write call log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close call log: %w", err)
	}
	return nil
}

// spawnGrandchild starts the fake's own binary in grandchild mode with the
// fake's stdout and stderr, and leaves it running.
func spawnGrandchild(exe string, life time.Duration) (int, error) {
	// The grandchild outlives this process on purpose; it ends itself.
	cmd := exec.CommandContext(context.Background(), exe)
	cmd.Env = append(os.Environ(), grandchildEnv+"="+life.String())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// runGrandchild holds the inherited stdout and stderr until its lifetime ends
// or Install's cleanup creates the stop file, then records that it is done.
func runGrandchild(life string) int {
	d, err := time.ParseDuration(life)
	if err != nil || d <= 0 {
		d = defaultGrandchildLife
	}
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	base := trimExe(exe)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(base + stopSuffix); err == nil {
			break
		}
		time.Sleep(grandchildPoll)
	}
	done := base + doneSuffix + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(done, nil, 0o600); err != nil {
		return 1
	}
	return 0
}

// stopGrandchildren signals every grandchild of the fake at base to exit and
// waits until each has recorded that it is done.
func stopGrandchildren(tb testing.TB, base string) {
	tb.Helper()
	calls, err := readCalls(base)
	if err != nil {
		tb.Errorf("proctest: %v", err)
		return
	}
	var pids []int
	for _, c := range calls {
		if c.GrandchildPID != 0 {
			pids = append(pids, c.GrandchildPID)
		}
	}
	if len(pids) == 0 {
		return
	}
	if err := os.WriteFile(base+stopSuffix, nil, 0o600); err != nil {
		tb.Errorf("proctest: %v", err)
		return
	}
	deadline := time.Now().Add(cleanupWait)
	for _, pid := range pids {
		done := base + doneSuffix + strconv.Itoa(pid)
		for {
			if _, err := os.Stat(done); err == nil {
				break
			}
			if time.Now().After(deadline) {
				tb.Errorf("proctest: grandchild %d did not stop within %s", pid, cleanupWait)
				return
			}
			time.Sleep(grandchildPoll)
		}
	}
}
