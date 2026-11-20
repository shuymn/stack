package guard

import (
	"encoding/json"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/shell"
)

const secretsAdvice = "Commands and reads must not show a secret file. Pass a secret file to the " +
	`command that needs it only as an assignment or flag value: K="$(cat <file>)" cmd, or ` +
	`cmd --token "$(cat <file>)".`

// foldCase reports whether paths compares case-insensitively on this platform (darwin, windows).
var foldCase = paths.Norm("A") == "a"

// secret is one `[safety].secrets` entry.
type secret struct {
	entry string
	// root is the absolute path a `~` or absolute entry names, "" for a relative entry or when home
	// is unknown.
	root string
	// realRoot is root with its symlinks resolved, when that differs (a symlink target is compared
	// with it).
	realRoot string
	// text is what a command mentions: the home-relative part of a `~` entry, else the entry.
	text string
	// comps are the components of a relative entry, matched as consecutive path components. A `~`
	// entry under an unknown home is matched by its home-relative part this way.
	comps []string
}

func secrets(opts Options) []secret {
	var out []secret
	for _, e := range opts.Config.Safety.Secrets {
		if e == "" {
			continue
		}
		s := secret{entry: e, text: e}
		switch {
		case e == "~" || strings.HasPrefix(e, "~/") || strings.HasPrefix(e, `~\`):
			s.text = strings.TrimLeft(e[1:], `/\`)
			if opts.Home != "" {
				s.root = paths.Expand(opts.Home, e)
			} else {
				s.comps = components(paths.Norm(s.text))
			}
		case filepath.IsAbs(e) || strings.HasPrefix(e, "/"):
			s.root = e
		default:
			s.comps = components(paths.Norm(e))
		}
		if s.root != "" {
			if resolved, err := filepath.EvalSymlinks(s.root); err == nil &&
				!paths.Same(resolved, s.root) {
				s.realRoot = resolved
			}
		}
		out = append(out, s)
	}
	return out
}

func components(norm string) []string {
	return strings.FieldsFunc(norm, func(r rune) bool { return r == '/' })
}

// matchPath reports whether p (absolute when abs, else as written) is or lies in the secret.
func (s secret) matchPath(p string, abs bool) bool {
	if s.root != "" {
		return abs && (paths.Within(p, s.root) || (s.realRoot != "" && paths.Within(p, s.realRoot)))
	}
	if len(s.comps) == 0 {
		return false
	}
	pc := components(paths.Norm(p))
	for i := 0; i+len(s.comps) <= len(pc); i++ {
		if slices.Equal(pc[i:i+len(s.comps)], s.comps) {
			return true
		}
	}
	return false
}

// matchGlobComponent reports whether a glob component can name a bare (one-component) entry: it
// equals the entry, or contains its literal text and matches it.
func (s secret) matchGlobComponent(c string) bool {
	if len(s.comps) != 1 {
		return false
	}
	c = paths.Norm(c)
	e := s.comps[0]
	if c == e {
		return true
	}
	ok, err := path.Match(c, e)
	return strings.Contains(c, e) && err == nil && ok
}

// fileToolInput holds the path fields of the file tools.
//
//nolint:tagliatelle // Claude Code defines the tool input keys in snake_case.
type fileToolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
	Glob         string `json:"glob"`
	Pattern      string `json:"pattern"`
}

// secretsFileTool judges Read, NotebookRead, Grep and Glob.
func secretsFileTool(in hookio.Input, opts Options) (Decision, bool) {
	var tin fileToolInput
	if json.Unmarshal(in.ToolInput, &tin) != nil {
		return Decision{}, false
	}
	list := secrets(opts)
	var targets []string
	var glob string
	switch in.ToolName {
	case "Read":
		targets = []string{tin.FilePath}
	case "NotebookRead":
		targets = []string{tin.NotebookPath, tin.FilePath}
	case "Grep":
		targets, glob = []string{tin.Path}, tin.Glob
	case "Glob":
		targets, glob = []string{tin.Path}, tin.Pattern
	}
	follow := in.ToolName == "Read" || in.ToolName == "NotebookRead"
	for _, t := range targets {
		if t == "" {
			continue
		}
		if s, ok := secretPath(list, t, in.CWD, opts.Home, follow); ok {
			return deny(RuleSecrets, "%s targets the secret %q. %s", in.ToolName, s.entry,
				secretsAdvice)
		}
	}
	patterns := []string{glob}
	if in.ToolName == "Grep" {
		patterns = splitGlobs(glob)
	}
	for _, p := range patterns {
		// A pattern starting with `!` excludes the files it matches (ripgrep's negation), so it
		// never reads a secret; the path above is judged on its own.
		if p == "" || strings.HasPrefix(p, "!") {
			continue
		}
		if s, ok := secretGlob(list, p, tin.Path, in.CWD, opts.Home); ok {
			return deny(RuleSecrets, "%s pattern %q can match the secret %q. %s", in.ToolName,
				p, s.entry, secretsAdvice)
		}
	}
	return Decision{}, false
}

// splitGlobs splits a Grep glob field into its patterns as Claude Code does: at whitespace and
// commas outside `{…}` brace groups, so `!*.md,.env` is two patterns and `*.{ts,tsx}` one.
func splitGlobs(field string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range field {
		switch {
		case r == '{':
			depth++
		case r == '}' && depth > 0:
			depth--
		case depth == 0 && (r == ',' || unicode.IsSpace(r)):
			out = append(out, field[start:i])
			start = i + utf8.RuneLen(r)
		}
	}
	return append(out, field[start:])
}

// secretPath reports the secret p names, resolved against cwd, and with follow also through its
// symlink target.
func secretPath(list []secret, p, cwd, home string, follow bool) (secret, bool) {
	full, abs := paths.Resolve(home, cwd, p, true)
	if !abs || full == paths.ShellRoot {
		full, abs = p, false
	}
	cands := []struct {
		p   string
		abs bool
	}{{full, abs}}
	if follow && abs {
		if target, err := filepath.EvalSymlinks(full); err == nil {
			cands = append(cands, struct {
				p   string
				abs bool
			}{target, true})
		}
	}
	for _, s := range list {
		for _, c := range cands {
			if s.matchPath(c.p, c.abs) {
				return s, true
			}
		}
	}
	return secret{}, false
}

// secretGlob reports the secret a Grep glob or Glob pattern can match: a component naming a bare
// entry, a literal prefix (joined to dir) within a `~` or absolute entry, or a mention of an entry's
// text as in a Bash command.
func secretGlob(list []secret, pattern, dir, cwd, home string) (secret, bool) {
	comps := strings.FieldsFunc(pattern, func(r rune) bool { return r == '/' || r == '\\' })
	prefix := literalPrefix(pattern)
	base := cwd
	if dir != "" {
		if d, ok := paths.Resolve(home, cwd, dir, true); ok {
			base = d
		} else {
			base = ""
		}
	}
	full, abs := "", false
	if prefix != "" || base != "" {
		full, abs = paths.Resolve(home, base, prefix, true)
		if full == paths.ShellRoot {
			abs = false
		}
	}
	for _, s := range list {
		if slices.ContainsFunc(comps, s.matchGlobComponent) ||
			(abs && s.root != "" && paths.Within(full, s.root)) || mentions(pattern, s.text) {
			return s, true
		}
	}
	return secret{}, false
}

// literalPrefix returns the leading components of a glob pattern that hold no glob syntax.
func literalPrefix(pattern string) string {
	end := 0
	for i := 0; i <= len(pattern); i++ {
		if i == len(pattern) || pattern[i] == '/' || pattern[i] == '\\' {
			if strings.ContainsAny(pattern[end:i], "*?[{") {
				break
			}
			end = i
		}
	}
	return pattern[:end]
}

// secretsBash denies a command whose text mentions a secret anywhere outside the handoff ranges:
// in words, quotes, heredoc bodies and an unparsable tail alike.
func secretsBash(command string, handoffs []shell.Range, opts Options) (Decision, bool) {
	text := command
	if len(handoffs) > 0 {
		masked := []byte(command)
		for _, r := range handoffs {
			for i := r.Start; i < r.End && i < len(masked); i++ {
				masked[i] = ' '
			}
		}
		text = string(masked)
	}
	for _, s := range secrets(opts) {
		if mentions(text, s.text) {
			return deny(RuleSecrets, "the command mentions the secret %q. %s", s.entry,
				secretsAdvice)
		}
	}
	return Decision{}, false
}

// mentions reports whether text contains needle on component boundaries: not preceded or followed
// by a name rune (letter, digit, `.`, `-`, `_`). A `/` in needle also matches `\`; case is folded
// where paths folds it.
func mentions(text, needle string) bool {
	if needle == "" {
		return false
	}
	if foldCase {
		text, needle = strings.ToLower(text), strings.ToLower(needle)
	}
	for i := 0; i+len(needle) <= len(text); i++ {
		if !matchAt(text, i, needle) {
			continue
		}
		if i > 0 {
			if r, _ := utf8.DecodeLastRuneInString(text[:i]); isNameRune(r) {
				continue
			}
		}
		if j := i + len(needle); j < len(text) {
			if r, _ := utf8.DecodeRuneInString(text[j:]); isNameRune(r) {
				continue
			}
		}
		return true
	}
	return false
}

func matchAt(text string, i int, needle string) bool {
	for k := range len(needle) {
		c, n := text[i+k], needle[k]
		if c == n || ((n == '/' || n == '\\') && (c == '/' || c == '\\')) {
			continue
		}
		return false
	}
	return true
}

func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_'
}
