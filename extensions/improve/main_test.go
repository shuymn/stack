package improve

import (
	"bytes"
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

// built holds the binaries TestMain builds: the real stack (protocol), stack-improve with the
// improvetest tag (I5, protocol) and, in a directory of its own that no test puts on PATH, the real
// stack-public (I10).
var built struct {
	dir, stack, improve, public string
	err                         error
}

func TestMain(m *testing.M) {
	runFakeSpec()
	proctest.Main()
	cleanup := buildBinaries()
	code := gittest.Main(m)
	cleanup()
	os.Exit(code)
}

func buildBinaries() func() {
	dir, err := os.MkdirTemp("", "stack-improve-test-")
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
	built.improve = filepath.Join(dir, "stack-improve"+exe)
	built.public = filepath.Join(dir, "public", "stack-public"+exe)
	if err := os.Mkdir(filepath.Dir(built.public), 0o700); err != nil {
		built.err = err
		return func() { _ = os.RemoveAll(dir) }
	}
	env := gittest.CleanEnv(os.Environ())
	for _, c := range [][]string{
		{"build", "-o", built.stack, "../../cmd/stack"},
		{"build", "-tags", "improvetest", "-o", built.improve, "./cmd/stack-improve"},
		{"build", "-o", built.public, "../public/cmd/stack-public"},
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

// needUnix skips tests that go past step 0: improve answers pass on Windows (design §3.4 step 0).
func needUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("improve runs on macOS and Linux")
	}
}

// fakeSpec is what a fake does besides answering from its proctest scenario: edit files in its
// working directory (fake claude), write the file named after a flag (fake codex's -o), copy the
// file named after a flag (fake gh's --body-file), print an answer that names the OID a branch
// has in the bare remote (fake gh's PR read), or kill itself after printing (fake claude).
type fakeSpec struct {
	Edits        []fakeEdit  `json:"edits,omitempty"`
	WriteAfter   string      `json:"writeAfter,omitempty"`
	WriteContent string      `json:"writeContent,omitempty"`
	CopyAfter    string      `json:"copyAfter,omitempty"`
	CopyTo       string      `json:"copyTo,omitempty"`
	Print        []fakePrint `json:"print,omitempty"`
	// EnvTo receives the fake's environment, one variable per line.
	EnvTo string `json:"envTo,omitempty"`
	// Kill makes the fake kill itself after its edits and prints, before proctest answers.
	Kill bool `json:"kill,omitempty"`
}

type fakeEdit struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Exec    bool   `json:"exec,omitempty"`
	Symlink string `json:"symlink,omitempty"`
	Remove  bool   `json:"remove,omitempty"`
	Rename  string `json:"rename,omitempty"`
	// NoRead leaves the file write-only.
	NoRead bool `json:"noRead,omitempty"`
	// Size extends the file, sparse, to this many bytes.
	Size int64 `json:"size,omitempty"`
}

type fakePrint struct {
	Contains string `json:"contains"`
	Stdout   string `json:"stdout"`
	// SHAFile replaces @@SHA@@ in Stdout with its trimmed contents.
	SHAFile string `json:"shaFile,omitempty"`
}

const specSuffix = ".fake.json"

// runFakeSpec applies the fake's spec when this binary runs as a fake that has one.
func runFakeSpec() {
	if os.Getenv("STACK_PROCTEST_GRANDCHILD") != "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	base := strings.TrimSuffix(exe, ".exe")
	data, err := os.ReadFile(base + specSuffix)
	if err != nil {
		return
	}
	var spec fakeSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		fmt.Fprintf(os.Stderr, "fake spec: %v\n", err)
		os.Exit(98)
	}
	if err := spec.apply(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "fake spec: %v\n", err)
		os.Exit(98)
	}
}

func after(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (s fakeSpec) apply(args []string) error {
	if s.EnvTo != "" {
		if err := os.WriteFile(
			s.EnvTo,
			[]byte(strings.Join(os.Environ(), "\n")),
			0o600,
		); err != nil {
			return fmt.Errorf("env: %w", err)
		}
	}
	for _, e := range s.Edits {
		if err := e.apply(); err != nil {
			return fmt.Errorf("edit %s: %w", e.Path, err)
		}
	}
	if p := after(args, s.WriteAfter); s.WriteAfter != "" && p != "" {
		//nolint:gosec // G703: the path is the fake's own argument in a test.
		if err := os.WriteFile(p, []byte(s.WriteContent), 0o600); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	// "-" is stdin, which proctest records in the call.
	if p := after(args, s.CopyAfter); s.CopyAfter != "" && p != "" && p != "-" {
		data, err := os.ReadFile(p) //nolint:gosec // G703: the fake's own argument in a test.
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		//nolint:gosec // G703: the capture path comes from the test.
		if err := os.WriteFile(s.CopyTo, data, 0o600); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}
	for _, pr := range s.Print {
		if !slices.ContainsFunc(
			args,
			func(a string) bool { return strings.Contains(a, pr.Contains) },
		) {
			continue
		}
		out := pr.Stdout
		if pr.SHAFile != "" {
			sha, _ := os.ReadFile(pr.SHAFile)
			out = strings.ReplaceAll(out, "@@SHA@@", strings.TrimSpace(string(sha)))
		}
		fmt.Print(out)
		break
	}
	if s.Kill {
		p, err := os.FindProcess(os.Getpid())
		if err == nil {
			err = p.Kill()
		}
		return fmt.Errorf("kill: %w", err)
	}
	return nil
}

// apply performs one edit relative to the working directory.
func (e fakeEdit) apply() error {
	p := filepath.FromSlash(e.Path)
	var err error
	switch {
	case e.Remove:
		err = os.Remove(p)
	case e.Rename != "":
		err = os.Rename(p, filepath.FromSlash(e.Rename))
	case e.Symlink != "":
		if err = os.Remove(p); err == nil {
			err = os.Symlink(e.Symlink, p)
		}
	default:
		mode := os.FileMode(0o644)
		if e.Exec {
			mode = 0o755
		}
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			err = os.WriteFile(p, []byte(e.Content), mode)
		}
		if e.NoRead {
			mode = 0o200
		}
		if err == nil && e.Size > 0 {
			err = os.Truncate(p, e.Size)
		}
		if err == nil {
			err = os.Chmod(p, mode)
		}
	}
	return err //nolint:wrapcheck // wrapped by the caller with the path
}

// now is the fixed wall clock: Wednesday of ISO week 2026-W47.
var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

const (
	week   = "2026-W47"
	branch = "improve/" + week
	slug   = "o/r"
	origin = "https://github.com/o/r.git"
	// token is the dummy endpoint credential.
	token = "dummy-token-0123456789"
)

// fx is one test HOME, a fixture repository with a bare origin, and fake tools.
type fx struct {
	t      *testing.T
	home   string
	repo   *gittest.Repo
	bare   string
	tools  map[string]string
	opts   Options
	req    Request
	stderr *bytes.Buffer
	// capture receives the body file gh pr create got, when it got a file.
	capture string
	// secrets must never appear in anything improve writes.
	secrets []string
}

func newFx(t *testing.T) *fx {
	t.Helper()
	needUnix(t)
	f := &fx{
		t: t, home: t.TempDir(), tools: map[string]string{}, stderr: &bytes.Buffer{},
		secrets: []string{token},
	}
	f.write(".config/stack/improve.toml", "enabled = true\n")
	r := gittest.New(t)
	f.repo = r
	r.Write(".stack/improve.toml", "paths = [\"skills/**\"]\n")
	r.Write("skills/a.md", "alpha\n")
	r.Write("skills/b.md", "beta\n")
	r.Write("skills/run.sh", "echo run\n")
	r.Write("skills2/x.md", "x\n")
	r.Write("other/c.md", "gamma\n")
	r.Git("add", "-A")
	r.Git("update-index", "--chmod=+x", "skills/run.sh")
	r.Git("commit", "-q", "-m", "base")
	f.bare = filepath.Join(r.Root, "origin.git")
	r.Git("clone", "-q", "--bare", r.Dir, f.bare)
	r.Git("remote", "add", "origin", origin)
	r.Config("url."+filepath.ToSlash(f.bare)+".insteadOf", origin)
	f.latest(now.Add(-time.Hour), publicItems())
	env := slices.DeleteFunc(slices.Clone(r.Env), func(kv string) bool {
		return strings.HasPrefix(kv, "HOME=")
	})
	env = append(env, "HOME="+f.home, "ANTHROPIC_AUTH_TOKEN="+token)
	f.opts = Options{
		Home: f.home, Env: env, LookPath: f.lookPath,
		Now: func() time.Time { return now }, Location: time.UTC, Stderr: f.stderr,
	}
	f.req = Request{Stage: "cycle", Repo: r.Dir}
	f.capture = filepath.Join(t.TempDir(), "body")
	f.fake("gh", f.ghSpec(), ghDefaults()...)
	f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "skills/a.md", Content: "alpha two\n"}}},
		proctest.Response{Stdout: claudeOut(change("e1"), 1.5, zeroUsage(7))})
	f.fake("codex", &fakeSpec{WriteAfter: "-o", WriteContent: `{"high":[]}`}, proctest.Response{})
	f.fake("stack-public", nil, proctest.Response{})
	return f
}

var errNotFake = errors.New("not on the test PATH")

func (f *fx) lookPath(name string) (string, error) {
	if p, ok := f.tools[name]; ok {
		return p, nil
	}
	if name == "git" {
		p, err := exec.LookPath("git")
		if err != nil {
			return "", fmt.Errorf("git: %w", err)
		}
		return p, nil
	}
	return "", fmt.Errorf("%s: %w", name, errNotFake)
}

// fake installs a proctest fake named name in a new directory, replacing an earlier one.
func (f *fx) fake(name string, spec *fakeSpec, responses ...proctest.Response) string {
	f.t.Helper()
	p := proctest.InstallAt(f.t, f.t.TempDir(), name, proctest.Scenario{Responses: responses})
	if spec != nil {
		data, err := json.Marshal(spec)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(strings.TrimSuffix(p, ".exe")+specSuffix, data, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	f.tools[name] = p
	return p
}

func (f *fx) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func publicItems() []map[string]any {
	return []map[string]any{
		{
			"id":         "e1",
			"signal":     "revert",
			"kind":       "miss",
			"where":      "https://github.com/o/r/pull/7",
			"excerpt":    "Revert a rule",
			"provenance": "public",
			"repo":       slug,
			"ts":         iso(now),
		},
		{
			"id": "p1", "signal": "owner-reply", "kind": "miss", "where": "/w/private.jsonl:3",
			"excerpt": "private note", "provenance": "owner", "repo": "", "ts": iso(now),
		},
		{
			"id": "u1", "signal": "fix-pr", "kind": "miss", "where": "https://example.com/pull/1",
			"excerpt": "not a GitHub URL", "provenance": "public", "repo": slug, "ts": iso(now),
		},
	}
}

// latest writes observe's latest.json with the window ending at until.
func (f *fx) latest(until time.Time, items []map[string]any) {
	f.t.Helper()
	doc := map[string]any{
		"version": 1,
		"window": map[string]any{
			"since": iso(until.Add(-7 * 24 * time.Hour)),
			"until": iso(until),
		},
		"sources": map[string]any{"gh": map[string]any{"status": "ok", "repos_read": 1}},
		"tally":   map[string]any{"owner": 0, "synthetic": 0, "unknown": 0},
		"items":   items,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(".local/state/stack/observe/latest.json", string(data))
}

var (
	argsRepoView = []string{
		"repo",
		"view",
		"github.com/" + slug,
		"--json",
		"nameWithOwner,visibility,defaultBranchRef",
	}
	argsOpenPRs = []string{
		"api",
		"--hostname",
		"github.com",
		"--paginate",
		"--slurp",
		"repos/o/r/pulls?state=open&per_page=100",
	}
	argsReadPRs = []string{
		"api",
		"--hostname",
		"github.com",
		"repos/o/r/pulls?state=all&head=o:" + branch + "&per_page=100",
	}
)

const repoView = `{"nameWithOwner":"o/r","visibility":"PUBLIC","defaultBranchRef":{"name":"main"}}`

func ghDefaults() []proctest.Response {
	return []proctest.Response{
		{Args: argsRepoView, Stdout: repoView},
		{Args: argsOpenPRs, Stdout: "[[]]"},
		{Args: argsReadPRs},
		{Args: nil, Stdout: "https://github.com/o/r/pull/1\n"},
	}
}

// prJSON is one REST pull request.
func prJSON(number int, headRepo, base string) string {
	return fmt.Sprintf(`{"number":%d,"html_url":"https://github.com/o/r/pull/%d",`+
		`"created_at":"2026-11-17T00:00:00Z","head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},`+
		`"base":{"ref":%q}}`, number, number, branch, "@@SHA@@", headRepo, base)
}

// ghSpec answers the PR read with one PR at whatever OID the branch has in the bare remote, and
// captures the body file of gh pr create.
func (f *fx) ghSpec() *fakeSpec {
	return f.ghSpecWith("[" + prJSON(1, slug, "main") + "]")
}

func (f *fx) ghSpecWith(read string) *fakeSpec {
	return &fakeSpec{
		CopyAfter: "--body-file", CopyTo: f.capture,
		Print: []fakePrint{{
			Contains: "state=all", Stdout: read,
			SHAFile: filepath.Join(f.bare, "refs", "heads", "improve", week),
		}},
	}
}

func change(ids ...string) map[string]any {
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{
		"change": true, "evidence_ids": ids, "summary": "tighten a rule", "watch": "reverts",
		"commit_message": "docs: tighten a rule", "pr_title": "docs: tighten a rule",
		"pr_body": "Tighten a rule.",
	}
}

func zeroUsage(n int) map[string]any {
	return map[string]any{
		"input_tokens": n, "output_tokens": n, "cache_creation_input_tokens": 0,
		"cache_read_input_tokens": 0,
	}
}

func claudeOut(out map[string]any, cost any, usage any) string {
	r := map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "structured_output": out,
	}
	if cost != nil {
		r["total_cost_usd"] = cost
	}
	if usage != nil {
		r["usage"] = usage
	}
	data, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (f *fx) run() Response {
	f.t.Helper()
	resp := Run(context.Background(), f.opts, f.req)
	checkResponse(f.t, resp)
	for _, secret := range f.secrets {
		if strings.Contains(f.stderr.String(), secret) || strings.Contains(resp.Context, secret) ||
			strings.Contains(fmt.Sprint(resp.Findings), secret) {
			f.t.Fatalf("a credential leaked: response %+v\nstderr %s", resp, f.stderr)
		}
		if st, ok := f.stampRaw(); ok && strings.Contains(st, secret) {
			f.t.Fatalf("a credential leaked into the stamp: %s", st)
		}
	}
	return resp
}

func checkResponse(t *testing.T, r Response) {
	t.Helper()
	if r.Status != StatusPass && r.Status != StatusWarn {
		t.Fatalf("status = %q", r.Status)
	}
	if r.Findings == nil {
		t.Fatal("findings is nil")
	}
}

// createBody returns the body gh pr create got: its stdin for `--body-file -`, else the file.
func (f *fx) createBody() string {
	f.t.Helper()
	for _, c := range f.calls("gh") {
		if len(c.Args) < 2 || c.Args[0] != "pr" || c.Args[1] != "create" {
			continue
		}
		if c.Args[len(c.Args)-1] == "-" {
			return c.Stdin
		}
		data, err := os.ReadFile(f.capture)
		if err != nil {
			f.t.Fatalf("gh pr create body: %v", err)
		}
		return string(data)
	}
	f.t.Fatal("no gh pr create")
	return ""
}

func (f *fx) stampPath() string {
	return filepath.Join(f.home, ".local", "state", "stack", "improve", week)
}

func (f *fx) stampRaw() (string, bool) {
	data, err := os.ReadFile(f.stampPath())
	if err != nil {
		return "", false
	}
	return string(data), true
}

func (f *fx) stamp() stamp {
	f.t.Helper()
	data, err := os.ReadFile(f.stampPath())
	if err != nil {
		f.t.Fatalf("stamp: %v", err)
	}
	var s stamp
	if err := json.Unmarshal(data, &s); err != nil {
		f.t.Fatalf("stamp: %v", err)
	}
	return s
}

func (f *fx) noStamp() {
	f.t.Helper()
	if _, err := os.Lstat(f.stampPath()); !errors.Is(err, os.ErrNotExist) {
		raw, _ := f.stampRaw()
		f.t.Fatalf("stamp exists: %s", raw)
	}
}

func (f *fx) calls(name string) []proctest.Call {
	f.t.Helper()
	return proctest.Calls(f.t, f.tools[name])
}

// ghCalls returns the gh calls whose first arguments are prefix.
func (f *fx) ghCalls(prefix ...string) int {
	n := 0
	for _, c := range f.calls("gh") {
		if len(c.Args) >= len(prefix) && slices.Equal(c.Args[:len(prefix)], prefix) {
			n++
		}
	}
	return n
}

// remote returns the bare remote's refs.
func (f *fx) remote() string {
	f.t.Helper()
	return f.repo.Git("ls-remote", f.bare)
}

func (f *fx) worktreeDir() string {
	return filepath.Join(f.home, ".local", "state", "stack", "improve", "work", week)
}

func (f *fx) wantOutcome(resp Response, status, outcome string) stamp {
	f.t.Helper()
	st := f.stamp()
	if resp.Status != status || st.Outcome != outcome {
		f.t.Fatalf("status %q outcome %q (reason %q), want %q %q\ncontext %s\nstderr %s",
			resp.Status, st.Outcome, st.Reason, status, outcome, resp.Context, f.stderr)
	}
	return st
}

func hasFinding(r Response, kind string) bool {
	return slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Kind == kind })
}

// noPublish asserts nothing reached the remote or gh pr create.
func (f *fx) noPublish() {
	f.t.Helper()
	if strings.Contains(f.remote(), "improve/") {
		f.t.Fatalf("the remote has an improve branch: %s", f.remote())
	}
	if n := f.ghCalls("pr", "create"); n != 0 {
		f.t.Fatalf("gh pr create ran %d times", n)
	}
}

// writeHook writes an executable shell hook.
func writeHook(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	//nolint:gosec // G306: a hook must be executable.
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
