package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// extBin makes a fixture repository the working directory and its git environment the process's,
// and puts a new directory first on PATH, whose path it returns. Not parallel: it changes PATH and
// the working directory.
func extBin(t *testing.T) (string, *gittest.Repo) {
	t.Helper()
	r := gittest.New(t)
	r.SetProcessEnv(t)
	r.Commit("first")
	t.Chdir(r.Dir)
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin, r
}

func fakeExt(t *testing.T, bin, name, stdout string) string {
	t.Helper()
	return proctest.InstallAt(t, bin, "stack-"+name,
		proctest.Scenario{Responses: []proctest.Response{{Stdout: stdout, Stderr: "note\n"}}})
}

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

func decodeAggregate(t *testing.T, r result) aggregate {
	t.Helper()
	var a aggregate
	if err := json.Unmarshal([]byte(r.stdout), &a); err != nil {
		t.Fatalf("stdout is not an aggregate: %v\n%+v", err, r)
	}
	return a
}

// TestRunNoExtensions covers P15: with nothing installed every stage passes and lists the
// configured names as skipped.
func TestRunNoExtensions(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for stage, skipped := range map[string]string{
		"entry":    `["jev-router"]`,
		"outbound": `["jev-semantic"]`,
		"cycle":    `["observe","jev-classify","improve"]`,
	} {
		r := run(t, nil, "run", stage)
		want := `{"stage":"` + stage + `","status":"pass","results":[],"skipped":` + skipped + "}\n"
		if r.code != exitOK || r.stdout != want || r.stderr != "" {
			t.Errorf("run %s = %+v, want %s", stage, r, want)
		}
	}
}

// TestRunCommand covers `stack run` end to end: request, statuses, exit codes and usage errors.
func TestRunCommand(t *testing.T) {
	bin, r := extBin(t)
	r.Git("branch", "base")
	r.Config("branch.main.remote", ".")
	r.Config("branch.main.merge", "refs/heads/base")
	block := fakeExt(t, bin, "blocker", `{"status":"block","context":"NO","findings":[{"k":1}]}`)
	fakeExt(t, bin, "passer", `{"status":"pass","context":"OK"}`)
	fakeExt(t, bin, "broken", `not json`)
	home := t.TempDir()
	env := map[string]string{"STACK_SESSION_ID": "s1"}

	res := runIn(t, home, env, "run", "outbound", "--ext", "passer", "--ext", "blocker",
		"--ext", "absent")
	a := decodeAggregate(t, res)
	if res.code != exitRefused || a.Status != "block" || len(a.Results) != 2 ||
		a.Results[0].Context != "OK" || a.Results[1].Status != "block" ||
		string(a.Results[1].Findings) != `[{"k":1}]` || len(a.Skipped) != 1 {
		t.Fatalf("block run = %+v", res)
	}
	if res.stderr != "[passer] note\n[blocker] note\nstack run: stage outbound: block\n" {
		t.Fatalf("stderr = %q", res.stderr)
	}
	calls := proctest.Calls(t, block)
	var req map[string]string
	if len(calls) != 1 || json.Unmarshal([]byte(calls[0].Stdin), &req) != nil {
		t.Fatalf("calls = %+v", calls)
	}
	if req["stage"] != "outbound" || req["range"] != "@{upstream}..HEAD" || req["prompt"] != "" ||
		!sameFile(t, req["repo"], r.Dir) || len(req) != 4 {
		t.Fatalf("request = %v", req)
	}

	res = runIn(t, home, env, "run", "cycle", "--ext", "broken", "--ext", "passer")
	a = decodeAggregate(t, res)
	if res.code != exitOK || a.Status != "warn" || a.Results[0].Status != "warn" ||
		!strings.Contains(string(a.Results[0].Findings), `"extension":"broken"`) {
		t.Fatalf("warn run = %+v", res)
	}

	// --range and --prompt-file reach the request.
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	writeFile(t, prompt, []byte("do the thing\n"))
	res = runIn(t, home, env, "run", "entry", "--ext", "blocker", "--range", "base..HEAD",
		"--prompt-file", prompt)
	calls = proctest.Calls(t, block)
	if res.code != exitRefused || len(calls) != 2 ||
		json.Unmarshal([]byte(calls[1].Stdin), &req) != nil ||
		req["stage"] != "entry" || req["range"] != "base..HEAD" || req["prompt"] != "do the thing\n" {
		t.Fatalf("entry run = %+v, request %v", res, req)
	}
}

// TestRunConfigNames covers names from `[stages]`; one that is not a single component is a config
// error.
func TestRunConfigNames(t *testing.T) {
	bin, _ := extBin(t)
	fakeExt(t, bin, "passer", `{"status":"pass","context":"OK"}`)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
		[]byte("[stages]\noutbound = [\"passer\"]\ncycle = [\"../x\"]\n"))
	if res := runIn(t, home, nil, "run", "outbound"); res.code != exitOK ||
		decodeAggregate(t, res).Results[0].Context != "OK" {
		t.Fatalf("configured run = %+v", res)
	}
	if res := runIn(t, home, nil, "run", "cycle"); res.code != exitUsage ||
		!strings.Contains(res.stderr, `[stages].cycle`) || res.stdout != "" {
		t.Fatalf("bad config name = %+v", res)
	}
}

// TestRunUsageErrors covers exit 2 before any extension runs: bad names, ranges and inputs.
func TestRunUsageErrors(t *testing.T) {
	bin, _ := extBin(t)
	block := fakeExt(t, bin, "blocker", `{"status":"block"}`)
	home := t.TempDir()
	nonUTF8 := filepath.Join(t.TempDir(), "bad.txt")
	writeFile(t, nonUTF8, []byte{0xff, 0xfe})
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--ext", "../x"}, `invalid extension name: "../x"`},
		{[]string{"--ext", "C:x"}, `invalid extension name: "C:x"`},
		{[]string{"--ext", "blocker", "--range", "nope..HEAD"}, `cannot resolve range: "nope..HEAD"`},
		{[]string{"--ext", "blocker", "--prompt-file", nonUTF8}, "not UTF-8"},
		{[]string{"--ext", "blocker", "--prompt-file", filepath.Join(bin, "none")}, "--prompt-file"},
		{[]string{"--replay", nonUTF8, "--range", "HEAD"}, "cannot be combined"},
		{[]string{"--ext", "absent", "--range="}, "--range: empty value"},
		{[]string{"--ext", "blocker", "--prompt-file="}, "--prompt-file: empty value"},
		{[]string{"--ext", "blocker", "--range", ""}, "--range: empty value"},
	} {
		args := append([]string{"run", "outbound"}, tt.args...)
		res := runIn(t, home, nil, args...)
		if res.code != exitUsage || res.stdout != "" || !strings.Contains(res.stderr, tt.want) {
			t.Errorf("%q = %+v, want exit 2 and %q", tt.args, res, tt.want)
		}
	}
	if res := runIn(t, home, nil, "run", "other"); res.code != exitUsage {
		t.Errorf("unknown stage = %+v", res)
	}
	if calls := proctest.Calls(t, block); len(calls) != 0 {
		t.Fatalf("an extension ran on a usage error: %+v", calls)
	}
}

// TestRunDryRun covers --dry-run: the plan and request are printed and nothing runs.
func TestRunDryRun(t *testing.T) {
	bin, r := extBin(t)
	block := fakeExt(t, bin, "blocker", `{"status":"block"}`)
	res := runIn(t, t.TempDir(), nil, "run", "entry", "--dry-run", "--ext", "blocker",
		"--ext", "absent")
	req, err := json.Marshal(ext.Request{
		Stage: "entry", Repo: strings.TrimSpace(r.Git("rev-parse", "--show-toplevel")),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "stage: entry\nplan:\n  blocker -> " + block + "\n  absent -> skipped\nrequest: " +
		string(req) + "\n"
	if res.code != exitOK || res.stdout != want {
		t.Fatalf("dry run = %+v\nwant %s", res, want)
	}
	if calls := proctest.Calls(t, block); len(calls) != 0 {
		t.Fatalf("dry run ran the extension: %+v", calls)
	}
}

// TestRunReplay covers C22: the fixture's request is sent verbatim, its expect is compared, and a
// stage mismatch is rejected.
func TestRunReplay(t *testing.T) {
	// Absolute before extBin changes the working directory.
	fixture, err := filepath.Abs(filepath.Join("..", "ext", "testdata", "outbound-block.json"))
	if err != nil {
		t.Fatal(err)
	}
	noExpect := filepath.Join(filepath.Dir(fixture), "outbound-no-expect.json")
	bin, _ := extBin(t)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	block := fakeExt(t, bin, "blocker", `{"status":"block"}`)
	res := runIn(t, home, nil, "run", "outbound", "--replay", fixture, "--ext", "blocker")
	if res.code != exitOK || decodeAggregate(t, res).Status != "block" {
		t.Fatalf("match = %+v, want status block and exit 0", res)
	}
	calls := proctest.Calls(t, block)
	if len(calls) != 1 || calls[0].Stdin != string(doc.Request) {
		t.Fatalf("stdin = %+v, want the fixture request %s", calls, doc.Request)
	}

	fakeExt(t, bin, "passer", `{"status":"pass"}`)
	res = runIn(t, home, nil, "run", "outbound", "--replay", fixture, "--ext", "passer")
	if res.code != exitRefused || decodeAggregate(t, res).Status != "pass" ||
		!strings.Contains(res.stderr, "got pass, want block") {
		t.Fatalf("status mismatch = %+v, want exit 1", res)
	}

	res = runIn(t, home, nil, "run", "entry", "--replay", fixture, "--ext", "blocker")
	if res.code != exitUsage || res.stdout != "" ||
		!strings.Contains(res.stderr, `"outbound", not "entry"`) {
		t.Fatalf("stage mismatch = %+v, want exit 2", res)
	}
	if n := len(proctest.Calls(t, block)); n != 1 {
		t.Fatalf("extension ran on a stage mismatch: %d calls", n)
	}

	// Without expect, the aggregate decides the exit code as for a normal run.
	res = runIn(t, home, nil, "run", "outbound", "--replay", noExpect, "--ext", "blocker")
	if res.code != exitRefused || decodeAggregate(t, res).Status != "block" {
		t.Fatalf("no expect = %+v, want exit 1", res)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	writeFile(t, bad, []byte(`{"request":[]}`))
	if res := runIn(t, home, nil, "run", "outbound", "--replay", bad); res.code != exitUsage ||
		!strings.Contains(res.stderr, "invalid replay file") {
		t.Fatalf("bad replay = %+v", res)
	}
}

// TestKitOpenEntryStage covers `kit open` running the entry stage and appending each context in
// the renderer's block format; block is shown, not enforced.
func TestKitOpenEntryStage(t *testing.T) {
	bin, r := extBin(t)
	router := fakeExt(t, bin, "jev-router", `{"status":"pass","context":"ROUTED"}`)
	gate := fakeExt(t, bin, "gate", `{"status":"block","context":"STOP"}`)
	fakeExt(t, bin, "quiet", `{"status":"pass"}`)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
		[]byte("[stages]\nentry = [\"jev-router\", \"gate\", \"quiet\", \"broken\", \"absent\"]\n"))
	writeFile(t, filepath.Join(bin, "stack-broken"), []byte("not runnable"))
	kitDir := filepath.Join(home, ".local", "state", "stack", "kits", "demo")

	res := runIn(t, home, map[string]string{"STACK_SESSION_ID": "s1"}, "kit", "open", "demo")
	want := "[new kit: " + kitDir + "; write state.md here]\n\n" +
		"<extension-context path=" + strconv.Quote(router) + ">\nROUTED\n</extension-context>\n\n" +
		"<extension-context path=" + strconv.Quote(gate) +
		">\n[extension gate: block (shown, not enforced)]\nSTOP\n</extension-context>\n\n" +
		"[extension broken: warn: "
	if res.code != exitOK || !strings.HasPrefix(res.stdout, want) ||
		!strings.Contains(res.stdout, "not runnable") || strings.Contains(res.stdout, "quiet") ||
		strings.Contains(res.stdout, "absent") {
		t.Fatalf("kit open = %+v\nwant prefix %s", res, want)
	}
	calls := proctest.Calls(t, router)
	var req map[string]string
	if len(calls) != 1 || json.Unmarshal([]byte(calls[0].Stdin), &req) != nil ||
		req["stage"] != "entry" || req["range"] != "" || req["prompt"] != "" ||
		!sameFile(t, req["repo"], r.Dir) {
		t.Fatalf("entry request = %+v", calls)
	}

	// A bad entry name is a config error before anything is recorded.
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
		[]byte("[stages]\nentry = [\"a/b\"]\n"))
	res = runIn(t, home, map[string]string{"STACK_SESSION_ID": "s2"}, "kit", "open", "other")
	if res.code != exitUsage || res.stdout != "" ||
		!strings.Contains(res.stderr, "[stages].entry") {
		t.Fatalf("bad entry name = %+v", res)
	}
	otherDir := filepath.Join(home, ".local", "state", "stack", "kits", "other")
	if _, err := os.Stat(otherDir); !os.IsNotExist(err) {
		t.Fatalf("kit dir created despite a config error: %v", err)
	}
}

// chunkWriter records each write and calls onWrite with the write's index before recording it.
type chunkWriter struct {
	chunks  []string
	onWrite func(i int)
}

func (w *chunkWriter) Write(b []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite(len(w.chunks))
	}
	w.chunks = append(w.chunks, string(b))
	return len(b), nil
}

// TestKitOpenWritesKitFirst covers `kit open` writing the kit before any entry extension starts
// and then each entry result as it completes, in the bytes of one joined render.
func TestKitOpenWritesKitFirst(t *testing.T) {
	bin, _ := extBin(t)
	first := fakeExt(t, bin, "first", `{"status":"pass","context":"ONE"}`)
	second := fakeExt(t, bin, "second", `{"status":"warn","context":"TWO"}`)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
		[]byte("[stages]\nentry = [\"first\", \"second\"]\n"))
	kitDir := filepath.Join(home, ".local", "state", "stack", "kits", "demo")

	// started[i] = calls of (first, second) when write i begins: none before the kit, and the
	// first block is written before the second extension starts.
	var started [][2]int
	stdout := &chunkWriter{onWrite: func(int) {
		started = append(
			started,
			[2]int{len(proctest.Calls(t, first)), len(proctest.Calls(t, second))},
		)
	}}
	var stderr strings.Builder
	code := Run(t.Context(), []string{"kit", "open", "demo"}, Env{
		Stdout: stdout,
		Stderr: &stderr,
		Getenv: func(k string) string { return map[string]string{"STACK_SESSION_ID": "s1"}[k] },
		Home:   home,
	})
	kitText := "[new kit: " + kitDir + "; write state.md here]\n"
	want := []string{
		kitText,
		"\n<extension-context path=" + strconv.Quote(first) + ">\nONE\n</extension-context>\n",
		"\n<extension-context path=" + strconv.Quote(second) +
			">\n[extension second: warn: warn]\nTWO\n</extension-context>\n",
	}
	wantStarted := [][2]int{{0, 0}, {1, 0}, {1, 1}}
	if code != exitOK || !slices.Equal(started, wantStarted) || !slices.Equal(stdout.chunks, want) {
		t.Fatalf("kit open = %d, started per write %v (want %v), chunks %q\nwant %q\nstderr %s",
			code, started, wantStarted, stdout.chunks, want, stderr.String())
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}
