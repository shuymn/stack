package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
)

// testCutoffValue is the cutoff the test build injects for both decisions (§3.5), and
// defaultTestCutoffs the default fixed-fixture build's cutoffs: both decisions at it.
const (
	testCutoffValue    = "0.5"
	defaultTestCutoffs = Qualification + "=" + testCutoffValue + "," + Duplicate + "=" +
		testCutoffValue
)

// built holds the binaries TestMain builds: stack and a test build of stack-jev-classify whose
// endpoint is spy.server and whose cutoffs are the test cutoffs; committed/ holds one with the
// committed cutoff table, and builds the directory of each named test build.
var built struct {
	dir, stack, classify, committed string
	builds                          map[string]string
	err                             error
}

// testBuilds are the named test builds a fixed fixture may run under, by their test cutoffs: the
// default one (both decisions at the test cutoff) is the unnamed build.
var testBuilds = map[string]string{
	"no-qualification": Duplicate + "=" + testCutoffValue,
	"no-duplicate":     Qualification + "=" + testCutoffValue,
	"no-decisions":     "none",
}

// binPath is the PATH entry that puts the build a fixture names before the default one: the
// committed-cutoff build for cal fixtures, a named test build where a fixed fixture asks for it.
func binPath(dir string) string {
	return "PATH=" + dir + string(os.PathListSeparator) + built.dir +
		string(os.PathListSeparator) + os.Getenv("PATH")
}

// spy is the endpoint of the test builds: it records every request and fails the test that set
// fail.
var spy struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   int
	handle http.HandlerFunc
}

func startSpy() {
	spy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		spy.mu.Lock()
		spy.hits++
		h := spy.handle
		spy.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

// spyHits returns the requests the endpoint received so far.
func spyHits() int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return spy.hits
}

func buildBinaries() func() {
	dir, err := os.MkdirTemp("", "stack-jev-classify-test-")
	if err != nil {
		built.err = err
		return func() {}
	}
	built.dir = dir
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	built.stack = filepath.Join(dir, "stack"+exe)
	built.classify = filepath.Join(dir, "stack-jev-classify"+exe)
	built.committed = filepath.Join(dir, "committed")
	committedBin := filepath.Join(built.committed, "stack-jev-classify"+exe)
	endpoint := "-X github.com/shuymn/stack/extensions/internal/jev.endpoint=" + spy.server.URL
	cutoffs := " -X github.com/shuymn/stack/extensions/jev/classify.testCutoffs="
	ldflags := endpoint + cutoffs + defaultTestCutoffs
	env := gittest.CleanEnv(os.Environ())
	cmds := [][]string{
		{"build", "-o", built.stack, "../../../cmd/stack"},
		{"build", "-ldflags", ldflags, "-o", built.classify, "./cmd/stack-jev-classify"},
		{"build", "-ldflags", endpoint, "-o", committedBin, "./cmd/stack-jev-classify"},
	}
	built.builds = map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(testBuilds)) {
		built.builds[name] = filepath.Join(dir, name)
		cmds = append(cmds, []string{
			"build", "-ldflags", endpoint + cutoffs + testBuilds[name], "-o",
			filepath.Join(built.builds[name], "stack-jev-classify"+exe), "./cmd/stack-jev-classify",
		})
	}
	for _, c := range cmds {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, "go", c...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			built.err = fmt.Errorf("go %s: %w: %s", strings.Join(c, " "), err, out)
			break
		}
	}
	return func() { _ = os.RemoveAll(dir) }
}

func needBuilt(t *testing.T) {
	t.Helper()
	if built.err != nil {
		t.Fatalf("building the test binaries failed: %v", built.err)
	}
}

type runResult struct {
	code           int
	stdout, stderr string
}

// runBinary runs a built binary in dir with home as the home directory, the built directory first
// on PATH, no inherited key and extra variables.
func runBinary(
	t *testing.T, home, dir string, extra []string, name string, args ...string,
) runResult {
	t.Helper()
	needBuilt(t)
	env := slices.DeleteFunc(gittest.CleanEnv(os.Environ()), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, "HOME") || strings.EqualFold(k, "USERPROFILE") ||
			strings.EqualFold(k, "PATH") || strings.EqualFold(k, "TYPESAFE_API_KEY") ||
			strings.EqualFold(k, "CLAUDE_CONFIG_DIR")
	})
	env = append(env, "HOME="+home, "USERPROFILE="+home,
		"PATH="+built.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = append(env, extra...)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := cmd.ProcessState.ExitCode()
	if err != nil && code < 0 {
		t.Fatalf("%s %q: %v", name, args, err)
	}
	return runResult{code, stdout.String(), stderr.String()}
}

// aggregate is `stack run`'s output.
type aggregate struct {
	Stage   string `json:"stage"`
	Status  string `json:"status"`
	Results []struct {
		Name     string          `json:"name"`
		Status   string          `json:"status"`
		Context  string          `json:"context"`
		Findings json.RawMessage `json:"findings"`
		Error    string          `json:"error"`
	} `json:"results"`
	Skipped []string `json:"skipped"`
}

func parseAggregate(t *testing.T, out string) aggregate {
	t.Helper()
	var a aggregate
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("stack run output %q: %v", out, err)
	}
	return a
}
