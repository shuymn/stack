package public

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/gittest"
)

// TestCheck covers C13: the CLI mode keeps the `stack check public` table, its stderr and its exit
// codes, with private terms named by safety.toml.
func TestCheck(t *testing.T) {
	hit := local("home", "bob")
	checkRepo(t, "fix: tidy")
	home := t.TempDir()
	terms := filepath.Join(home, ".config", "stack", "private-terms")
	writeFile(t, terms, []byte("# synthetic\r\nZebra\r\n"))
	writeFile(t, filepath.Join(home, "title.txt"), []byte("docs: role\n"))
	writeFile(t, "body.md", []byte("Ran in "+hit+"x\n"))
	writeFile(t, "latin1.md", []byte("caf\xe9\n"))

	tests := []struct {
		name string
		args []string
		code int
		want []string
	}{
		{"clean", []string{"base..HEAD", "--branch", "kaizen/role"}, exitOK, nil},
		{
			"tilde title file",
			[]string{"base..HEAD", "--title-file", "~/title.txt"},
			exitOK, nil,
		},
		{
			"hits in body and branch, in order",
			[]string{"--branch", "kaizen/project-zebra", "base..HEAD", "--body-file", "body.md"},
			exitRefused,
			[]string{
				"check public: --body-file: local path '" + hit + "'",
				"check public: --branch: private term 'Zebra'",
				"stack check public: refused: found text that must not be published",
			},
		},
		{"three dots", []string{"base...HEAD"}, exitUsage, []string{"RANGE must be"}},
		{"unknown ref", []string{"base..nope"}, exitUsage, []string{"git failed"}},
		{
			"missing body",
			[]string{"base..HEAD", "--body-file", "none.md"},
			exitUsage,
			[]string{"--body-file: cannot read"},
		},
		{
			"body not UTF-8",
			[]string{"base..HEAD", "--body-file", "latin1.md"},
			exitUsage,
			[]string{"not UTF-8"},
		},
		{"no range", []string{"--branch", "x"}, exitUsage, []string{"want 1 positional"}},
		{"unknown flag", []string{"base..HEAD", "--nope"}, exitUsage, []string{"unknown flag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runMain(t, home, "", append([]string{"check"}, tt.args...)...)
			if r.code != tt.code || r.stdout != "" {
				t.Fatalf("code = %d, stdout = %q, stderr = %q; want code %d",
					r.code, r.stdout, r.stderr, tt.code)
			}
			last := -1
			for _, w := range tt.want {
				i := strings.Index(r.stderr, w)
				if i <= last {
					t.Errorf("stderr lacks %q in order:\n%s", w, r.stderr)
				}
				last = i
			}
			if tt.code == exitOK && r.stderr != "" {
				t.Errorf("stderr = %q, want empty", r.stderr)
			}
		})
	}
}

func TestCheckCommitHit(t *testing.T) {
	checkRepo(t, "fix: path\n\nFound under "+local("Users", "erin")+"work.")
	r := runMain(t, t.TempDir(), "", "check", "base..HEAD")
	if r.code != exitRefused ||
		!strings.Contains(r.stderr, " message: local path '"+local("Users", "erin")+"'") {
		t.Fatalf("r = %+v", r)
	}
}

// TestCheckBadSetup checks the inputs that fail before git runs: a private-terms file that is not
// UTF-8 or is a directory, a broken safety.toml and an unknown home.
func TestCheckBadSetup(t *testing.T) {
	checkRepo(t, "fix: tidy")
	tests := []struct {
		name  string
		setup func(home string)
		want  string
	}{
		{"terms not UTF-8", func(home string) {
			writeFile(t, filepath.Join(home, ".config", "stack", "private-terms"), []byte("\xff\n"))
		}, "not UTF-8"},
		{"terms directory", func(home string) {
			if err := os.MkdirAll(filepath.Join(home, ".config", "stack", "private-terms"),
				0o700); err != nil {
				t.Fatal(err)
			}
		}, "is a directory"},
		{"broken safety.toml", func(home string) {
			writeFile(t, filepath.Join(home, ".config", "stack", "safety.toml"),
				[]byte("private_terms = \n"))
		}, "safety.toml:1"},
		{"custom terms file", func(home string) {
			writeFile(t, filepath.Join(home, ".config", "stack", "safety.toml"),
				[]byte("private_terms = \"~/terms\"\n"))
			writeFile(t, filepath.Join(home, "terms"), []byte("\xff\n"))
		}, "not UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			tt.setup(home)
			r := runMain(t, home, "", "check", "base..HEAD")
			if r.code != exitUsage || !strings.Contains(r.stderr, tt.want) {
				t.Fatalf("r = %+v, want exit 2 with %q", r, tt.want)
			}
		})
	}
	if r := runMain(t, "", "", "check", "base..HEAD"); r.code != exitUsage ||
		!strings.Contains(r.stderr, "home directory unknown") {
		t.Fatalf("no home: r = %+v, want exit 2", r)
	}
}

// TestCheckInheritsNoGitVars checks that the git stack-public runs in these tests does not inherit
// a GIT_* variable from the test process: GIT_TRACE would make it write a trace file.
func TestCheckInheritsNoGitVars(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	checkRepo(t, "fix: tidy")
	if r := runMain(t, t.TempDir(), "", "check", "base..HEAD"); r.code != exitOK {
		t.Fatalf("r = %+v", r)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Errorf("GIT_TRACE reached git: stat = %v", err)
	}
}

// TestUsage checks the top-level usage, which names the stack-public invocation; `check`'s own
// usage is the core command's (TestCheckMatchesCore).
func TestUsage(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		if r := runMain(t, "", "", args...); r.code != exitOK ||
			r.stdout != "usage: stack-public check <base>..<head> [--title-file F] [--body-file F] "+
				"[--branch B]\n       stack-public   (no arguments: one outbound request on stdin)\n" ||
			r.stderr != "" {
			t.Errorf("%q = %+v", args, r)
		}
	}
	if r := runMain(t, "", "", "scan"); r.code != exitUsage ||
		!strings.HasPrefix(
			r.stderr,
			`stack-public: unknown command "scan"`+"\nusage: stack-public check ",
		) {
		t.Errorf("unknown command = %+v", r)
	}
}

// TestCheckMatchesCore covers P9 in the test suite: on a seeded repository with a local path and a
// private term in a commit, the title and the body, the real `stack check public` and
// `stack-public check` write byte-identical stderr and exit with the same code.
func TestCheckMatchesCore(t *testing.T) {
	needBuilt(t)
	r := checkRepoNoChdir(t, "feat: notes for Project Zebra\n\nSee "+local("Users", "erin")+"x")
	r.Write("notes.md", "zebra notes under "+local("home", "bob")+"\n")
	r.Commit("docs: notes")
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "stack", "private-terms"), []byte("Zebra\n"))
	writeFile(t, filepath.Join(home, ".config", "stack", "safety.toml"),
		[]byte("private_terms = \"~/.config/stack/private-terms\"\n"))
	title := filepath.Join(home, "title.txt")
	body := filepath.Join(home, "body.md")
	writeFile(t, title, []byte("docs: zebra\n"))
	writeFile(t, body, []byte("Ran in "+local("Users", "erin")+"x\n"))
	for _, args := range [][]string{
		{"base..HEAD", "--title-file", title, "--body-file", body, "--branch", "kaizen/zebra"},
		{"base..HEAD"},
		{"base..nope"},
		{"base...HEAD"},
		{"base..HEAD", "--body-file", filepath.Join(home, "none.md")},
		{"base..HEAD", "--nope"},
		{"base..HEAD", "--branch"},
		{"--branch", "x"},
		{"base..HEAD", "a..b"},
		{"--help"},
		{"base..HEAD", "-h"},
	} {
		core := runBinary(t, r, home, r.Dir, "", built.stack, append([]string{"check", "public"},
			args...)...)
		ext := runBinary(t, r, home, r.Dir, "", built.public, append([]string{"check"},
			args...)...)
		if core.code != ext.code || core.stderr != ext.stderr || core.stdout != ext.stdout {
			t.Errorf("%q:\nstack check public = %+v\nstack-public check = %+v", args, core, ext)
		}
	}
}

// checkRepoNoChdir is checkRepo for tests that run binaries with the fixture's environment.
func checkRepoNoChdir(t *testing.T, msg string) *gittest.Repo {
	t.Helper()
	r := gittest.New(t)
	r.Commit("chore: base")
	r.Tag("base")
	r.Commit(msg)
	return r
}
