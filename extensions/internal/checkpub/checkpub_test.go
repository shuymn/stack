package checkpub

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestMain(m *testing.M) {
	proctest.Main()
	os.Exit(gittest.Main(m))
}

// local builds an absolute local path with a trailing slash at run time, so this file publishes no
// literal one: local("Users", "a").
func local(parts ...string) string {
	return "/" + strings.Join(parts, "/") + "/"
}

// check runs Check over base..HEAD of r, or o.Range, and returns its error and hit output.
func check(t *testing.T, r *gittest.Repo, o Options) (string, error) {
	t.Helper()
	var out bytes.Buffer
	if o.Range == "" {
		o.Range = "base..HEAD"
	}
	if o.Dir == "" {
		o.Dir = r.Dir
	}
	o.Env, o.Runner, o.Out = r.Env, proc.Exec{}, &out
	err := Check(t.Context(), o)
	return out.String(), err
}

// start writes files, makes the base commit and tags it `base`.
func start(r *gittest.Repo, files map[string]string) {
	for name, content := range files {
		r.Write(name, content)
	}
	r.Commit("chore: base")
	r.Tag("base")
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
	r := gittest.New(t)
	start(r, map[string]string{"home/dot_claude/CLAUDE.md": cleanBase()})
	r.Write("home/dot_claude/CLAUDE.md", strings.Replace(cleanBase(),
		"  removed line with "+local("home", "dave", "old"),
		"  Build for aarch64-darwin on the personal role, under ~/.claude/.", 1))
	r.Commit("docs: describe the role")
	r.Write("b.md", "Edits home/dot_claude/CLAUDE.md for aarch64-darwin.\n")
	r.Commit("fix: tidy")
	if diff := r.Git("show", "--format=", "HEAD~1"); !strings.Contains(diff, "@@ see /") {
		t.Fatalf("hunk header does not carry the base path:\n%s", diff)
	}
	out, err := check(t, r, Options{Meta: []Text{
		{"--title-file", "docs: role\n"},
		{"--body-file", "Edits home/dot_claude/CLAUDE.md.\n"},
		{"--branch", "kaizen/role"},
	}})
	assertClean(t, out, err)
}

func TestRangeIntermediateCommit(t *testing.T) {
	r := gittest.New(t)
	start(r, map[string]string{"a.md": "x\n"})
	notes := local("Users", "erin", "notes")
	r.Write("a.md", "see "+notes+"x\n")
	c1 := r.Commit("feat: add notes")
	r.Write("a.md", "see the notes\n")
	c2 := r.Commit("fix: drop")
	out, err := check(t, r, Options{})
	assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("Users", "erin")+"'")
	if strings.Contains(out, c2) {
		t.Errorf("hit names the commit that removed the path: %s", out)
	}
}

func TestRangeMessage(t *testing.T) {
	r := gittest.New(t)
	start(r, map[string]string{"a.md": "x\n"})
	c1 := r.Commit("fix: path\n\nFound under " + local("home", "bob", "work") + ".")
	out, err := check(t, r, Options{})
	assertHits(t, out, err, "commit "+c1+" message: local path '"+local("home", "bob")+"'")
}

func TestRangeVolumeAndBareHomePaths(t *testing.T) {
	r := gittest.New(t)
	start(r, nil)
	bare := strings.TrimSuffix(local("Users", "erin"), "/")
	c1 := r.Commit("fix: paths\n\nCopied from " + local("Volumes", "Data") + "x and cd " + bare)
	out, err := check(t, r, Options{})
	assertHits(t, out, err,
		"commit "+c1+" message: local path '"+local("Volumes", "Data")+"'",
		"commit "+c1+" message: local path '"+bare+"'")
}

// TestRangeMerge checks a line that only the merge commit adds (an evil merge): first-parent diffs
// publish it under the merge.
func TestRangeMerge(t *testing.T) {
	r := gittest.New(t)
	start(r, map[string]string{"a.md": "x\n"})
	r.Branch("side")
	r.Write("side.md", "clean side\n")
	r.Commit("feat: side")
	r.Checkout("main")
	r.Write("main.md", "clean main\n")
	r.Commit("feat: main")
	r.MergeNoCommit("side")
	r.Write("merge.md", "resolved under "+local("home", "mia", "x")+"\n")
	merge := r.Commit("Merge branch 'side'")
	out, err := check(t, r, Options{})
	assertHits(t, out, err, "commit "+merge+" diff: local path '"+local("home", "mia")+"'")
}

// TestRangeDestinationPaths checks that destination names are scanned and old or deleted names, and
// patch headers, are not (port of the legacy rename case).
func TestRangeDestinationPaths(t *testing.T) {
	const secret = "\u79d8\u5bc6" // a Han term, escaped for gosmopolitan
	r := gittest.New(t)
	body := strings.Repeat("shared line\n", 10)
	start(r, map[string]string{"zebra-old.md": body + "x\n", "zebra-gone.md": "gone\n"})
	r.Git("mv", "zebra-old.md", "notes.md")
	r.Write("notes.md", body+"y\n")
	r.Git("rm", "-q", "zebra-gone.md")
	r.Write("メモ/"+secret+".md", "hello\n")
	c1 := r.Commit("docs: move notes")
	if names := r.Git("show", "--format=", "-M", "--name-status", "HEAD"); !strings.HasPrefix(
		names, "R") {
		t.Fatalf("not detected as a rename:\n%s", names)
	}
	out, err := check(t, r, Options{Terms: []string{secret, "zebra"}})
	assertHits(t, out, err, "commit "+c1+" path: private term '"+secret+"'")
}

func TestRangeRenameDestination(t *testing.T) {
	r := gittest.New(t)
	body := strings.Repeat("shared line\n", 10)
	start(r, map[string]string{"notes.md": body})
	r.Git("mv", "notes.md", "zebra-notes.md")
	c1 := r.Commit("docs: rename")
	out, err := check(t, r, Options{Terms: []string{"Zebra"}})
	assertHits(t, out, err, "commit "+c1+" path: private term 'Zebra'")
}

func TestRangePrivateTerm(t *testing.T) {
	r := gittest.New(t)
	start(r, map[string]string{"a.md": "x\n"})
	r.Write("a.md", "notes for project zebra\n")
	c1 := r.Commit("feat: notes")
	out, err := check(t, r, Options{Terms: []string{"Project Zebra"}})
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
	r := gittest.New(t)
	start(r, map[string]string{"a.md": "x\n"})
	r.Write("a.md", "y\n")
	r.Commit("docs: note")
	for _, tt := range tests {
		t.Run(tt.meta.Where, func(t *testing.T) {
			out, err := check(
				t,
				r,
				Options{Terms: []string{"Project Zebra"}, Meta: []Text{tt.meta}},
			)
			assertHits(t, out, err, tt.want)
		})
	}
}

// TestRangeAfterLargePatch checks that a hit after megabytes of patch is found: nothing is cut.
func TestRangeAfterLargePatch(t *testing.T) {
	r := gittest.New(t)
	start(r, nil)
	line := strings.Repeat("0123456789abcdef", 8) + "\n"
	big := strings.Repeat(line, (3<<20)/len(line))
	r.Write("lock.json", big+"see "+local("home", "zoe", "x")+"\n")
	c1 := r.Commit("chore: lockfile")
	out, err := check(t, r, Options{})
	assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("home", "zoe")+"'")
}

// TestRangeLineBreaks checks that an added line is split on `\n` only, and that textconv is not
// run: the driver names a command that does not exist, which would fail git.
func TestRangeLineBreaks(t *testing.T) {
	for name, sep := range map[string]string{"U+2028": "\u2028", "CR": "\r"} {
		t.Run(name, func(t *testing.T) {
			r := gittest.New(t)
			start(r, map[string]string{".gitattributes": "*.md diff=hide\n"})
			r.Config("diff.hide.textconv", "stack-no-such-textconv")
			r.Write("a.md", "note"+sep+"see "+local("home", "bob")+"x\n")
			c1 := r.Commit("docs: note")
			out, err := check(t, r, Options{})
			assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("home", "bob")+"'")
		})
	}
}

func TestRangeDuplicateHitsOnce(t *testing.T) {
	r := gittest.New(t)
	start(r, nil)
	r.Write("a.md", strings.Repeat("see "+local("home", "bob")+"\n", 3))
	c1 := r.Commit("docs: notes")
	out, err := check(t, r, Options{})
	assertHits(t, out, err, "commit "+c1+" diff: local path '"+local("home", "bob")+"'")
}

func TestRangeEmptyRangeScansMetadata(t *testing.T) {
	r := gittest.New(t)
	start(r, nil)
	out, err := check(t, r, Options{Meta: []Text{{"--branch", "x" + local("home", "bob")}}})
	assertClean(t, out, err) // the path does not start a token
	out, err = check(t, r, Options{Meta: []Text{{"--branch", local("home", "bob")}}})
	assertHits(t, out, err, "--branch: local path '"+local("home", "bob")+"'")
}

func TestValidRange(t *testing.T) {
	for _, rng := range []string{
		"topic", "main...topic", "..topic", "main..", "main..topic..x",
		".main..topic", "main..topic.", "-x..topic", "main..--output=x",
	} {
		if err := ValidRange(rng); !errors.Is(err, errUsage) {
			t.Errorf("ValidRange(%q) = %v, want errUsage", rng, err)
		}
	}
	for _, rng := range []string{"main..topic", "origin/main..HEAD", "a1b2c3..HEAD~2"} {
		if err := ValidRange(rng); err != nil {
			t.Errorf("ValidRange(%q) = %v", rng, err)
		}
	}
}

func TestRangeGitFailure(t *testing.T) {
	r := gittest.New(t)
	start(r, nil)
	out, err := check(t, r, Options{Range: "base..no-such-ref"})
	if !errors.Is(err, errGit) || out != "" {
		t.Fatalf("err = %v, out = %q; want errGit", err, out)
	}
	out, err = check(t, r, Options{Range: "base...HEAD"})
	if !errors.Is(err, errUsage) || out != "" {
		t.Fatalf("err = %v, out = %q; want errUsage", err, out)
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

// toplevel is the fake git's answer to `rev-parse --show-toplevel`.
var toplevel = proctest.Response{Args: []string{"rev-parse", "--show-toplevel"}, Stdout: "top\n"}

func TestGitTimeout(t *testing.T) {
	sleep := proctest.Response{Sleep: 30 * time.Second}
	for name, responses := range map[string][]proctest.Response{
		"toplevel": {sleep},
		"stream":   {toplevel, sleep},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := fakeCheck(t, proctest.Scenario{Responses: responses}, 200*time.Millisecond)
			if !errors.Is(err, errGit) || !errors.Is(err, proc.ErrTimedOut) || out != "" {
				t.Fatalf("err = %v, out = %q; want errGit wrapping ErrTimedOut", err, out)
			}
		})
	}
}

func TestGitNotWorkTree(t *testing.T) {
	out, err := fakeCheck(t, proctest.Scenario{Responses: []proctest.Response{
		{Stderr: "fatal: not a git repository", Exit: 128},
	}}, 0)
	if !errors.Is(err, errGit) || !strings.Contains(err.Error(), "not a git repository") ||
		out != "" {
		t.Fatalf("err = %v, out = %q; want errGit with git's stderr", err, out)
	}
}

func TestGitUnexpectedRevList(t *testing.T) {
	out, err := fakeCheck(t, proctest.Scenario{Responses: []proctest.Response{
		toplevel,
		{Stdout: "warning: not a sha\n"},
	}}, 0)
	if !errors.Is(err, errGit) || out != "" {
		t.Fatalf("err = %v, out = %q; want errGit", err, out)
	}
}

// TestGitFailureAfterHits checks that a git failure is reported as such even after hits: the scan
// is incomplete, so it is not a plain finding.
func TestGitFailureAfterHits(t *testing.T) {
	out, err := fakeCheck(t, proctest.Scenario{Responses: []proctest.Response{
		toplevel,
		{
			Args:   []string{"-C", "top", "rev-list", "--reverse", "--end-of-options", "a..b"},
			Stdout: fakeSHA + "\n",
		},
		{
			Args:   []string{"-C", "top", "show", "-s", "--encoding=UTF-8", "--format=%B", fakeSHA},
			Stdout: "see " + local("home", "bob"),
		},
		{Stderr: "fatal: broken", Exit: 128},
	}}, 0)
	if !errors.Is(err, errGit) || errors.Is(err, ErrFound) {
		t.Fatalf("err = %v; want errGit only", err)
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

// TestInheritedRepoVarsIgnored checks that GIT_DIR, GIT_WORK_TREE and GIT_INDEX_FILE exported by a
// git hook do not redirect the scan: the repository is the one around Dir.
func TestInheritedRepoVarsIgnored(t *testing.T) {
	scanned := gittest.New(t)
	start(scanned, nil)
	scanned.Write("sub/a.md", "see "+local("home", "bob")+"\n")
	c1 := scanned.Commit("docs: notes")
	other := gittest.New(t)
	start(other, nil)
	gitDir := other.Git("rev-parse", "--absolute-git-dir")
	var out bytes.Buffer
	err := Check(t.Context(), Options{
		Range: "base..HEAD",
		Dir:   filepath.Join(scanned.Dir, "sub"),
		Env: append(slices.Clone(scanned.Env),
			"GIT_DIR="+gitDir, "GIT_WORK_TREE="+other.Dir,
			"GIT_INDEX_FILE="+filepath.Join(gitDir, "index")),
		Runner: proc.Exec{},
		Out:    &out,
	})
	assertHits(t, out.String(), err, "commit "+c1+" diff: local path '"+local("home", "bob")+"'")
}

// TestRangeDisplayConfig checks that git display settings cannot hide published text: a UTF-16 log
// encoding would interleave NULs into the message, and diff.ignoreSubmodules=all would drop an added
// gitlink's name.
func TestRangeDisplayConfig(t *testing.T) {
	r := gittest.New(t)
	start(r, nil)
	base := r.Git("rev-parse", "HEAD")
	r.Git("update-index", "--add", "--cacheinfo", "160000,"+base+",zebra-sub")
	r.Git("commit", "-q", "-m", "feat: vendor\n\nFrom "+local("home", "bob", "src"))
	c1 := r.Git("rev-parse", "HEAD")[:9]
	r.Config("i18n.logOutputEncoding", "UTF-16LE")
	r.Config("diff.ignoreSubmodules", "all")
	out, err := check(t, r, Options{Terms: []string{"zebra"}})
	assertHits(t, out, err,
		"commit "+c1+" message: local path '"+local("home", "bob")+"'",
		"commit "+c1+" path: private term 'zebra'")
}

// TestToplevelTrailingSpace checks that the work tree keeps a trailing space in its name: trimming
// it would scan the clean sibling instead.
func TestToplevelTrailingSpace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows strips trailing spaces from file names")
	}
	clean := gittest.New(t)
	start(clean, nil)
	clean.Commit("docs: clean")
	spaced := clean.Sibling("repo ")
	start(spaced, nil)
	c1 := spaced.Commit("fix: path\n\nSee " + local("home", "bob", "x"))
	out, err := check(t, spaced, Options{})
	assertHits(t, out, err, "commit "+c1+" message: local path '"+local("home", "bob")+"'")
}

func TestParseTerms(t *testing.T) {
	text := "# heading\rZebra\r  Project X  \r\n\n" +
		"a\vb\fc\x1cd\x1de\x1ef\u0085g\u2028h\u2029i\n#tail"
	want := []string{"Zebra", "Project X", "a", "b", "c", "d", "e", "f", "g", "h", "i"}
	if got := parseTerms(text); !slices.Equal(got, want) {
		t.Errorf("parseTerms = %q, want %q", got, want)
	}
	m := newTerms([]string{"Project X", "zebra"})
	if got := m.find(
		"notes for PROJECT x and ZEBRA",
	); !slices.Equal(
		got,
		[]string{"Project X", "zebra"},
	) {
		t.Errorf("find = %q", got)
	}
}

// TestNoRangeScansMetaOnly checks that an empty range scans the metadata without running git, as
// stage mode does for a request without a range.
func TestNoRangeScansMetaOnly(t *testing.T) {
	git := proctest.Install(t, "git", proctest.Scenario{Responses: []proctest.Response{{Exit: 1}}})
	var out bytes.Buffer
	err := Check(t.Context(), Options{
		Git: git, Runner: proc.Exec{}, Out: &out,
		Meta: []Text{{"--branch", local("home", "bob")}},
	})
	assertHits(t, out.String(), err, "--branch: local path '"+local("home", "bob")+"'")
	if calls := proctest.Calls(t, git); len(calls) != 0 {
		t.Errorf("git ran: %+v", calls)
	}
	out.Reset()
	err = Check(t.Context(), Options{Git: git, Runner: proc.Exec{}, Out: &out})
	assertClean(t, out.String(), err)
}

func TestReadInputFile(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(home, "t.txt"), []byte("title\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "b.md"), []byte("caf\xe9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadInputFile(home, "~/t.txt"); err != nil || got != "title\n" {
		t.Errorf("ReadInputFile(~/t.txt) = %q, %v", got, err)
	}
	for _, arg := range []string{"b.md", "none.md"} {
		if _, err := ReadInputFile(home, arg); !errors.Is(err, ErrInput) {
			t.Errorf("ReadInputFile(%q) = %v, want ErrInput", arg, err)
		}
	}
}
