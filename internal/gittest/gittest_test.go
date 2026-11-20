package gittest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	os.Exit(Main(m))
}

// fatalTB records a fatal failure instead of ending the test.
type fatalTB struct {
	testing.TB

	msg string
}

type fatal struct{}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	panic(fatal{})
}

func (f *fatalTB) Fatal(args ...any) {
	f.msg = fmt.Sprint(args...)
	panic(fatal{})
}

// gitFails runs fn and returns the fatal message it raised, or "".
func gitFails(f *fatalTB, fn func()) string {
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(fatal); !ok {
					panic(r)
				}
			}
		}()
		fn()
	}()
	return f.msg
}

// TestGitRefusesForeignGitDir checks the guard: a git dir outside the fixture, here forced through
// GIT_DIR, stops the command before it runs.
func TestGitRefusesForeignGitDir(t *testing.T) {
	other := New(t)
	other.Commit("chore: other")
	before := other.Git("for-each-ref")

	f := &fatalTB{TB: t}
	r := New(f)
	r.Env = append(slices.Clone(r.Env), "GIT_DIR="+other.Git("rev-parse", "--absolute-git-dir"))
	msg := gitFails(f, func() { r.Tag("leak") })
	if !strings.Contains(msg, "outside the fixture") {
		t.Fatalf("Tag with a foreign GIT_DIR: fatal = %q, want the guard", msg)
	}
	if after := other.Git("for-each-ref"); after != before {
		t.Errorf("foreign repository changed: %q -> %q", before, after)
	}
}

func TestEnvIsAllowlisted(t *testing.T) {
	t.Setenv("GIT_DIR", "elsewhere")
	t.Setenv("GIT_INDEX_FILE", "elsewhere")
	r := New(t)
	for _, kv := range r.Env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			t.Errorf("inherited %s", kv)
		}
	}
	if sha := r.Commit("chore: first"); len(sha) != 9 {
		t.Errorf("Commit = %q", sha)
	}
	if got := CleanEnv([]string{"PATH=x", "GIT_DIR=y", "git_work_tree=z"}); !slices.Equal(
		got, []string{"PATH=x"}) {
		t.Errorf("CleanEnv = %q", got)
	}
}

// TestSnapshot checks what the sentinel compares and when it may skip: a skip-worktree flag change
// shows; outside a checkout there is no snapshot; a broken checkout is an error.
func TestSnapshot(t *testing.T) {
	r := New(t)
	r.Write("a.md", "x\n")
	r.Commit("chore: first")
	before, err := guard(r.Dir, r.Env)
	if err != nil || before == nil {
		t.Fatalf("guard = %v, %v", before, err)
	}
	r.Git("update-index", "--skip-worktree", "a.md")
	after, err := snapshot(r.Dir, r.Env)
	if err != nil {
		t.Fatal(err)
	}
	if d := diff(before, after); !strings.HasPrefix(d, "index:") {
		t.Errorf("skip-worktree change: diff = %q, want an index change", d)
	}

	outside := filepath.Join(r.Root, "plain")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := guard(outside, r.Env); !errors.Is(err, errNotCheckout) {
		t.Errorf("outside a checkout: err = %v, want errNotCheckout", err)
	}

	broken := filepath.Join(r.Root, "broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(broken, ".git"),
		[]byte("gitdir: missing\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := guard(broken, r.Env); err == nil || errors.Is(err, errNotCheckout) {
		t.Errorf("broken checkout: err = %v, want a failure that is not errNotCheckout", err)
	}
}
