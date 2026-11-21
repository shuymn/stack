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
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// built holds the binaries TestMain builds: the real stack (replay, protocol) and, for the
// protocol test, stack-observe with the observetest tag.
var built struct {
	dir, stack, observe string
	err                 error
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
	built.observe = filepath.Join(dir, "stack-observe"+exe)
	env := gittest.CleanEnv(os.Environ())
	for _, c := range [][]string{
		{"build", "-o", built.stack, "../../cmd/stack"},
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

// fake installs a proctest fake as name.
func (f *fixture) fake(name string, responses ...proctest.Response) string {
	f.t.Helper()
	p := proctest.InstallAt(f.t, f.bin, name, proctest.Scenario{Responses: responses})
	f.tools[name] = p
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
	r := map[string]any{
		"type": "user", "timestamp": ts(at), "cwd": "/w",
		"message": map[string]any{"role": "user", "content": text},
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

// result answers the tool_use id of an assistant record built by assistant.
func result(a map[string]any, i int, content string) map[string]any {
	blocks, _ := a["message"].(map[string]any)["content"].([]any)
	id, _ := blocks[i].(map[string]any)["id"].(string)
	return map[string]any{
		"type": "user", "timestamp": a["timestamp"], "cwd": a["cwd"],
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": content},
		}},
		"toolUseResult": content,
	}
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
