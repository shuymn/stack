package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/gittest"
)

func TestMain(m *testing.M) {
	os.Exit(gittest.Main(m))
}

// checkRepo makes a fixture repository with an empty base commit tagged `base` and one commit on top
// whose message is msg, and changes into it. The git that stack runs inherits the process
// environment, so the process gets the fixture's git variables and no inherited ones.
func checkRepo(t *testing.T, msg string) {
	t.Helper()
	r := gittest.New(t)
	r.Commit("chore: base")
	r.Tag("base")
	r.Commit(msg)
	r.SetProcessEnv(t)
	t.Chdir(r.Dir)
}

func runHome(t *testing.T, home string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(t.Context(), args, Env{
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) string { return "" },
		Home:   home,
	})
	return result{code, stdout.String(), stderr.String()}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPublic(t *testing.T) {
	hit := "/" + "home" + "/bob/"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runHome(t, home, append([]string{"check", "public"}, tt.args...)...)
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

func TestCheckPublicCommitHit(t *testing.T) {
	checkRepo(t, "fix: path\n\nFound under /"+"Users/erin/work.")
	r := runHome(t, t.TempDir(), "check", "public", "base..HEAD")
	if r.code != exitRefused ||
		!strings.Contains(r.stderr, " message: local path '/"+"Users/erin/'") {
		t.Fatalf("r = %+v", r)
	}
}

// TestCheckPublicBadSetup checks the inputs that fail before git runs: a private-terms file that is
// not UTF-8 or is a directory, a broken config and an unknown home.
func TestCheckPublicBadSetup(t *testing.T) {
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
		{"broken config", func(home string) {
			writeFile(
				t,
				filepath.Join(home, ".config", "stack", "config.toml"),
				[]byte("version = \n"),
			)
		}, "config.toml:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			tt.setup(home)
			r := runHome(t, home, "check", "public", "base..HEAD")
			if r.code != exitUsage || !strings.Contains(r.stderr, tt.want) {
				t.Fatalf("r = %+v, want exit 2 with %q", r, tt.want)
			}
		})
	}
	if r := runHome(t, "", "check", "public", "base..HEAD"); r.code != exitUsage {
		t.Fatalf("no home: r = %+v, want exit 2", r)
	}
}

// TestCheckPublicInheritsNoGitVars checks that the git stack runs in these tests does not inherit a
// GIT_* variable from the test process: GIT_TRACE would make it write a trace file.
func TestCheckPublicInheritsNoGitVars(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	checkRepo(t, "fix: tidy")
	if r := runHome(t, t.TempDir(), "check", "public", "base..HEAD"); r.code != exitOK {
		t.Fatalf("r = %+v", r)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Errorf("GIT_TRACE reached git: stat = %v", err)
	}
}
