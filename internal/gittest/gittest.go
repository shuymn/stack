// Package gittest creates git fixture repositories for tests and guards the repository the tests
// run in.
//
// Fixture git commands get an environment built from an allowlist, never the inherited one: a git
// hook (e.g. lefthook's pre-push) exports GIT_DIR, GIT_INDEX_FILE and friends, which would point a
// fixture's git at the real repository. Before every command a Repo also checks that git resolves
// its git dir inside the fixture. Main records the real repository's shared config, the HEAD and
// index of the worktree the tests run in, and its tags, and fails the run if any of them changed.
package gittest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc"
)

const timeout = time.Minute

// errGit reports a git command that did not finish with status 0 and complete output.
var errGit = errors.New("git failed")

// passThrough are the only inherited variables a fixture's git sees: the executable search path
// and what Windows needs to run programs and find a temp dir.
var passThrough = []string{"PATH", "PATHEXT", "SYSTEMROOT", "COMSPEC", "TEMP", "TMP", "TMPDIR"}

// CleanEnv returns env without any GIT_* variable.
func CleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// Repo is a fixture repository at Dir inside the temporary directory Root.
type Repo struct {
	tb testing.TB
	// Root holds the repository, its isolated home and git config.
	Root string
	// Dir is the work tree.
	Dir string
	// Env is the allowlisted environment every git command of the fixture gets.
	Env []string
	// GlobalConfig is the isolated global git config file.
	GlobalConfig string
	root         os.FileInfo
}

// New initialises an empty repository on branch main with signing and hooks off.
func New(tb testing.TB) *Repo {
	tb.Helper()
	root := tb.TempDir()
	r := &Repo{
		tb:           tb,
		Root:         root,
		Dir:          filepath.Join(root, "repo"),
		GlobalConfig: filepath.Join(root, "gitconfig"),
	}
	hooks := filepath.Join(root, "no-hooks")
	for _, d := range []string{hooks, filepath.Join(root, "home"), filepath.Join(root, "xdg")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			tb.Fatal(err)
		}
	}
	config := "[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n" +
		"[core]\n\tautocrlf = false\n\thooksPath = " + filepath.ToSlash(hooks) + "\n" +
		"[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(r.GlobalConfig, []byte(config), 0o600); err != nil {
		tb.Fatal(err)
	}
	for _, k := range passThrough {
		if v, ok := os.LookupEnv(k); ok {
			r.Env = append(r.Env, k+"="+v)
		}
	}
	r.Env = append(r.Env,
		"HOME="+filepath.Join(root, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "xdg"),
		"GIT_CONFIG_GLOBAL="+r.GlobalConfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CEILING_DIRECTORIES="+root,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	info, err := os.Stat(root)
	if err != nil {
		tb.Fatal(err)
	}
	r.root = info
	r.run(root, "-c", "core.autocrlf=false", "init", "-q", r.Dir)
	return r
}

// Git runs git in the work tree after checking that its git dir is inside the fixture, and returns
// stdout without surrounding space.
func (r *Repo) Git(args ...string) string {
	r.tb.Helper()
	gitDir := r.run(r.Dir, "rev-parse", "--absolute-git-dir")
	if !r.inside(filepath.FromSlash(gitDir)) {
		r.tb.Fatalf("gittest: git dir %s is outside the fixture %s; refusing git %q",
			gitDir, r.Root, args)
	}
	return r.run(r.Dir, args...)
}

// Write creates or replaces a work-tree file, creating its directories.
func (r *Repo) Write(name, content string) {
	r.tb.Helper()
	p := filepath.Join(r.Dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		r.tb.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.tb.Fatal(err)
	}
}

// Commit stages everything and commits it (empty commits allowed), returning the 9-character
// abbreviated SHA.
func (r *Repo) Commit(msg string) string {
	r.tb.Helper()
	r.Git("add", "-A")
	r.Git("commit", "-q", "--allow-empty", "-m", msg)
	return r.Git("rev-parse", "HEAD")[:9]
}

// Tag tags HEAD.
func (r *Repo) Tag(name string) {
	r.tb.Helper()
	r.Git("tag", name)
}

// Branch creates name at HEAD and switches to it.
func (r *Repo) Branch(name string) {
	r.tb.Helper()
	r.Git("checkout", "-q", "-b", name)
}

// Checkout switches to an existing branch.
func (r *Repo) Checkout(name string) {
	r.tb.Helper()
	r.Git("checkout", "-q", name)
}

// MergeNoCommit merges branch with a merge commit left uncommitted.
func (r *Repo) MergeNoCommit(branch string) {
	r.tb.Helper()
	r.Git("merge", "-q", "--no-ff", "--no-commit", branch)
}

// Config sets a repository-local config value.
func (r *Repo) Config(key, value string) {
	r.tb.Helper()
	r.Git("config", key, value)
}

func (r *Repo) run(dir string, args ...string) string {
	r.tb.Helper()
	out, err := run(dir, r.Env, args...)
	if err != nil {
		r.tb.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// inside reports whether p or one of its parents is the fixture root, comparing files rather than
// strings so symlinked temp dirs and Windows short names do not matter.
func (r *Repo) inside(p string) bool {
	for {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, r.root) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

func run(dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := proc.Exec{}.Run(ctx, proc.Cmd{Name: "git", Args: args, Dir: dir, Env: env})
	switch {
	case err != nil:
		return "", fmt.Errorf("gittest: git %q: %w", args, err)
	case res.TimedOut, res.WaitDelayExceeded, res.Truncated:
		return "", fmt.Errorf("gittest: git %q: %w: %+v", args, errGit, res)
	case res.ExitCode != 0:
		return "", fmt.Errorf("gittest: git %q: %w: exit %d: %s", args, errGit, res.ExitCode,
			res.Stderr)
	}
	return string(res.Stdout), nil
}

// Main runs the tests and fails the run when the real repository around the package directory
// changed: its shared config, this worktree's HEAD or index, or its tags. Outside a git checkout it only runs the tests.
func Main(m *testing.M) int {
	env := CleanEnv(os.Environ())
	before, err := snapshot(env)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gittest: real repository not checked: %v\n", err)
		return m.Run()
	}
	code := m.Run()
	after, err := snapshot(env)
	if err != nil {
		_, _ = fmt.Fprintf(
			os.Stderr,
			"gittest: real repository unreadable after the tests: %v\n",
			err,
		)
		return 1
	}
	if changed := diff(before, after); changed != "" {
		_, _ = fmt.Fprintf(
			os.Stderr,
			"gittest: the tests changed the real repository: %s\n",
			changed,
		)
		return 1
	}
	return code
}

type state map[string]string

// snapshot records what fixture tests could damage but other tools and sessions sharing the
// repository do not move: the shared config, the HEAD and index of the worktree the tests run in,
// and the tags. Other refs and worktrees change legitimately while the tests run.
func snapshot(env []string) (state, error) {
	gitPath := func(args ...string) (string, error) {
		out, err := run(
			"",
			env,
			append([]string{"rev-parse", "--path-format=absolute"}, args...)...)
		return filepath.FromSlash(strings.TrimSpace(out)), err
	}
	common, err := gitPath("--git-common-dir")
	if err != nil {
		return nil, err
	}
	s := state{}
	if s["config"], err = fileHash(filepath.Join(common, "config")); err != nil {
		return nil, err
	}
	// The staged entries, not the index file's bytes: any `git status` in the worktree (an editor,
	// a prompt) rewrites the file's cached stat data.
	if s["index"], err = run("", env, "ls-files", "--stage"); err != nil {
		return nil, err
	}
	if s["HEAD"], err = run("", env, "rev-parse", "HEAD"); err != nil {
		return nil, err
	}
	// The branch HEAD points to; detached is fine, so the exit status is not checked.
	s["HEAD ref"], _ = run("", env, "symbolic-ref", "-q", "HEAD")
	s["tags"], err = run("", env, "for-each-ref", "--format=%(refname) %(objectname)", "refs/tags")
	if err != nil {
		return nil, err
	}
	return s, nil
}

// fileHash is the SHA-256 of a file.
func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("gittest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func diff(before, after state) string {
	var changed []string
	for k, v := range before {
		if after[k] != v {
			changed = append(changed, fmt.Sprintf("%s:\nbefore:\n%s\nafter:\n%s", k, v, after[k]))
		}
	}
	return strings.Join(changed, "\n")
}
