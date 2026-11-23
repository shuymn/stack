package classify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// Layout kinds of a fixture's repo spec: what the test builds under the fixture's root (§3.4).
const (
	// LayoutCheckout is an ordinary checkout with one commit.
	LayoutCheckout = "checkout"
	// LayoutWorktree is a linked worktree of the checkout named in Of.
	LayoutWorktree = "worktree"
	// LayoutSeparate is a work tree whose .git file points at a separate Git directory named .git
	// that is not a registered worktree.
	LayoutSeparate = "separate"
	// LayoutForeign is a work tree whose .git file points at another checkout's .git.
	LayoutForeign = "foreign"
)

// LayoutSpec is one directory of a fixture's repo spec.
type LayoutSpec struct {
	Kind string `json:"kind"`
	Dir  string `json:"dir"`
	Of   string `json:"of,omitempty"`
}

// RepoSpec is a classify fixture's repo field.
type RepoSpec struct {
	Layouts []LayoutSpec `json:"layouts"`
}

// GitFunc runs git in dir with a fixed identity and no inherited configuration.
type GitFunc func(dir string, args ...string) error

var errLayout = errors.New("invalid fixture layout")

// Materialize writes a classify fixture's disk inputs under root: its repository layouts, the
// observe file (observe.json), its files and the memory indexes, each written where the lookup
// places it (under root/claude). It returns root with symlinks resolved, the directory every
// relative path of the fixture's request resolves against.
func Materialize(f jev.Fixture, root string, git GitFunc) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("fixture root: %w", err)
	}
	var spec RepoSpec
	if len(f.Repo) > 0 {
		if err := json.Unmarshal(f.Repo, &spec); err != nil {
			return "", fmt.Errorf("fixture repo: %w", err)
		}
	}
	for _, l := range spec.Layouts {
		if err := buildLayout(root, l, git); err != nil {
			return "", err
		}
	}
	if len(f.Observe) > 0 {
		if err := writeFixtureFile(root, "observe.json", indent(f.Observe)); err != nil {
			return "", err
		}
	}
	for rel, raw := range f.Files {
		content, err := fileContent(raw)
		if err != nil {
			return "", fmt.Errorf("fixture file %s: %w", rel, err)
		}
		// "@project:<dir>/<rest>" is <rest> in the Claude project directory of <dir>.
		if p, ok := strings.CutPrefix(rel, "@project:"); ok {
			dir, rest, _ := strings.Cut(p, "/")
			abs := filepath.Join(root, filepath.FromSlash(dir))
			rel = "claude/projects/" + ProjectName(abs) + "/" + rest
		}
		if err := writeFixtureFile(root, rel, content); err != nil {
			return "", err
		}
	}
	for dir, index := range f.Memory {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		rel := filepath.Join("claude", "projects", ProjectName(abs), "memory", memoryIndexFile)
		if err := writeFixtureFile(root, rel, index); err != nil {
			return "", err
		}
	}
	return root, nil
}

// fileContent is a string as is, or an array of records written one JSON object per line.
func fileContent(raw json.RawMessage) (string, error) {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return "", fmt.Errorf("decode: %w", err)
		}
		return s, nil
	}
	var recs []json.RawMessage
	if err := json.Unmarshal(t, &recs); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	var b strings.Builder
	for _, r := range recs {
		var c bytes.Buffer
		if err := json.Compact(&c, r); err != nil {
			return "", fmt.Errorf("compact: %w", err)
		}
		b.Write(c.Bytes())
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func indent(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return string(raw)
	}
	return b.String() + "\n"
}

func writeFixtureFile(root, rel, content string) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("fixture file %s: %w", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		return fmt.Errorf("fixture file %s: %w", rel, err)
	}
	return nil
}

func buildLayout(root string, l LayoutSpec, git GitFunc) error {
	dir := filepath.Join(root, filepath.FromSlash(l.Dir))
	of := filepath.Join(root, filepath.FromSlash(l.Of))
	switch l.Kind {
	case LayoutCheckout:
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return nil
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("layout %s: %w", l.Dir, err)
		}
		for _, args := range [][]string{
			{"init", "-q"},
			{"commit", "-q", "--allow-empty", "-m", "init"},
		} {
			if err := git(dir, args...); err != nil {
				return err
			}
		}
	case LayoutWorktree:
		return git(of, "worktree", "add", "-q", dir, "-b", "wt-"+filepath.Base(dir))
	case LayoutSeparate:
		meta := filepath.Join(root, "meta-"+filepath.Base(dir), ".git")
		if err := os.MkdirAll(filepath.Dir(meta), 0o700); err != nil {
			return fmt.Errorf("layout %s: %w", l.Dir, err)
		}
		if err := git(of, "clone", "-q", "--bare", ".", meta); err != nil {
			return err
		}
		if err := git(of, "config", "--file", filepath.Join(meta, "config"), "core.bare",
			"false"); err != nil {
			return err
		}
		return gitFile(dir, meta)
	case LayoutForeign:
		return gitFile(dir, filepath.Join(of, ".git"))
	default:
		return fmt.Errorf("%w: kind %q", errLayout, l.Kind)
	}
	return nil
}

// gitFile makes dir a work tree whose .git file names gitdir.
func gitFile(dir, gitdir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("layout: %w", err)
	}
	content := "gitdir: " + filepath.ToSlash(gitdir) + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o600); err != nil {
		return fmt.Errorf("layout: %w", err)
	}
	return nil
}
