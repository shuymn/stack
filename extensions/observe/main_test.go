package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// built holds the binaries TestMain builds: the real stack and stack-guard (replay, protocol)
// and, for the protocol test, stack-observe with the observetest tag.
var built struct {
	dir, stack, guard, observe string
	err                        error
}

func TestMain(m *testing.M) {
	proctest.Main()
	cleanup := buildBinaries()
	code := gittest.Main(m)
	cleanup()
	os.Exit(code)
}

// buildBinaries builds the binaries once before the tests run; the returned func removes them.
func buildBinaries() func() {
	dir, err := os.MkdirTemp("", "stack-observe-test-")
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
	built.guard = filepath.Join(dir, "stack-guard"+exe)
	built.observe = filepath.Join(dir, "stack-observe"+exe)
	env := gittest.CleanEnv(os.Environ())
	for _, c := range [][]string{
		{"build", "-o", built.stack, "../../cmd/stack"},
		{"build", "-o", built.guard, "../guard/cmd/stack-guard"},
		{"build", "-tags", "observetest", "-o", built.observe, "./cmd/stack-observe"},
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

// probePath stands for a private local path in child output and tool inputs; no JSON observe
// writes may contain it.
var probePath = "/" + "private" + "/probe/hosts.yml"

// expiring is a work deadline that passes after a number of Err calls, so a test can cut a step
// in the middle deterministically.
type expiring struct {
	context.Context //nolint:containedctx // the parent of a test deadline

	left atomic.Int64
}

func expireAfter(n int64) *expiring {
	e := &expiring{Context: context.Background()}
	e.left.Store(n)
	return e
}

func (e *expiring) Err() error {
	if e.left.Add(-1) < 0 {
		return context.DeadlineExceeded
	}
	return nil
}

// now is the fixed end of every test window; the fixtures' records are on the day before.
var now = time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)

// fixture is one test HOME with fake tools.
type fixture struct {
	t    *testing.T
	home string
	bin  string
	cwd  string
	// tools maps a tool name to its path; LookPath fails for any other name.
	tools map[string]string
	opts  Options
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:     t,
		home:  t.TempDir(),
		bin:   t.TempDir(),
		cwd:   t.TempDir(),
		tools: map[string]string{},
	}
	f.opts = Options{
		Home:     f.home,
		Env:      []string{"HOME=" + f.home, "USERPROFILE=" + f.home, "PATH=" + f.bin},
		LookPath: f.lookPath,
		Now:      func() time.Time { return now },
		Location: time.UTC,
		Budget:   time.Minute,
		Reserve:  5 * time.Second,
		// Generous for proctest fakes under load; a test that wants the timeout path sets its own.
		replayTimeout: 30 * time.Second,
	}
	return f
}

var errNotOnPath = errors.New("not on PATH")

func (f *fixture) lookPath(name string) (string, error) {
	if p, ok := f.tools[name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("%s: %w", name, errNotOnPath)
}

// fake installs a proctest fake as name. A fake stack comes with a stack-guard that denies
// nothing, as both are installed together, unless the test set stack-guard already; a test that
// wants stack-guard absent deletes it from tools.
func (f *fixture) fake(name string, responses ...proctest.Response) string {
	f.t.Helper()
	p := proctest.InstallAt(f.t, f.bin, name, proctest.Scenario{Responses: responses})
	f.tools[name] = p
	if _, ok := f.tools["stack-guard"]; name == "stack" && !ok {
		f.fake("stack-guard", proctest.Response{Args: hookArgs})
	}
	return p
}

// write creates a file under home with parent directories.
func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	//nolint:gosec // G703: rel is a test-chosen path under the test HOME.
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// transcript writes records as a parent transcript of project -p.
func (f *fixture) transcript(name string, records ...map[string]any) string {
	f.t.Helper()
	return f.write(".claude/projects/-p/"+name+".jsonl", jsonl(f.t, records...))
}

// fromTestdata writes a testdata transcript template with @@CWD@@ replaced by cwd.
func (f *fixture) fromTestdata(file, rel, cwd string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		f.t.Fatal(err)
	}
	quoted := mustJSON(f.t, cwd)
	text := strings.ReplaceAll(string(data), "@@CWD@@", strings.Trim(string(quoted), `"`))
	return f.write(rel, text)
}

// state is a run's state over the fixture window with work as its deadline, for unit tests.
func (f *fixture) state(work context.Context) *state {
	return &state{
		o: f.opts.withDefaults(), since: now.Add(-window), until: now, work: work,
		seen: map[string]bool{}, cwds: map[string]int{}, refs: map[string]bool{},
		t3Sessions: map[string]bool{}, strs: map[string]string{},
		latest: Latest{Sources: map[string]*Source{}, Items: []Item{}},
	}
}

func (f *fixture) run() (Response, Latest) {
	f.t.Helper()
	resp := Run(context.Background(), f.opts)
	checkResponse(f.t, resp)
	return resp, f.latest()
}

func (f *fixture) latest() Latest {
	f.t.Helper()
	data, err := os.ReadFile(latestPath(f.home))
	if err != nil {
		f.t.Fatalf("latest.json: %v", err)
	}
	var l Latest
	if err := json.Unmarshal(data, &l); err != nil {
		f.t.Fatalf("latest.json: %v", err)
	}
	return l
}

// checkResponse asserts the response never blocks (O8) and is well formed.
func checkResponse(t *testing.T, r Response) {
	t.Helper()
	if r.Status != StatusPass && r.Status != StatusWarn {
		t.Fatalf("status = %q, want pass or warn", r.Status)
	}
	if r.Findings == nil {
		t.Fatal("findings is nil, want an array")
	}
	if n := len([]rune(r.Context)); n > contextLen {
		t.Fatalf("context has %d chars", n)
	}
	for _, fd := range r.Findings {
		if len([]rune(fd.Detail)) > detailLen {
			t.Fatalf("finding detail too long: %q", fd.Detail)
		}
	}
}

func jsonl(t *testing.T, records ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, r := range records {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// hourAgo is an in-window record time.
var hourAgo = now.Add(-time.Hour)

func user(text string, at time.Time, human bool) map[string]any {
	return userContent(text, at, human)
}

// userContent is a user record with any message content.
func userContent(content any, at time.Time, human bool) map[string]any {
	r := map[string]any{
		"type": "user", "timestamp": ts(at), "cwd": "/w",
		"message": map[string]any{"role": "user", "content": content},
	}
	if human {
		r["origin"] = map[string]any{"kind": "human"}
	}
	return r
}

type tool struct {
	name  string
	input map[string]any
}

func assistant(at time.Time, cwd string, tools ...tool) map[string]any {
	content := []any{}
	for i, tl := range tools {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    fmt.Sprintf("t%d-%d", at.UnixNano(), i),
			"name":  tl.name,
			"input": tl.input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": "ok"})
	}
	return map[string]any{
		"type": "assistant", "timestamp": ts(at), "cwd": cwd, "sessionId": "s",
		"message": map[string]any{"role": "assistant", "content": content},
	}
}

// noContent makes result leave the tool_result content out.
var noContent = new(struct{})

// result answers the first tool_use of an assistant record built by assistant with content.
func result(a map[string]any, content any) map[string]any {
	message, _ := a["message"].(map[string]any)
	blocks, _ := message["content"].([]any)
	first, _ := blocks[0].(map[string]any)
	id, _ := first["id"].(string)
	block := map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	if content == noContent {
		delete(block, "content")
	}
	return map[string]any{
		"type": "user", "timestamp": a["timestamp"], "cwd": a["cwd"],
		"message":       map[string]any{"role": "user", "content": []any{block}},
		"toolUseResult": "result",
	}
}

// errorResult is result marked is_error, as Claude Code records a hook deny.
func errorResult(a map[string]any, content any) map[string]any {
	r := result(a, content)
	message, _ := r["message"].(map[string]any)
	blocks, _ := message["content"].([]any)
	block, _ := blocks[0].(map[string]any)
	block["is_error"] = true
	return r
}

func signalItems(l Latest, signal string) []Item {
	var out []Item
	for _, it := range l.Items {
		if it.Signal == signal {
			out = append(out, it)
		}
	}
	return out
}

func findings(r Response, kind string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func excerpts(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Excerpt
	}
	slices.Sort(out)
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
