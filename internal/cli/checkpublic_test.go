package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc"
)

// checkRepo makes a git repository with a clean base commit tagged `base` and one commit on top
// whose message is msg, isolated from the user's git config through t.Setenv, and changes into it.
func checkRepo(t *testing.T, msg string) {
	t.Helper()
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, who := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+who+"_NAME", "Test")
		t.Setenv("GIT_"+who+"_EMAIL", "test@example.com")
		t.Setenv("GIT_"+who+"_DATE", "2026-01-01T00:00:00Z")
	}
	dir := filepath.Join(root, "repo")
	git := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		res, err := proc.Exec{}.Run(ctx, proc.Cmd{Name: "git", Args: args, Dir: dir})
		if err != nil || res.ExitCode != 0 || res.TimedOut || res.WaitDelayExceeded {
			t.Fatalf("git %q: %v %s", args, err, res.Stderr)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	git("-c", "core.autocrlf=false", "-c", "init.defaultBranch=main", "init", "-q")
	git("config", "commit.gpgsign", "false")
	git("commit", "-q", "--allow-empty", "-m", "chore: base")
	git("tag", "base")
	git("commit", "-q", "--allow-empty", "-m", msg)
	t.Chdir(dir)
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
