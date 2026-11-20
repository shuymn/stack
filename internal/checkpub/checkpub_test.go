package checkpub

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestMain(m *testing.M) {
	proctest.Main()
	os.Exit(m.Run())
}

// local builds an absolute local path with a trailing slash at run time, so this file publishes no
// literal one: local("Users", "a").
func local(parts ...string) string {
	return "/" + strings.Join(parts, "/") + "/"
}

// repo is a temporary git repository isolated from the user's and system git config.
type repo struct {
	t   *testing.T
	dir string
	env []string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	global := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+global,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	r := &repo{t: t, dir: root, env: env}
	r.git("-c", "core.autocrlf=false", "-c", "init.defaultBranch=main", "init", "-q", dir)
	r.dir = dir
	r.git("config", "core.autocrlf", "false")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), time.Minute)
	defer cancel()
	res, err := proc.Exec{}.Run(ctx, proc.Cmd{Name: "git", Args: args, Dir: r.dir, Env: r.env})
	if err != nil || res.ExitCode != 0 || res.TimedOut || res.WaitDelayExceeded || res.Truncated {
		r.t.Fatalf("git %q: %v exit %d: %s", args, err, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(string(res.Stdout))
}

func (r *repo) write(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// commit stages everything and commits, returning the short SHA that hits name.
func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")[:9]
}

// check runs Check over base..HEAD and returns its error and hit output.
func (r *repo) check(o Options) (string, error) {
	r.t.Helper()
	var out bytes.Buffer
	if o.Range == "" {
		o.Range = "base..HEAD"
	}
	o.Dir, o.Env, o.Runner, o.Out = r.dir, r.env, proc.Exec{}, &out
	err := Check(r.t.Context(), o)
	return out.String(), err
}

// start makes the base commit and tags it `base`.
func (r *repo) start(files map[string]string) {
	r.t.Helper()
	for name, content := range files {
		r.write(name, content)
	}
	r.commit("chore: base")
	r.git("tag", "base")
}

func assertHits(t *testing.T, out string, err error, want ...string) {
	t.Helper()
	if !errors.Is(err, ErrFound) {
		t.Fatalf("err = %v, want ErrFound; out:\n%s", err, out)
	}
	got := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for i := range want {
		want[i] = "check public: " + want[i]
	}
	if !slices.Equal(got, want) {
		t.Errorf("hits:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func assertClean(t *testing.T, out string, err error) {
	t.Helper()
	if err != nil || out != "" {
		t.Fatalf("err = %v, out = %q; want clean", err, out)
	}
}

// cleanBase is a file whose hunk header, context line and removed line carry local paths: none of
// them is published anew by a commit that edits it.
func cleanBase() string {
	lines := make([]string, 0, 12)
	lines = append(lines, "see "+local("Users", "context")+"heading")
	for i := range 8 {
		lines = append(lines, "  filler "+string(rune('a'+i)))
	}
	lines = append(lines,
		"  context line mentioning "+local("Users", "carol", "old"),
		"  removed line with "+local("home", "dave", "old"),
		"  tail",
	)
	return strings.Join(lines, "\n") + "\n"
}

func TestRangeCleanPasses(t *testing.T) {
	r := newRepo(t)
	r.start(map[string]string{"home/dot_claude/CLAUDE.md": cleanBase()})
	r.write("home/dot_claude/CLAUDE.md", strings.Replace(cleanBase(),
		"  removed line with "+local("home", "dave", "old"),
		"  Build for aarch64-darwin on the personal role, under ~/.claude/.", 1))
	r.commit("docs: describe the role")
	r.write("b.md", "Edits home/dot_claude/CLAUDE.md for aarch64-darwin.\n")
	r.commit("fix: tidy")
	if diff := r.git("show", "--format=", "HEAD~1"); !strings.Contains(diff, "@@ see /") {
		t.Fatalf("hunk header does not carry the base path:\n%s", diff)
	}
	out, err := r.check(Options{Meta: []Text{
		{"--title-file", "docs: role\n"},
		{"--body-file", "Edits home/dot_claude/CLAUDE.md.\n"},
		{"--branch", "kaizen/role"},
	}})
	assertClean(t, out, err)
}

func TestRangeIntermediateCommit(t *testing.T) {
	r := newRepo(t)
	r.start(map[string]string{"a.md": "x\n"})
	notes := local("Users", "erin", "notes")
	r.write("a.md", "see "+notes+"x\n")
	c1 := r.commit("feat: add notes")
	r.write("a.md", "see the notes\n")
	c2 := r.commit("fix: drop")
	out, err := r.check(Options{})
	assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("Users", "erin")+"'")
	if strings.Contains(out, c2) {
		t.Errorf("hit names the commit that removed the path: %s", out)
	}
}

func TestRangeMessage(t *testing.T) {
	r := newRepo(t)
	r.start(map[string]string{"a.md": "x\n"})
	c1 := r.commit("fix: path\n\nFound under " + local("home", "bob", "work") + ".")
	out, err := r.check(Options{})
	assertHits(t, out, err, "commit "+c1+" message: local path '"+local("home", "bob")+"'")
}

func TestRangeVolumeAndBareHomePaths(t *testing.T) {
	r := newRepo(t)
	r.start(nil)
	bare := strings.TrimSuffix(local("Users", "erin"), "/")
	c1 := r.commit("fix: paths\n\nCopied from " + local("Volumes", "Data") + "x and cd " + bare)
	out, err := r.check(Options{})
	assertHits(t, out, err,
		"commit "+c1+" message: local path '"+local("Volumes", "Data")+"'",
		"commit "+c1+" message: local path '"+bare+"'")
}

// TestRangeMerge checks a line that only the merge commit adds (an evil merge): first-parent diffs
// publish it under the merge.
func TestRangeMerge(t *testing.T) {
	r := newRepo(t)
	r.start(map[string]string{"a.md": "x\n"})
	r.git("checkout", "-q", "-b", "side")
	r.write("side.md", "clean side\n")
	r.commit("feat: side")
	r.git("checkout", "-q", "main")
	r.write("main.md", "clean main\n")
	r.commit("feat: main")
	r.git("merge", "-q", "--no-ff", "--no-commit", "side")
	r.write("merge.md", "resolved under "+local("home", "mia", "x")+"\n")
	merge := r.commit("Merge branch 'side'")
	out, err := r.check(Options{})
	assertHits(t, out, err, "commit "+merge+" diff: local path '"+local("home", "mia")+"'")
}

// TestRangeDestinationPaths checks that destination names are scanned and old or deleted names, and
// patch headers, are not (port of the legacy rename case).
func TestRangeDestinationPaths(t *testing.T) {
	const secret = "\u79d8\u5bc6" // a Han term, escaped for gosmopolitan
	r := newRepo(t)
	body := strings.Repeat("shared line\n", 10)
	r.start(map[string]string{"zebra-old.md": body + "x\n", "zebra-gone.md": "gone\n"})
	r.git("mv", "zebra-old.md", "notes.md")
	r.write("notes.md", body+"y\n")
	r.git("rm", "-q", "zebra-gone.md")
	r.write("メモ/"+secret+".md", "hello\n")
	c1 := r.commit("docs: move notes")
	if names := r.git("show", "--format=", "-M", "--name-status", "HEAD"); !strings.HasPrefix(
		names, "R") {
		t.Fatalf("not detected as a rename:\n%s", names)
	}
	out, err := r.check(Options{Terms: []string{secret, "zebra"}})
	assertHits(t, out, err, "commit "+c1+" path: private term '"+secret+"'")
}

func TestRangeRenameDestination(t *testing.T) {
	r := newRepo(t)
	body := strings.Repeat("shared line\n", 10)
	r.start(map[string]string{"notes.md": body})
	r.git("mv", "notes.md", "zebra-notes.md")
	c1 := r.commit("docs: rename")
	out, err := r.check(Options{Terms: []string{"Zebra"}})
	assertHits(t, out, err, "commit "+c1+" path: private term 'Zebra'")
}

func TestRangePrivateTerm(t *testing.T) {
	r := newRepo(t)
	r.start(map[string]string{"a.md": "x\n"})
	r.write("a.md", "notes for project zebra\n")
	c1 := r.commit("feat: notes")
	out, err := r.check(Options{Terms: []string{"Project Zebra"}})
	assertHits(t, out, err, "commit "+c1+" diff: private term 'Project Zebra'")
}

func TestRangeMetadata(t *testing.T) {
	folders := local("private", "var", "folders", "xy")
	tests := []struct {
		meta Text
		want string
	}{
		{
			Text{"--title-file", "fix: project zebra\n"},
			"--title-file: private term 'Project Zebra'",
		},
		{
			Text{"--body-file", "Ran in " + folders + "T.\n"},
			"--body-file: local path '" + folders + "'",
		},
		{Text{"--branch", "kaizen/PROJECT ZEBRA"}, "--branch: private term 'Project Zebra'"},
	}
	r := newRepo(t)
	r.start(map[string]string{"a.md": "x\n"})
	r.write("a.md", "y\n")
	r.commit("docs: note")
	for _, tt := range tests {
		t.Run(tt.meta.Where, func(t *testing.T) {
			out, err := r.check(Options{Terms: []string{"Project Zebra"}, Meta: []Text{tt.meta}})
			assertHits(t, out, err, tt.want)
		})
	}
}

// TestRangeAfterLargePatch checks that a hit after megabytes of patch is found: nothing is cut.
func TestRangeAfterLargePatch(t *testing.T) {
	r := newRepo(t)
	r.start(nil)
	line := strings.Repeat("0123456789abcdef", 8) + "\n"
	big := strings.Repeat(line, (3<<20)/len(line))
	r.write("lock.json", big+"see "+local("home", "zoe", "x")+"\n")
	c1 := r.commit("chore: lockfile")
	out, err := r.check(Options{})
	assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("home", "zoe")+"'")
}

// TestRangeLineBreaks checks that an added line is split on `\n` only, and that textconv is not
// run: the driver names a command that does not exist, which would fail git.
func TestRangeLineBreaks(t *testing.T) {
	for name, sep := range map[string]string{"U+2028": "\u2028", "CR": "\r"} {
		t.Run(name, func(t *testing.T) {
			r := newRepo(t)
			r.start(map[string]string{".gitattributes": "*.md diff=hide\n"})
			r.git("config", "diff.hide.textconv", "stack-no-such-textconv")
			r.write("a.md", "note"+sep+"see "+local("home", "bob")+"x\n")
			c1 := r.commit("docs: note")
			out, err := r.check(Options{})
			assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("home", "bob")+"'")
		})
	}
}

func TestRangeDuplicateHitsOnce(t *testing.T) {
	r := newRepo(t)
	r.start(nil)
	r.write("a.md", strings.Repeat("see "+local("home", "bob")+"\n", 3))
	c1 := r.commit("docs: notes")
	out, err := r.check(Options{})
	assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("home", "bob")+"'")
}

func TestRangeEmptyRangeScansMetadata(t *testing.T) {
	r := newRepo(t)
	r.start(nil)
	out, err := r.check(Options{Meta: []Text{{"--branch", "x" + local("home", "bob")}}})
	assertClean(t, out, err) // the path does not start a token
	out, err = r.check(Options{Meta: []Text{{"--branch", local("home", "bob")}}})
	assertHits(t, out, err, "--branch: local path '"+local("home", "bob")+"'")
}

func TestValidRange(t *testing.T) {
	for _, rng := range []string{
		"topic", "main...topic", "..topic", "main..", "main..topic..x",
		".main..topic", "main..topic.", "-x..topic", "main..--output=x",
	} {
		if err := ValidRange(rng); !errors.Is(err, ErrUsage) {
			t.Errorf("ValidRange(%q) = %v, want ErrUsage", rng, err)
		}
	}
	for _, rng := range []string{"main..topic", "origin/main..HEAD", "a1b2c3..HEAD~2"} {
		if err := ValidRange(rng); err != nil {
			t.Errorf("ValidRange(%q) = %v", rng, err)
		}
	}
}

func TestRangeGitFailure(t *testing.T) {
	r := newRepo(t)
	r.start(nil)
	out, err := r.check(Options{Range: "base..no-such-ref"})
	if !errors.Is(err, ErrGit) || out != "" {
		t.Fatalf("err = %v, out = %q; want ErrGit", err, out)
	}
	out, err = r.check(Options{Range: "base...HEAD"})
	if !errors.Is(err, ErrUsage) || out != "" {
		t.Fatalf("err = %v, out = %q; want ErrUsage", err, out)
	}
}

const fakeSHA = "0123456789abcdef0123456789abcdef01234567"

func fakeCheck(t *testing.T, s proctest.Scenario, timeout time.Duration) (string, error) {
	t.Helper()
	git := proctest.Install(t, "git", s)
	var out bytes.Buffer
	err := Check(t.Context(), Options{
		Range: "a..b", Git: git, Runner: proc.Exec{}, Out: &out, GitTimeout: timeout,
	})
	return out.String(), err
}

func TestGitTimeout(t *testing.T) {
	out, err := fakeCheck(t, proctest.Scenario{Responses: []proctest.Response{
		{Sleep: 30 * time.Second},
	}}, 200*time.Millisecond)
	if !errors.Is(err, ErrGit) || !errors.Is(err, proc.ErrTimedOut) || out != "" {
		t.Fatalf("err = %v, out = %q; want ErrGit wrapping ErrTimedOut", err, out)
	}
}

func TestGitUnexpectedRevList(t *testing.T) {
	out, err := fakeCheck(t, proctest.Scenario{Responses: []proctest.Response{
		{Stdout: "warning: not a sha\n"},
	}}, 0)
	if !errors.Is(err, ErrGit) || out != "" {
		t.Fatalf("err = %v, out = %q; want ErrGit", err, out)
	}
}

// TestGitFailureAfterHits checks that a git failure is reported as such even after hits: the scan
// is incomplete, so it is not a plain finding.
func TestGitFailureAfterHits(t *testing.T) {
	out, err := fakeCheck(t, proctest.Scenario{Responses: []proctest.Response{
		{
			Args:   []string{"rev-list", "--reverse", "--end-of-options", "a..b"},
			Stdout: fakeSHA + "\n",
		},
		{
			Args:   []string{"show", "-s", "--format=%B", fakeSHA},
			Stdout: "see " + local("home", "bob"),
		},
		{Stderr: "fatal: broken", Exit: 128},
	}}, 0)
	if !errors.Is(err, ErrGit) || errors.Is(err, ErrFound) {
		t.Fatalf("err = %v; want ErrGit only", err)
	}
	if want := "check public: commit 012345678 message: local path '" + local("home", "bob") +
		"'\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
	if !strings.Contains(err.Error(), "fatal: broken") {
		t.Errorf("err does not carry git's stderr: %v", err)
	}
}

func TestPrivateTermsUnreadable(t *testing.T) {
	dir := t.TempDir()
	t.Run("missing", func(t *testing.T) {
		terms, err := ReadTerms(filepath.Join(dir, "none"))
		if err != nil || terms != nil {
			t.Fatalf("ReadTerms = %q, %v; want no terms", terms, err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := ReadTerms(dir); !errors.Is(err, ErrInput) {
			t.Fatalf("err = %v, want ErrInput", err)
		}
	})
	t.Run("not UTF-8", func(t *testing.T) {
		p := filepath.Join(dir, "latin1")
		if err := os.WriteFile(p, []byte("caf\xe9\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadTerms(p); !errors.Is(err, ErrInput) {
			t.Fatalf("err = %v, want ErrInput", err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("file modes do not deny reads on Windows")
		}
		p := filepath.Join(dir, "locked")
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(p); err == nil {
			t.Skip("running with privileges that read any file")
		}
		if _, err := ReadTerms(p); !errors.Is(err, ErrInput) {
			t.Fatalf("err = %v, want ErrInput", err)
		}
	})
}
