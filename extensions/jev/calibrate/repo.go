package calibrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shuymn/stack/extensions/internal/proc"
)

const gitTimeout = time.Minute

var errGit = errors.New("git failed")

// Repo builds a synthetic repository with its own git: no inherited GIT_* variable, no system or
// global config, a fixed identity and date. Bodies depend only on file contents and messages, so
// they equal those of the tests' gittest repositories.
type Repo struct {
	Dir    string
	runner proc.Runner
	env    []string
}

// NewRepo initialises an empty repository at dir.
func NewRepo(ctx context.Context, runner proc.Runner, environ []string, dir string) (*Repo, error) {
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		return strings.HasPrefix(strings.ToUpper(kv), "GIT_")
	})
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=jev-calibrate", "GIT_AUTHOR_EMAIL=jev-calibrate@example.invalid",
		"GIT_COMMITTER_NAME=jev-calibrate", "GIT_COMMITTER_EMAIL=jev-calibrate@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	r := &Repo{Dir: dir, runner: runner, env: env}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create repository: %w", err)
	}
	if err := r.git(ctx, "init", "-q"); err != nil {
		return nil, err
	}
	return r, nil
}

// Write implements semantic.RepoOps.
func (r *Repo) Write(name, content string) error {
	p := filepath.Join(r.Dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// Remove implements semantic.RepoOps.
func (r *Repo) Remove(name string) error {
	if err := os.Remove(filepath.Join(r.Dir, filepath.FromSlash(name))); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// Git implements semantic.RepoOps.
func (r *Repo) Git(args ...string) error {
	return r.git(context.Background(), args...)
}

func (r *Repo) git(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	full := slices.Concat([]string{
		"-c", "core.autocrlf=false", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "init.defaultBranch=main",
	}, args)
	res, err := r.runner.Run(ctx, proc.Cmd{Name: "git", Args: full, Dir: r.Dir, Env: r.env})
	switch {
	case err != nil:
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	case res.TimedOut || res.ExitCode != 0:
		return fmt.Errorf("%w: git %s: exit %d: %s", errGit, strings.Join(args, " "),
			res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
