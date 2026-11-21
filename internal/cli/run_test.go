package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// longStageDeadlines lifts the 10 s deadlines of entry and merged, meant for an interactive start,
// for fakes that start slowly under -race on a loaded machine; the deadline paths have their own
// tests. Not parallel: it changes a package variable.
func longStageDeadlines(t *testing.T) {
	t.Helper()
	for _, stage := range []string{ext.StageEntry, ext.StageMerged} {
		saved := ext.Timeouts[stage]
		ext.Timeouts[stage] = time.Minute
		t.Cleanup(func() { ext.Timeouts[stage] = saved })
	}
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
		"outbound": `["public","jev-semantic"]`,
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
		!sameFile(t, req["repo"], r.Dir) || req["title"] != "" || req["body"] != "" ||
		req["branch"] != "" || len(req) != 7 {
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
	var entryReq map[string]any
	if res.code != exitOK || len(calls) != 2 ||
		json.Unmarshal([]byte(calls[1].Stdin), &entryReq) != nil ||
		entryReq["stage"] != "entry" || entryReq["range"] != "base..HEAD" ||
		entryReq["prompt"] != "do the thing\n" {
		t.Fatalf("entry run = %+v, request %v", res, entryReq)
	}
}

// TestRunBlockExit covers C2: `stack run` exits 1 on block only at outbound, and a replay's
// expect is compared with the aggregate after demotion.
func TestRunBlockExit(t *testing.T) {
	longStageDeadlines(t)
	bin, _ := extBin(t)
	blocker := fakeExt(t, bin, "blocker", `{"status":"block","context":"NO"}`)
	home := t.TempDir()
	replay := func(stage, expect string) string {
		path := filepath.Join(t.TempDir(), stage+"-"+expect+".json")
		writeFile(t, path, []byte(`{"request":{"stage":"`+stage+`","repo":"","range":"",`+
			`"prompt":""},"expect":{"status":"`+expect+`"}}`))
		return path
	}
	for _, tt := range []struct {
		stage, status string
		code          int
	}{
		{"entry", "warn", exitOK},
		{"outbound", "block", exitRefused},
		{"merged", "warn", exitOK},
		{"cycle", "warn", exitOK},
	} {
		res := runIn(t, home, nil, "run", tt.stage, "--ext", "blocker")
		a := decodeAggregate(t, res)
		if res.code != tt.code || a.Status != tt.status || a.Results[0].Context != "NO" {
			t.Errorf("run %s = %+v, want %s and exit %d", tt.stage, res, tt.status, tt.code)
		}
		if tt.stage == "outbound" {
			continue
		}
		reason := "block is not allowed at stage " + tt.stage
		if a.Results[0].Error != reason ||
			!strings.Contains(string(a.Results[0].Findings), `"error":"`+reason+`"`) {
			t.Errorf("run %s result = %+v, want the demotion finding", tt.stage, a.Results[0])
		}
		res = runIn(t, home, nil, "run", tt.stage, "--ext", "blocker",
			"--replay", replay(tt.stage, "block"))
		if res.code != exitRefused || !strings.Contains(res.stderr, "got warn, want block") {
			t.Errorf("replay %s expecting block = %+v, want a mismatch", tt.stage, res)
		}
		res = runIn(t, home, nil, "run", tt.stage, "--ext", "blocker",
			"--replay", replay(tt.stage, "warn"))
		if res.code != exitOK || decodeAggregate(t, res).Status != "warn" {
			t.Errorf("replay %s expecting warn = %+v, want a match", tt.stage, res)
		}
	}
	// The outbound text flags are usage errors at any other stage and with --replay.
	text := filepath.Join(t.TempDir(), "text.md")
	writeFile(t, text, []byte("x\n"))
	calls := len(proctest.Calls(t, blocker))
	for _, flag := range [][]string{
		{"--title-file", text}, {"--body-file", text}, {"--branch", "b"}, {"--branch="},
	} {
		for _, stage := range []string{"entry", "merged", "cycle"} {
			res := runIn(
				t,
				home,
				nil,
				append([]string{"run", stage, "--ext", "blocker"}, flag...)...)
			if res.code != exitUsage || res.stdout != "" ||
				!strings.Contains(res.stderr, "only for the outbound stage") {
				t.Errorf("run %s %q = %+v, want exit 2", stage, flag, res)
			}
		}
		res := runIn(t, home, nil, append([]string{
			"run", "outbound", "--ext", "blocker",
			"--replay", replay("outbound", "block"),
		}, flag...)...)
		if res.code != exitUsage || res.stdout != "" ||
			!strings.Contains(res.stderr, "cannot be combined") {
			t.Errorf("replay with %q = %+v, want exit 2", flag, res)
		}
	}
	if n := len(proctest.Calls(t, blocker)); n != calls {
		t.Errorf("the extension ran on a usage error: %d calls, want %d", n, calls)
	}
}

// TestRunOutboundTexts covers C15: `stack run outbound` sends title, body and branch from
// --title-file, --body-file and --branch; each is "" when absent or explicitly empty, `~` is
// resolved against home, and a non-UTF-8 file exits 2 before any extension runs.
func TestRunOutboundTexts(t *testing.T) {
	bin, _ := extBin(t)
	logger := fakeExt(t, bin, "logger", `{"status":"pass"}`)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "title.txt"), []byte("docs: title\n"))
	writeFile(t, "body.md", []byte("Body \u00e9\n"))
	writeFile(t, "empty.md", nil)
	writeFile(t, "latin1.md", []byte("caf\xe9\n"))
	texts := func(args ...string) (map[string]any, result) {
		t.Helper()
		res := runIn(
			t,
			home,
			nil,
			append([]string{"run", "outbound", "--ext", "logger"}, args...)...)
		calls := proctest.Calls(t, logger)
		var req map[string]any
		if res.code != exitOK || json.Unmarshal([]byte(calls[len(calls)-1].Stdin), &req) != nil {
			t.Fatalf("run %q = %+v, calls %+v", args, res, calls)
		}
		return map[string]any{"title": req["title"], "body": req["body"], "branch": req["branch"]},
			res
	}
	for _, tt := range []struct {
		args []string
		want map[string]any
	}{
		{nil, map[string]any{"title": "", "body": "", "branch": ""}},
		{
			[]string{"--title-file", "~/title.txt", "--body-file", "body.md", "--branch", "kaizen/x"},
			map[string]any{"title": "docs: title\n", "body": "Body \u00e9\n", "branch": "kaizen/x"},
		},
		{
			[]string{"--title-file", "empty.md", "--body-file=empty.md", "--branch="},
			map[string]any{"title": "", "body": "", "branch": ""},
		},
	} {
		if got, _ := texts(tt.args...); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("run %q texts = %v, want %v", tt.args, got, tt.want)
		}
	}
	calls := len(proctest.Calls(t, logger))
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--body-file", "latin1.md"}, "--body-file: cannot read"},
		{[]string{"--title-file", "none.md"}, "--title-file: cannot read"},
		{[]string{"--title-file="}, "--title-file: empty value"},
	} {
		res := runIn(t, home, nil,
			append([]string{"run", "outbound", "--ext", "logger"}, tt.args...)...)
		if res.code != exitUsage || res.stdout != "" || !strings.Contains(res.stderr, tt.want) {
			t.Errorf("run %q = %+v, want exit 2 with %q", tt.args, res, tt.want)
		}
	}
	if n := len(proctest.Calls(t, logger)); n != calls {
		t.Errorf("the extension ran on an input error: %d calls, want %d", n, calls)
	}
}

// TestRequestKits covers C3: a live entry or merged request lists the session's open kits in
// `.kits` order, [] without a valid id or under a held lock; outbound and cycle requests carry no
// kits, and a live `stack run merged` request no merge field.
func TestRequestKits(t *testing.T) {
	longStageDeadlines(t)
	bin, _ := extBin(t)
	logger := fakeExt(t, bin, "logger", `{"status":"pass"}`)
	home := t.TempDir()
	root := filepath.Join(home, ".local", "state", "stack", "kits")
	withID := map[string]string{"STACK_SESSION_ID": "s1"}
	for _, slug := range []string{"b", "a", "gone"} {
		if res := runIn(t, home, withID, "kit", "open", slug); res.code != exitOK {
			t.Fatalf("kit open %s = %+v", slug, res)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "gone")); err != nil {
		t.Fatal(err)
	}
	// lastKits runs stage and returns the kits key of the request logger received.
	lastKits := func(env map[string]string, stage string) (json.RawMessage, bool, result) {
		t.Helper()
		res := runIn(t, home, env, "run", stage, "--ext", "logger")
		calls := proctest.Calls(t, logger)
		var req map[string]json.RawMessage
		if res.code != exitOK || json.Unmarshal([]byte(calls[len(calls)-1].Stdin), &req) != nil {
			t.Fatalf("run %s = %+v, calls %+v", stage, res, calls)
		}
		kits, ok := req["kits"]
		return kits, ok, res
	}

	want := `[{"slug":"b","dir":` + strconv.Quote(filepath.Join(root, "b")) + `},` +
		`{"slug":"a","dir":` + strconv.Quote(filepath.Join(root, "a")) + `}]`
	for _, stage := range []string{"entry", "merged"} {
		if kits, _, res := lastKits(withID, stage); string(kits) != want ||
			res.stderr != "[logger] note\n" {
			t.Fatalf("%s kits = %s (%+v), want %s", stage, kits, res, want)
		}
		for _, env := range []map[string]string{nil, {"STACK_SESSION_ID": "../x"}} {
			if kits, _, _ := lastKits(env, stage); string(kits) != "[]" {
				t.Errorf("%s kits with id %q = %s, want []", stage, env["STACK_SESSION_ID"], kits)
			}
		}
	}
	if calls := proctest.Calls(t, logger); strings.Contains(calls[len(calls)-1].Stdin, `"merge"`) {
		t.Errorf("stack run merged request = %s, want no merge field", calls[len(calls)-1].Stdin)
	}
	for _, stage := range []string{"outbound", "cycle"} {
		if kits, ok, _ := lastKits(withID, stage); ok {
			t.Errorf("%s request has kits %s", stage, kits)
		}
	}
	// A replayed entry request is sent byte for byte, its own kits, key order and spacing
	// included: nothing decodes and re-encodes it.
	fixture := filepath.Join(t.TempDir(), "entry.json")
	request := "{ \"kits\": [ {\"dir\": " + strconv.Quote(filepath.Join(root, "other")) +
		", \"slug\":\"other\"} ],\n  \"prompt\":\"\", \"range\": \"\",\"repo\":\"\", \"stage\": \"entry\" }"
	writeFile(t, fixture, []byte(`{"request":`+request+`}`))
	replayed := runIn(t, home, withID, "run", "entry", "--replay", fixture, "--ext", "logger")
	if calls := proctest.Calls(t, logger); replayed.code != exitOK ||
		calls[len(calls)-1].Stdin != request {
		t.Fatalf("replay = %+v, stdin %q, want %s", replayed, calls[len(calls)-1].Stdin, request)
	}

	holdKitsLock(t, home, "s1")
	kits, _, res := lastKits(withID, "entry")
	if string(kits) != "[]" ||
		!strings.Contains(res.stderr, "stack: s1.kits locked; kits not sent\n") {
		t.Fatalf("kits under a held lock = %s, %+v; want [] and the note", kits, res)
	}
	// The wait itself, without a fake's start: the 1 s lock timeout, not kit open's 5 s.
	var stderr strings.Builder
	start := time.Now()
	got := requestKits(t.Context(), Env{Home: home, Stderr: &stderr}, "s1", root)
	if elapsed := time.Since(start); got == nil || len(*got) != 0 ||
		elapsed < 900*time.Millisecond || elapsed >= 2*time.Second ||
		stderr.String() != "stack: s1.kits locked; kits not sent\n" {
		t.Fatalf("requestKits under a held lock = %v after %s, stderr %q", got, elapsed,
			stderr.String())
	}
}

// TestRunEntryBrokenConfig: with --ext, a config that cannot be loaded costs only the kits, as
// stack run did before entry requests carried them.
func TestRunEntryBrokenConfig(t *testing.T) {
	longStageDeadlines(t)
	bin, _ := extBin(t)
	demo := fakeExt(t, bin, "demo", `{"status":"pass"}`)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"), []byte("version = 2\n"))
	withID := map[string]string{"STACK_SESSION_ID": "s1"}

	res := runIn(t, home, withID, "run", "entry", "--ext", "demo")
	calls := proctest.Calls(t, demo)
	var req map[string]json.RawMessage
	if res.code != exitOK || decodeAggregate(t, res).Status != "pass" || len(calls) != 1 ||
		json.Unmarshal([]byte(calls[0].Stdin), &req) != nil || string(req["kits"]) != "[]" ||
		!strings.HasPrefix(res.stderr, "stack: ") ||
		!strings.Contains(res.stderr, "; kits not sent\n") {
		t.Fatalf("broken config run = %+v, calls %+v", res, calls)
	}
	res = runIn(t, home, withID, "run", "entry", "--ext", "demo", "--dry-run")
	if res.code != exitOK || !strings.Contains(res.stdout, `"kits":[]`) ||
		!strings.Contains(res.stderr, "; kits not sent\n") {
		t.Fatalf("broken config dry run = %+v", res)
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
	req := `{"stage":"entry","repo":` +
		strconv.Quote(strings.TrimSpace(r.Git("rev-parse", "--show-toplevel"))) +
		`,"range":"","prompt":"","kits":[]}`
	want := "stage: entry\nplan:\n  blocker -> " + block + "\n  absent -> skipped\nrequest: " +
		req + "\n"
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

// TestKitOpenEntryStage covers `kit open` running the entry stage with the session's kits and
// appending each context in the renderer's block format; a block is shown as a demoted warn.
func TestKitOpenEntryStage(t *testing.T) {
	bin, r := extBin(t)
	longStageDeadlines(t)
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
		">\n[extension gate: warn: block is not allowed at stage entry]\nSTOP\n" +
		"</extension-context>\n\n" +
		"[extension broken: warn: "
	if res.code != exitOK || !strings.HasPrefix(res.stdout, want) ||
		!strings.Contains(res.stdout, "not runnable") || strings.Contains(res.stdout, "quiet") ||
		strings.Contains(res.stdout, "absent") {
		t.Fatalf("kit open = %+v\nwant prefix %s", res, want)
	}
	calls := proctest.Calls(t, router)
	var req map[string]json.RawMessage
	var repo string
	if len(calls) != 1 || json.Unmarshal([]byte(calls[0].Stdin), &req) != nil ||
		json.Unmarshal(req["repo"], &repo) != nil || !sameFile(t, repo, r.Dir) ||
		string(req["stage"]) != `"entry"` || string(req["range"]) != `""` ||
		string(req["prompt"]) != `""` ||
		string(req["kits"]) != `[{"slug":"demo","dir":`+strconv.Quote(kitDir)+`}]` {
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
