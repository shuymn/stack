// Package checkpub implements `stack check public`: it scans what a push or a PR would publish for
// local paths and private terms.
//
// For each commit of a `<base>..<head>` range it scans the message, the destination paths and the
// added lines of the patch, then the PR title, body and branch. Git output is streamed line by line
// with no size cap, so nothing passes because output was cut. Lines are split on `\n` only: a bare
// CR or U+2028 stays inside its line, as git publishes it.
package checkpub

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/publictext"
)

// defaultGitTimeout bounds each git invocation; a patch of a huge lockfile still fits.
const defaultGitTimeout = 10 * time.Minute

var (
	// ErrFound reports that at least one hit was written.
	ErrFound = errors.New("found text that must not be published")
	// ErrInput reports an input file that cannot be used.
	ErrInput = errors.New("cannot read")

	errUsage = errors.New("RANGE must be <base>..<head>")
	errGit   = errors.New("git failed")

	shaRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

	// repoVars point git at a repository. A git hook that runs `stack check public` exports some of
	// them, so they are dropped and the repository is found from the working directory instead.
	repoVars = []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
	}
)

// Text is one piece of metadata to scan.
type Text struct {
	// Where names it in hits, e.g. `--branch`.
	Where string
	Text  string
}

// Options configures Check.
type Options struct {
	// Range is `<base>..<head>`.
	Range string
	// Dir is where the repository is looked for; empty means the current directory.
	Dir string
	// Git is the git executable; empty means "git".
	Git string
	// Env replaces the inherited environment of git when non-nil. Repository-location variables
	// (GIT_DIR, GIT_WORK_TREE, …) are dropped from either.
	Env    []string
	Runner proc.Runner
	// Terms are the private terms (see ReadTerms).
	Terms []string
	// Meta is scanned after the commits, in order.
	Meta []Text
	// Out receives one line per distinct hit.
	Out io.Writer
	// GitTimeout bounds each git invocation; zero means 10 minutes.
	GitTimeout time.Duration
}

// ValidRange reports whether r is `<base>..<head>` with non-empty ends that do not start or end in
// `.` and do not start with `-`, which git would take as an option.
func ValidRange(r string) error {
	base, head, ok := strings.Cut(r, "..")
	if !ok || strings.Contains(head, "..") {
		return errUsage
	}
	for _, e := range []string{base, head} {
		if e == "" || strings.HasPrefix(e, ".") || strings.HasSuffix(e, ".") ||
			strings.HasPrefix(e, "-") {
			return errUsage
		}
	}
	return nil
}

// ReadTerms reads a private-terms file. A missing file has no terms; a file that exists but cannot
// be read, is a directory or is not UTF-8 is an ErrInput error.
func ReadTerms(path string) ([]string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err == nil && info.IsDir() {
		return nil, fmt.Errorf("%w %s: is a directory", ErrInput, path)
	}
	text, err := ReadUTF8(path)
	if err != nil {
		return nil, err
	}
	return parseTerms(text), nil
}

// ReadUTF8 reads a whole input file, which must be UTF-8.
func ReadUTF8(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w %s: %w", ErrInput, path, err)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w %s: not UTF-8", ErrInput, path)
	}
	return string(data), nil
}

// Check scans the range and the metadata. It writes each distinct hit once, in order, and returns
// ErrFound when there was any. A git failure is reported as such, also after hits were written.
func Check(ctx context.Context, o Options) error {
	if err := ValidRange(o.Range); err != nil {
		return err
	}
	if o.Git == "" {
		o.Git = "git"
	}
	if o.GitTimeout <= 0 {
		o.GitTimeout = defaultGitTimeout
	}
	if o.Env == nil {
		o.Env = os.Environ()
	}
	o.Env = dropRepoVars(o.Env)
	top, err := o.toplevel(ctx)
	if err != nil {
		return err
	}
	o.Dir = top
	s := &scanner{terms: newTerms(o.Terms), out: o.Out, seen: map[string]bool{}}

	var shas []string
	err = o.stream(ctx, []string{"rev-list", "--reverse", "--end-of-options", o.Range},
		eachLine(func(line string) error {
			if !shaRe.MatchString(line) {
				return fmt.Errorf("%w: unexpected rev-list line %q", errGit, line)
			}
			shas = append(shas, line)
			return nil
		}))
	if err != nil {
		return err
	}
	for _, sha := range shas {
		if err := o.commit(ctx, s, sha); err != nil {
			return err
		}
	}
	for _, m := range o.Meta {
		for line := range strings.SplitSeq(m.Text, "\n") {
			if err := s.scan(m.Where, line); err != nil {
				return err
			}
		}
	}
	if len(s.seen) > 0 {
		return ErrFound
	}
	return nil
}

// commit scans the message, destination paths and added lines of one commit.
func (o Options) commit(ctx context.Context, s *scanner, sha string) error {
	prefix := "commit " + sha[:9]
	// --encoding: i18n.logOutputEncoding could re-encode the message (UTF-16 hides every ASCII hit).
	err := o.stream(ctx, []string{"show", "-s", "--encoding=UTF-8", "--format=%B", sha},
		eachLine(func(line string) error { return s.scan(prefix+" message", line) }))
	if err != nil {
		return err
	}
	// The submodule flags override diff.ignoreSubmodules, which could hide an added gitlink.
	diff := []string{
		"show", "--format=", "-M", "--no-relative", "--diff-merges=first-parent",
		"--ignore-submodules=none", "--submodule=short",
	}
	// Destination names only: a deleted or renamed-away name is not published anew.
	err = o.stream(
		ctx,
		slices.Concat(diff, []string{"--name-only", "-z", "--diff-filter=ACMRT", sha}),
		eachRecord(0, func(name string) error { return s.scan(prefix+" path", name) }),
	)
	if err != nil {
		return err
	}
	where := prefix + " diff"
	inHunk := false
	return o.stream(ctx,
		slices.Concat(diff, []string{
			"--patch", "--text", "--no-color", "--no-ext-diff", "--no-textconv", sha,
		}),
		eachLine(func(line string) error {
			switch {
			case strings.HasPrefix(line, "diff --git "):
				inHunk = false
			case strings.HasPrefix(line, "@@"):
				inHunk = true
			case inHunk && strings.HasPrefix(line, "+"):
				return s.scan(where, line[1:])
			}
			return nil
		}))
}

// dropRepoVars returns env without the repository-location variables.
func dropRepoVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(
			repoVars,
			func(v string) bool { return strings.EqualFold(v, name) },
		)
	})
}

// toplevel returns the work tree that contains o.Dir.
func (o Options) toplevel(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, o.GitTimeout)
	defer cancel()
	args := []string{"rev-parse", "--show-toplevel"}
	res, err := o.Runner.Run(ctx, proc.Cmd{Name: o.Git, Args: args, Dir: o.Dir, Env: o.Env})
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: git %s: %w", errGit, strings.Join(args, " "), err)
	case res.TimedOut:
		return "", fmt.Errorf("%w: git %s: %w", errGit, strings.Join(args, " "), proc.ErrTimedOut)
	case res.WaitDelayExceeded, res.Truncated, res.ExitCode != 0:
		return "", fmt.Errorf("%w: git %s: exit %d: %s", errGit, strings.Join(args, " "),
			res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	// Only git's line end: a directory name may itself end in a space.
	top := strings.TrimSuffix(string(res.Stdout), "\n")
	if top == "" {
		return "", fmt.Errorf("%w: not in a work tree", errGit)
	}
	return top, nil
}

// stream runs git with args and passes its stdout to consume.
func (o Options) stream(ctx context.Context, args []string, consume func(io.Reader) error) error {
	ctx, cancel := context.WithTimeout(ctx, o.GitTimeout)
	defer cancel()
	cmd := proc.Cmd{Name: o.Git, Args: slices.Concat([]string{"-C", o.Dir}, args), Env: o.Env}
	err := o.Runner.Stream(ctx, cmd, consume)
	if err != nil {
		if errors.Is(err, errGit) {
			return err
		}
		return fmt.Errorf("%w: git %s: %w", errGit, strings.Join(args, " "), err)
	}
	return nil
}

// eachLine calls fn with each `\n`-terminated line of r, without the `\n`.
func eachLine(fn func(string) error) func(io.Reader) error {
	return eachRecord('\n', fn)
}

// eachRecord calls fn with each delim-terminated record of r, without delim. Records have no length
// limit.
func eachRecord(delim byte, fn func(string) error) func(io.Reader) error {
	return func(r io.Reader) error {
		br := bufio.NewReader(r)
		for {
			rec, err := br.ReadString(delim)
			if rec != "" {
				if ferr := fn(strings.TrimSuffix(rec, string(delim))); ferr != nil {
					return ferr
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%w: read: %w", errGit, err)
			}
		}
	}
}

// scanner writes each distinct hit once.
type scanner struct {
	terms terms
	out   io.Writer
	seen  map[string]bool
}

func (s *scanner) scan(where, text string) error {
	for _, hit := range publictext.LocalPaths(text) {
		if err := s.report(fmt.Sprintf("%s: local path '%s'", where, hit)); err != nil {
			return err
		}
	}
	for _, term := range s.terms.find(text) {
		if err := s.report(fmt.Sprintf("%s: private term '%s'", where, term)); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) report(msg string) error {
	if s.seen[msg] {
		return nil
	}
	s.seen[msg] = true
	if _, err := fmt.Fprintf(s.out, "check public: %s\n", msg); err != nil {
		return fmt.Errorf("write hit: %w", err)
	}
	return nil
}

// parseTerms returns the private terms of a private-terms file: each line trimmed of surrounding
// space, without blank lines and lines starting with `#`. Lines break where Python's
// str.splitlines breaks them, as the legacy public-check read the same file.
func parseTerms(text string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(text, isLineBreak) {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, t)
		}
	}
	return out
}

// isLineBreak reports the runes str.splitlines splits on (CRLF is a CR then an LF).
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// terms matches private terms as case-insensitive substrings.
type terms struct {
	terms []string
	lower []string
}

func newTerms(list []string) terms {
	t := terms{terms: list, lower: make([]string, len(list))}
	for i, term := range list {
		t.lower[i] = strings.ToLower(term)
	}
	return t
}

// find returns the terms that occur in s, in the order they were given.
func (t terms) find(s string) []string {
	if len(t.terms) == 0 {
		return nil
	}
	lower := strings.ToLower(s)
	var found []string
	for i, term := range t.lower {
		if strings.Contains(lower, term) {
			found = append(found, t.terms[i])
		}
	}
	return found
}
