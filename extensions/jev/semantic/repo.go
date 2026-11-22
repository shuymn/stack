package semantic

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Refs of a fixture repository (§3.4): the merge base, the PR head and the diverged base.
const (
	RefMergeBase = "jev-mb"
	RefHead      = "jev-head"
	RefBase      = "jev-base"
	// FixtureRange is the range every semantic fixture asks about.
	FixtureRange = RefMergeBase + ".." + RefHead
)

var errContent = errors.New("file content must be a string or {line, repeat}")

// RepoSpec is a fixture's synthetic repository: base files committed at jev-mb, the PR's commits
// up to jev-head, and commits made only on the base after the merge base (jev-base).
type RepoSpec struct {
	Base     map[string]Content `json:"base"`
	Commits  []CommitSpec       `json:"commits"`
	Upstream []CommitSpec       `json:"upstream,omitempty"`
}

// CommitSpec is one commit: its message and the files it writes (null deletes).
type CommitSpec struct {
	Message string              `json:"message"`
	Files   map[string]*Content `json:"files"`
}

// Content is a file's text, or Repeat lines of Line with "{n}" replaced by the line number.
type Content struct {
	Text   string
	Line   string
	Repeat int
}

// UnmarshalJSON accepts a string or {"line": …, "repeat": n}.
func (c *Content) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &c.Text); err == nil {
		return nil
	}
	var gen struct {
		Line   string `json:"line"`
		Repeat int    `json:"repeat"`
	}
	if err := json.Unmarshal(data, &gen); err != nil || gen.Repeat <= 0 {
		return errContent
	}
	c.Line, c.Repeat = gen.Line, gen.Repeat
	return nil
}

// MarshalJSON writes the form it was read from.
func (c Content) MarshalJSON() ([]byte, error) {
	var v any = c.Text
	if c.Repeat > 0 {
		v = map[string]any{"line": c.Line, "repeat": c.Repeat}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode content: %w", err)
	}
	return data, nil
}

// String is the file's text.
func (c Content) String() string {
	if c.Repeat == 0 {
		return c.Text
	}
	var b strings.Builder
	for i := 1; i <= c.Repeat; i++ {
		b.WriteString(strings.ReplaceAll(c.Line, "{n}", strconv.Itoa(i)))
		b.WriteByte('\n')
	}
	return b.String()
}

// RepoOps is what building a repository needs; the tests use gittest, jev-calibrate its own git.
type RepoOps interface {
	Write(name, content string) error
	Remove(name string) error
	Git(args ...string) error
}

// Build makes the repository in ops's work tree and leaves jev-head checked out, detached.
func (s RepoSpec) Build(ops RepoOps) error {
	for _, name := range slices.Sorted(maps.Keys(s.Base)) {
		if err := ops.Write(name, s.Base[name].String()); err != nil {
			return err
		}
	}
	steps := [][]string{
		{"add", "-A"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
		{"tag", RefMergeBase},
	}
	if err := run(ops, steps...); err != nil {
		return err
	}
	if err := commits(ops, s.Commits); err != nil {
		return err
	}
	if err := run(ops, []string{"tag", RefHead}); err != nil {
		return err
	}
	if len(s.Upstream) > 0 {
		if err := run(ops, []string{"checkout", "-q", "--detach", RefMergeBase}); err != nil {
			return err
		}
		if err := commits(ops, s.Upstream); err != nil {
			return err
		}
		if err := run(ops, []string{"tag", RefBase}); err != nil {
			return err
		}
	}
	return run(ops, []string{"checkout", "-q", "--detach", RefHead})
}

func commits(ops RepoOps, cs []CommitSpec) error {
	for _, c := range cs {
		for _, name := range slices.Sorted(maps.Keys(c.Files)) {
			var err error
			if c.Files[name] == nil {
				err = ops.Remove(name)
			} else {
				err = ops.Write(name, c.Files[name].String())
			}
			if err != nil {
				return err
			}
		}
		err := run(ops, []string{"add", "-A"},
			[]string{"commit", "-q", "--allow-empty", "-m", c.Message})
		if err != nil {
			return err
		}
	}
	return nil
}

func run(ops RepoOps, cmds ...[]string) error {
	for _, args := range cmds {
		if err := ops.Git(args...); err != nil {
			return fmt.Errorf("fixture repository: %w", err)
		}
	}
	return nil
}

// Label is a semantic fixture's hand-written ground truth (§3.5).
//
//nolint:tagliatelle // the keys are the question names.
type Label struct {
	Files       map[string]map[string]bool `json:"files"`
	Unsupported bool                       `json:"unsupported_claim"`
	// Split is fit or heldout for a cal case, fixed for a fixed one.
	Split string `json:"split"`
	Why   string `json:"why"`
}

// MetaTruth is the ground truth of a request carrying files: any of them labelled true.
func (l Label) MetaTruth(files []string) bool {
	return slices.ContainsFunc(files, func(f string) bool { return l.Files[f][MetaDuty] })
}
