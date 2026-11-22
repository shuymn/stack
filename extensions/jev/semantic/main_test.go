package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// testCutoffValue is the cutoff the test build injects for both decisions (§3.5).
const testCutoffValue = "0.5"

// built holds the binaries TestMain builds: stack, stack-public and a test build of
// stack-jev-semantic whose endpoint is fake.server and whose cutoffs are the test cutoffs.
var built struct {
	dir, stack, public, semantic string
	// committed holds a stack-jev-semantic with the committed cutoff table, for cal fixtures.
	committed string
	err       error
}

// committedPath is the PATH entry that puts the committed-cutoff build of stack-jev-semantic
// before the test build.
func committedPath() string {
	return "PATH=" + built.committed + string(os.PathListSeparator) + built.dir +
		string(os.PathListSeparator) + os.Getenv("PATH")
}

// fake is the endpoint the test build calls. Its handler is set per test; the default fails the
// test that set want.
var fake struct {
	server *httptest.Server
	mu     sync.Mutex
	handle http.HandlerFunc
	bodies [][]byte
}

func TestMain(m *testing.M) {
	proctest.Main()
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.bodies = append(fake.bodies, body)
		h := fake.handle
		fake.mu.Unlock()
		if h == nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	}))
	cleanup := buildBinaries()
	code := gittest.Main(m)
	cleanup()
	fake.server.Close()
	os.Exit(code)
}

// serve sets the fake endpoint's handler for one test and returns the bodies it received.
func serve(t *testing.T, h http.HandlerFunc) func() [][]byte {
	t.Helper()
	fake.mu.Lock()
	fake.handle, fake.bodies = h, nil
	fake.mu.Unlock()
	t.Cleanup(func() {
		fake.mu.Lock()
		fake.handle, fake.bodies = nil, nil
		fake.mu.Unlock()
	})
	return func() [][]byte {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return slices.Clone(fake.bodies)
	}
}

func buildBinaries() func() {
	dir, err := os.MkdirTemp("", "stack-jev-semantic-test-")
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
	built.public = filepath.Join(dir, "stack-public"+exe)
	built.semantic = filepath.Join(dir, "stack-jev-semantic"+exe)
	built.committed = filepath.Join(dir, "committed")
	committedBin := filepath.Join(built.committed, "stack-jev-semantic"+exe)
	endpoint := "-X github.com/shuymn/stack/extensions/internal/jev.endpoint=" + fake.server.URL
	ldflags := endpoint + " -X github.com/shuymn/stack/extensions/jev/semantic.testCutoffs=" +
		MetaDuty + "=" + testCutoffValue + "," + Unsupported + "=" + testCutoffValue
	env := gittest.CleanEnv(os.Environ())
	for _, c := range [][]string{
		{"build", "-o", built.stack, "../../../cmd/stack"},
		{"build", "-o", built.public, "../../public/cmd/stack-public"},
		{"build", "-ldflags", ldflags, "-o", built.semantic, "./cmd/stack-jev-semantic"},
		{"build", "-ldflags", endpoint, "-o", committedBin, "./cmd/stack-jev-semantic"},
	} {
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

func buildRepo(t *testing.T, spec RepoSpec) *gittest.Repo {
	t.Helper()
	r := gittest.New(t)
	if err := spec.Build(gittest.Ops{Repo: r}); err != nil {
		t.Fatal(err)
	}
	return r
}

// testDecisions are the test build's decisions, for in-process runs.
func testDecisions() map[string]jev.Decision {
	return parseTestCutoffs(MetaDuty + "=" + testCutoffValue + "," + Unsupported + "=" +
		testCutoffValue)
}

type mainResult struct {
	code   int
	resp   jev.Response
	stdout string
	stderr string
}

// runMain runs jev-semantic in process on the fixture repository r.
func runMain(t *testing.T, env Env, req any) mainResult {
	t.Helper()
	var stdin string
	if s, ok := req.(string); ok {
		stdin = s
	} else {
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		stdin = string(data)
	}
	var stdout, stderr bytes.Buffer
	env.Stdin, env.Stdout, env.Stderr = strings.NewReader(stdin), &stdout, &stderr
	if env.Decisions == nil {
		env.Decisions = testDecisions()
	}
	code := Main(t.Context(), env)
	var resp jev.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("stdout %q is not a response: %v", stdout.String(), err)
	}
	return mainResult{code, resp, stdout.String(), stderr.String()}
}

// repoEnv is an Env running git in r with r's isolated environment and a dummy key.
func repoEnv(r *gittest.Repo) Env {
	return Env{
		Environ: r.Env,
		Getenv: func(k string) string {
			if k == jev.KeyEnv {
				return "k"
			}
			return ""
		},
	}
}

func outboundReq(r *gittest.Repo, title, body string) map[string]any {
	return map[string]any{
		"stage": "outbound", "repo": r.Dir, "range": FixtureRange,
		"prompt": "", "title": title, "body": body, "branch": "feat/x",
	}
}

// answer is a 200 body answering every asked noul question with p.
func answer(t *testing.T, body []byte, p map[string]float64) []byte {
	t.Helper()
	var req struct {
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Errorf("request body: %v", err)
	}
	answers := map[string]any{}
	for q := range req.Questions {
		answers[q] = map[string]any{"type": "noul", "noul": p[q]}
	}
	out, err := json.Marshal(map[string]any{
		"model": jev.Pin, "answers": answers,
		"usage": map[string]int{"input_tokens": 100, "output_tokens": 1},
	})
	if err != nil {
		t.Error(err)
	}
	return out
}

type runResult struct {
	code           int
	stdout, stderr string
}

// runBinary runs a built binary in dir with r's environment, home as the home directory, the
// built directory first on PATH and extra variables.
func runBinary(
	t *testing.T, r *gittest.Repo, home, dir string, extra []string, name string, args ...string,
) runResult {
	t.Helper()
	needBuilt(t)
	env := slices.DeleteFunc(slices.Clone(r.Env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, "HOME") || strings.EqualFold(k, "USERPROFILE") ||
			strings.EqualFold(k, "PATH")
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
