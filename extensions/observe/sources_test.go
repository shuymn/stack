package observe

import (
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

const (
	fixtureRepo = "o/r"
	ghDay       = "2026-11-14"
	ghVersionOK = "gh version 2.105.0 (2026-11-17)\n"
)

func ghView(repo, visibility string) proctest.Response {
	return proctest.Response{
		Args:   []string{"repo", "view", repo, "--json", "visibility,defaultBranchRef"},
		Stdout: `{"visibility":"` + visibility + `","defaultBranchRef":{"name":"main"}}`,
	}
}

func ghPRs(repo, stdout string) proctest.Response {
	return proctest.Response{
		Args: []string{
			"pr", "list", "-R", repo, "--state", "merged", "--search", "merged:>=" + ghDay,
			"--limit", "200", "--json", "number,title,body,mergedAt,url",
		},
		Stdout: stdout,
	}
}

func ghRuns(repo, stdout string) proctest.Response {
	return proctest.Response{
		Args: []string{
			"run",
			"list",
			"-R",
			repo,
			"--branch",
			"main",
			"--status",
			"failure",
			"--created",
			">=" + ghDay,
			"--limit",
			"100",
			"--json",
			"displayTitle,createdAt,url,workflowName",
		},
		Stdout: stdout,
	}
}

var (
	ghAuthOK   = proctest.Response{Args: []string{"auth", "status"}}
	ghVersion  = proctest.Response{Args: []string{"--version"}, Stdout: ghVersionOK}
	stackModel = proctest.Response{
		Args: []string{
			"--version",
			"--json",
		},
		Stdout: `{"version":"dev","gh_model":"v2.105.0"}` + "\n",
	}
)

// withRepo makes the transcripts name f.cwd and a fake git report its origin as repo.
func (f *fixture) withRepo(repo string) {
	f.t.Helper()
	f.fake("git", proctest.Response{Stdout: "git@github.com:" + repo + ".git\n"})
	f.transcript("repo", assistant(hourAgo, f.cwd, editTool))
}

func TestSourceIsolation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.withRepo(fixtureRepo)
	f.transcript(
		"s1",
		user("task", hourAgo, true),
		assistant(hourAgo, "/w"),
		user("no, the other", hourAgo, true),
	)
	target := f.write("target.jsonl", "")
	link := filepath.Join(f.home, ".claude", "projects", "-p", "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f.write(".t3/userdata/statev2.sqlite", "")
	f.fake("sqlite3", proctest.Response{Stderr: "Error: database is locked\n", Exit: 1})
	f.fake("gh", ghAuthOK, ghView(fixtureRepo, "PUBLIC"),
		ghPRs(fixtureRepo, `[{"number":5,"title":"Revert x","body":"","mergedAt":"`+ts(hourAgo)+
			`","url":"https://github.com/o/r/pull/5"}]`),
		ghRuns(fixtureRepo, "[]"), ghVersion)
	f.fake("stack", stackModel)

	resp, l := f.run()
	if resp.Status != StatusWarn {
		t.Errorf("status = %q, want warn", resp.Status)
	}
	tr := l.Sources["transcripts"]
	if tr.Status != srcFail || !strings.Contains(tr.Error, "link.jsonl") ||
		!strings.Contains(tr.Error, "not a regular file") {
		t.Errorf("transcripts = %+v, want fail listing the link", tr)
	}
	if l.Sources["t3"].Status != srcFail || !strings.Contains(l.Sources["t3"].Error, "locked") {
		t.Errorf("t3 = %+v, want fail", l.Sources["t3"])
	}
	if got := excerpts(
		signalItems(l, "owner-reply"),
	); !slices.Equal(
		got,
		[]string{"no, the other"},
	) {
		t.Errorf("owner-reply = %q: transcript items lost after the T3 failure", got)
	}
	if gh := l.Sources["gh"]; gh.Status != srcOK || *gh.ReposRead != 1 {
		t.Errorf("gh = %+v, want ok with one repo read", gh)
	}
	if got := signalItems(
		l,
		"revert",
	); len(got) != 1 || got[0].Provenance != "public" ||
		got[0].Repo != fixtureRepo {
		t.Errorf("revert = %+v", got)
	}
	if l.Sources["codex"].Status != srcOff || l.Sources["ledgers"].Status != srcOff {
		t.Errorf("absent sources not off: %+v %+v", l.Sources["codex"], l.Sources["ledgers"])
	}
}

func TestGhFailureKeepsReposRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fake("git", proctest.Response{
		Args:   []string{"-C", f.cwd, "config", "--get", "remote.origin.url"},
		Stdout: "https://github.com/o/a.git\n",
	},
		proctest.Response{Stdout: "https://github.com/o/b\n"})
	other := t.TempDir()
	f.transcript("a1", assistant(hourAgo, f.cwd, editTool))
	f.transcript("a2", assistant(hourAgo, f.cwd, editTool))
	f.transcript("b1", assistant(hourAgo, other, editTool))
	f.fake(
		"gh",
		ghAuthOK,
		ghView("o/a", "PRIVATE"),
		ghPRs(
			"o/a",
			`[{"number":1,"title":"Revert y","mergedAt":"`+ts(
				hourAgo,
			)+`","url":"https://github.com/o/a/pull/1"}]`,
		),
		ghRuns("o/a", "[]"),
		proctest.Response{
			Args:   []string{"repo", "view", "o/b", "--json", "visibility,defaultBranchRef"},
			Stderr: "HTTP 502\n", Exit: 1,
		},
		ghVersion,
	)
	f.fake("stack", stackModel)
	resp, l := f.run()
	gh := l.Sources["gh"]
	if gh.Status != srcFail || *gh.ReposRead != 1 || !strings.Contains(gh.Error, "HTTP 502") {
		t.Fatalf("gh = %+v, want fail after one repo", gh)
	}
	if got := signalItems(l, "revert"); len(got) != 1 || got[0].Provenance != "synthetic" {
		t.Errorf("items of the repo read before the error = %+v", got)
	}
	if resp.Status != StatusWarn {
		t.Errorf("status = %q", resp.Status)
	}
}

func TestGhAuthFinding(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fake(
		"gh",
		proctest.Response{Args: []string{"auth", "status"}, Stderr: "not logged in\n", Exit: 1},
		ghVersion,
	)
	f.fake("stack", stackModel)
	resp, l := f.run()
	if got := findings(
		resp,
		kindGhAuth,
	); len(got) != 1 ||
		!strings.Contains(got[0].Detail, "not logged in") {
		t.Fatalf("gh-auth findings = %+v", got)
	}
	if resp.Status != StatusWarn || l.Sources["gh"].Status != srcOK {
		t.Errorf("status %q, gh %+v", resp.Status, l.Sources["gh"])
	}
}

// TestOriginRepoRealGit reads the origin of a real fixture repository.
func TestOriginRepoRealGit(t *testing.T) {
	t.Parallel()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	r := gittest.New(t)
	r.Config("remote.origin.url", "https://github.com/Some-Owner/some.repo.git")
	f := newFixture(t)
	f.opts.Env = r.Env
	s := &state{o: f.opts.withDefaults(), work: t.Context()}
	if got := s.originRepo(git, r.Dir); got != "Some-Owner/some.repo" {
		t.Fatalf("originRepo = %q", got)
	}
	if got := s.originRepo(git, filepath.Join(r.Root, "missing")); got != "" {
		t.Fatalf("originRepo of a missing dir = %q", got)
	}
}

func TestWindow(t *testing.T) {
	t.Parallel()
	since := now.Add(-window)
	f := newFixture(t)
	// Boundary records: since counts, until and before since do not.
	f.transcript("bounds",
		user("task", since.Add(time.Hour), true), assistant(since.Add(time.Hour), "/w"),
		user("at since", since, true), user("at until", now, true),
		user("before since", since.Add(-time.Second), true))
	// Appended after until: the file's mtime is past until and its in-window records still count.
	late := f.transcript("late", user("task", hourAgo, true), assistant(hourAgo, "/w"),
		user("appended reply", hourAgo, true), user("after until", now.Add(time.Minute), true))
	if err := os.Chtimes(late, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A file last written before since is not opened even when it holds in-window records.
	old := f.transcript(
		"old",
		user("task", hourAgo, true),
		assistant(hourAgo, "/w"),
		user("stale file", hourAgo, true),
	)
	if err := os.Chtimes(old, since.Add(-time.Minute), since.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// An unterminated last line is still being written.
	partial := jsonl(t, user("task", hourAgo, true), assistant(hourAgo, "/w"))
	tail := strings.TrimSuffix(jsonl(t, user("unterminated", hourAgo, true)), "\n")
	f.write(".claude/projects/-p/partial.jsonl", partial+tail)
	_, l := f.run()
	got := excerpts(signalItems(l, "owner-reply"))
	want := []string{"appended reply", "at since"}
	if !slices.Equal(got, want) {
		t.Fatalf("owner-reply = %q, want %q", got, want)
	}
	if st := l.Sources["transcripts"]; st.Status != srcOK || st.Files != 3 {
		t.Errorf("transcripts = %+v, want ok with 3 files", st)
	}
	if l.Window.Since != "2026-11-14T00:00:00Z" || l.Window.Until != "2026-11-21T00:00:00Z" {
		t.Errorf("window = %+v", l.Window)
	}
}

func TestWindowUnreadable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs and symlinks need Unix")
	}
	f := newFixture(t)
	f.transcript("ok", user("task", hourAgo, true))
	dir := filepath.Join(f.home, ".claude", "projects", "-p")
	if err := mkfifo(filepath.Join(dir, "stuck.jsonl")); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.Symlink(
		filepath.Join(dir, "ok.jsonl"),
		filepath.Join(dir, "link.jsonl"),
	); err != nil {
		t.Fatal(err)
	}
	done := make(chan Latest, 1)
	go func() {
		_, l := f.run()
		done <- l
	}()
	select {
	case l := <-done:
		st := l.Sources["transcripts"]
		if st.Status != srcFail || st.Files != 1 || !strings.Contains(st.Error, "2 unreadable") ||
			!strings.Contains(
				st.Error,
				"stuck.jsonl",
			) || !strings.Contains(st.Error, "link.jsonl") {
			t.Fatalf("transcripts = %+v", st)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("observe blocked on a FIFO")
	}
}

func TestSignals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.withRepo(fixtureRepo)
	// owner-reply: positive after an assistant turn; negatives: the first utterance, over 300
	// characters, not typed by a human, a command.
	f.transcript(
		"s1",
		user("first utterance", hourAgo, true),
		assistant(hourAgo, "/w"),
		user("short reply", hourAgo, true),
		user(strings.Repeat("x", 301), hourAgo, true),
		user(
			"injected",
			hourAgo,
			false,
		),
		user("<command-name>/clear</command-name>", hourAgo, true),
		// meta-effort: positive names review-ledgers/; negative names only the retired budget hook.
		assistant(
			hourAgo,
			"/w",
			tool{"Bash", map[string]any{"command": "cat ~/.claude/review-ledgers/a.md"}},
		),
		assistant(
			hourAgo,
			"/w",
			tool{"Bash", map[string]any{"command": "python attention-budget.py status"}},
		),
		// reference-unread: read.md is read.
		assistant(
			hourAgo,
			"/w",
			tool{"Read", map[string]any{"file_path": "/h/.claude/skills/hiden/references/read.md"}},
		),
	)
	f.transcript(
		"s2",
		assistant(
			hourAgo,
			"/w",
			tool{"Bash", map[string]any{"command": "python attention-budget.py"}},
		),
	)
	// hiden-in-worker: positive in a subagent; negative in a parent transcript (s3).
	f.write(".claude/projects/-p/s1/subagents/agent-w.jsonl",
		jsonl(t, assistant(hourAgo, "/w", tool{"Skill", map[string]any{"skill": "hiden"}})))
	f.transcript("s3", assistant(hourAgo, "/w", tool{"Skill", map[string]any{"skill": "hiden"}}))
	f.write(".claude/skills/hiden/references/read.md", "x")
	f.write(".claude/skills/hiden/references/unread.md", "x")
	// ledger-miss: positive; negatives: no miss, a day before the window.
	//nolint:gosmopolitan // ledger lines record a miss as 見逃し
	f.write(".claude/review-ledgers/x.md", "- 2026-11-20 r: 見逃し 1\n- 2026-11-19 r2: 見逃し 0\n"+
		"- 2026-11-13 r3: 見逃し 2\n- 2026-11-18 r4: no misses\n")
	pr := func(n, title, body, url string) string {
		return `{"number":` + n + `,"title":"` + title + `","body":"` + body + `","mergedAt":"` + ts(
			hourAgo,
		) +
			`","url":"` + url + `"}`
	}
	const u5, u6, u7 = "https://github.com/o/r/pull/5", "https://github.com/o/r/pull/6", "https://github.com/o/r/pull/7"
	f.fake(
		"gh",
		ghAuthOK,
		ghView(fixtureRepo, "PUBLIC"),
		ghPRs(fixtureRepo, "["+strings.Join([]string{
			// revert positive; fix-pr positive (#5 merged in the window), negatives #99 and o/x#5.
			pr("5", "Revert x", "", u5),
			pr("6", "Revert z", "fixes #5 and #99, fixes o/x#5", u6),
			pr("7", "feat: y", "unrelated", u7),
		}, ",")+"]"),
		ghRuns(
			fixtureRepo,
			`[{"displayTitle":"t","createdAt":"`+ts(
				hourAgo,
			)+`","url":"https://github.com/o/r/actions/runs/1","workflowName":"CI"},`+
				`{"displayTitle":"t again","createdAt":"`+ts(
				hourAgo,
			)+`","url":"https://github.com/o/r/actions/runs/1","workflowName":"CI"},`+
				`{"displayTitle":"late","createdAt":"`+ts(
				now,
			)+`","url":"https://github.com/o/r/actions/runs/2","workflowName":"CI"}]`,
		),
		ghVersion,
	)
	f.fake("stack", stackModel)

	_, l := f.run()
	want := map[string][]string{
		"owner-reply":  {"short reply"},
		"revert":       {"Revert x", "Revert z"},
		"fix-pr":       {"Revert z (fixes #5)"},
		"ci-fail-main": {"CI: t"},
		"ledger-miss":  {"- 2026-11-20 r: 見逃し 1"}, //nolint:gosmopolitan // ledger text
		"meta-effort": {
			"1 meta tool calls; first: Bash {\"command\":\"cat ~/.claude/review-ledgers/a.md\"}",
		},
		"hiden-in-worker":  {"Skill {\"skill\":\"hiden\"}"},
		"reference-unread": {"references/unread.md not read in the window"},
	}
	for signal, w := range want {
		if got := excerpts(signalItems(l, signal)); !slices.Equal(got, w) {
			t.Errorf("%s = %q, want %q", signal, got, w)
		}
	}
	// One where, two signals: both kept (revert and fix-pr of PR 6); the duplicate run once.
	var u6Signals []string
	for _, it := range l.Items {
		if it.Where == u6 {
			u6Signals = append(u6Signals, it.Signal)
		}
		if it.ID != itemID(it.Signal, it.Where) || len(it.ID) != 12 {
			t.Errorf("item id = %q for %s %s", it.ID, it.Signal, it.Where)
		}
	}
	slices.Sort(u6Signals)
	if !slices.Equal(u6Signals, []string{"fix-pr", "revert"}) {
		t.Errorf("signals at %s = %q", u6, u6Signals)
	}
	if got := signalItems(l, "owner-reply"); got[0].Provenance != "owner" {
		t.Errorf("owner-reply provenance = %q", got[0].Provenance)
	}
}

func TestReferenceUnreadNeedsTranscripts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(".claude/skills/hiden/references/unread.md", "x")
	_, l := f.run() // no projects directory: transcripts off
	if got := signalItems(l, "reference-unread"); len(got) != 0 {
		t.Fatalf("reference-unread with transcripts %s: %+v", l.Sources["transcripts"].Status, got)
	}
	f.transcript("ok", user("task", hourAgo, true))
	target := f.write("target.jsonl", "")
	if err := os.Symlink(
		target,
		filepath.Join(f.home, ".claude", "projects", "-p", "l.jsonl"),
	); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, l = f.run()
	if l.Sources["transcripts"].Status != srcFail || len(signalItems(l, "reference-unread")) != 0 {
		t.Fatalf("reference-unread with transcripts fail: %+v", l.Items)
	}
}

func TestCodexAndLedgers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write(".codex/sessions/2026/11/20/rollout-a.jsonl",
		`{"payload":{"timestamp":"`+ts(hourAgo)+`","cwd":"`+filepathJSON(f.cwd)+`"}}`+"\n{}\n")
	f.write(".codex/sessions/2026/11/20/rollout-old.jsonl",
		`{"payload":{"timestamp":"2026-11-01T00:00:00Z","cwd":"/other"}}`+"\n")
	f.write(".codex/sessions/2026/11/20/other.jsonl", "{}\n")
	// Local zone ledger days: with UTC+9 the window starts at 09:00 on Nov 14 local.
	f.opts.Location = time.FixedZone("plus9", 9*3600)
	//nolint:gosmopolitan // ledger lines record a miss as 見逃し
	f.write(".claude/review-ledgers/x.md", "- 2026-11-14 a: 見逃し 1\n- 2026-11-15 b: 見逃し 1\n"+
		"- 2026-11-21 c: 見逃し 1\n- 2026-11-20 d: 見逃し　なし\n")
	f.fake("git", proctest.Response{Stdout: "https://github.com/o/codex\n"})
	f.fake(
		"gh",
		ghAuthOK,
		ghView("o/codex", "PUBLIC"),
		ghPRs("o/codex", "[]"),
		ghRuns("o/codex", "[]"),
		ghVersion,
	)
	f.fake("stack", stackModel)
	_, l := f.run()
	if st := l.Sources["codex"]; st.Status != srcOK || st.Files != 2 || st.Records != 2 {
		t.Errorf("codex = %+v", st)
	}
	if *l.Sources["gh"].ReposRead != 1 {
		t.Errorf("codex cwd did not reach gh: %+v", l.Sources["gh"])
	}
	got := excerpts(signalItems(l, "ledger-miss"))
	//nolint:gosmopolitan // ledger text
	if !slices.Equal(got, []string{"- 2026-11-15 b: 見逃し 1", "- 2026-11-21 c: 見逃し 1"}) {
		t.Errorf("ledger-miss = %q", got)
	}
}

func filepathJSON(p string) string {
	return strings.ReplaceAll(p, `\`, `\\`)
}

// editTool makes an assistant record that is parsed (a text-only turn is skipped unparsed, so its
// cwd is not read; legacy observe.py:133-135).
var editTool = tool{"Edit", map[string]any{"file_path": "x"}}
