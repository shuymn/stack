package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/shuymn/stack/extensions/internal/proc"
)

// Lookup reasons (§3.2 Duplicates): an unassessed duplicate check, or a new item with no index.
const (
	ReasonCwdGone        = "cwd gone"
	ReasonCwdUnknown     = "cwd unknown"
	ReasonLayout         = "unsupported layout"
	ReasonOverride       = "project memory override"
	ReasonSettings       = "settings unreadable"
	ReasonIndexBad       = "index unreadable"
	ReasonNoMemory       = "no memory yet"
	memoryIndexFile      = "MEMORY.md"
	settingsKey          = "autoMemoryDirectory"
	indexLines           = 200
	indexBytes           = 25 << 10
	claudeConfigEnv      = "CLAUDE_CONFIG_DIR"
	gitNotRepositoryExit = 128
)

// Memory is the outcome of one cwd's memory lookup: the index to send, or a reason. With
// Outcome "new" the item needs no request (no memory yet); with "unassessed" the check cannot
// run.
type Memory struct {
	Index   string
	Outcome string
	Reason  string
	// Dir is the memory directory, when one was established.
	Dir string
}

// Lookup finds the memory directory of a cwd only as §3.2 establishes it.
type Lookup struct {
	Runner proc.Runner
	// Git is the git executable; "" means git.
	Git string
	// Env is git's base environment; the repository-location variables are dropped from it.
	Env []string
	// Config is the Claude configuration directory; Home expands a "~/" setting.
	Config, Home string

	cache map[string]Memory
	// slow is set by layout when git ran into the deadline; Find resets it per lookup.
	slow bool
}

// ClaudeConfig is $CLAUDE_CONFIG_DIR, else .claude under home ("" when neither is known).
func ClaudeConfig(getenv func(string) string, home string) string {
	if getenv != nil {
		if d := getenv(claudeConfigEnv); d != "" {
			return d
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// ProjectName encodes a main checkout's path as Claude Code names its project directory: every
// byte outside [A-Za-z0-9] becomes '-' (**assumption**, observed).
func ProjectName(path string) string {
	b := []byte(path)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// Find returns cwd's memory: layout, settings, then the index.
func (l *Lookup) Find(ctx context.Context, cwd string) Memory {
	if l.cache == nil {
		l.cache = map[string]Memory{}
	}
	if m, ok := l.cache[cwd]; ok {
		return m
	}
	m := l.find(ctx, cwd)
	if m.Reason != ReasonDeadline {
		l.cache[cwd] = m
	}
	return m
}

func unassessed(reason string) Memory { return Memory{Outcome: DupUnassessed, Reason: reason} }

func (l *Lookup) find(ctx context.Context, cwd string) Memory {
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return unassessed(ReasonCwdGone)
	}
	l.slow = false
	top, main, ok := l.layout(ctx, cwd)
	if l.slow {
		return unassessed(ReasonDeadline)
	}
	if !ok {
		return unassessed(ReasonLayout)
	}
	if l.Config == "" {
		return unassessed(ReasonSettings)
	}
	// A repository setting the location in its own settings files is honoured by Claude Code
	// only under trust and its read boundary, neither visible here.
	seen := map[string]bool{}
	for _, dir := range []string{cwd, top, main} {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		for _, name := range []string{"settings.json", "settings.local.json"} {
			set, err := settingsValue(filepath.Join(dir, ".claude", name))
			if err != nil {
				return unassessed(ReasonSettings)
			}
			if set != nil {
				return unassessed(ReasonOverride)
			}
		}
	}
	memDir := filepath.Join(l.Config, "projects", ProjectName(main), "memory")
	set, err := settingsValue(filepath.Join(l.Config, "settings.json"))
	if err != nil {
		return unassessed(ReasonSettings)
	}
	projectDir := filepath.Dir(memDir)
	if set != nil {
		v, isString := set.(string)
		switch {
		case !isString:
			return unassessed(ReasonSettings)
		case strings.HasPrefix(v, "~/") && l.Home != "":
			memDir = filepath.Join(l.Home, filepath.FromSlash(v[2:]))
		case filepath.IsAbs(v):
			memDir = filepath.Clean(v)
		default:
			return unassessed(ReasonSettings)
		}
		projectDir = memDir
	}
	if _, err := os.Stat(projectDir); errors.Is(err, fs.ErrNotExist) {
		return Memory{Outcome: DupNew, Reason: ReasonNoMemory, Dir: memDir}
	}
	data, err := os.ReadFile(filepath.Join(memDir, memoryIndexFile))
	switch {
	case errors.Is(err, fs.ErrNotExist) || err == nil && len(bytes.TrimSpace(data)) == 0:
		return Memory{Outcome: DupNew, Reason: ReasonNoMemory, Dir: memDir}
	case err != nil:
		return Memory{Outcome: DupUnassessed, Reason: ReasonIndexBad, Dir: memDir}
	}
	return Memory{Index: loadedIndex(data), Dir: memDir}
}

// loadedIndex is what Claude Code loads at session start: the first 200 lines or 25 KB,
// whichever comes first, cut at a UTF-8 boundary.
func loadedIndex(data []byte) string {
	n := 0
	for i := 0; i < len(data) && n < indexLines; i++ {
		if data[i] == '\n' {
			n++
			if n == indexLines {
				data = data[:i+1]
			}
		}
	}
	if len(data) > indexBytes {
		i := indexBytes
		for i > 0 && !utf8.RuneStart(data[i]) {
			i--
		}
		data = data[:i]
	}
	return string(data)
}

// settingsValue returns the autoMemoryDirectory value of a settings file: nil when the file or
// the key is absent, an error when the file is unreadable or malformed.
func settingsValue(path string) (any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // an absent file sets nothing
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // mapped to a fixed reason
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err //nolint:wrapcheck // mapped to a fixed reason
	}
	return doc[settingsKey], nil
}

// discoveryEnv is env without what redirects or stops git's repository discovery: the
// repository-location variables and the discovery limits, so the cwd alone decides the layout.
func discoveryEnv(env []string) []string {
	return slices.DeleteFunc(proc.DropRepoVars(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(name, "GIT_CEILING_DIRECTORIES") ||
			strings.EqualFold(name, "GIT_DISCOVERY_ACROSS_FILESYSTEM")
	})
}

// layout recognises only an ordinary checkout and a linked worktree registered back to its
// common directory (§3.2 Duplicates, step 1); a cwd outside any repository is its own main
// checkout. ok is false for any other layout.
func (l *Lookup) layout(ctx context.Context, cwd string) (string, string, bool) {
	name := l.Git
	if name == "" {
		name = "git"
	}
	cctx, cancel := childContext(ctx)
	defer cancel()
	res, err := l.Runner.Run(cctx, proc.Cmd{
		Name: name, Args: []string{"rev-parse", "--show-toplevel"}, Dir: cwd,
		Env: discoveryEnv(l.Env),
	})
	switch {
	case res.TimedOut || res.WaitDelayExceeded || ctx.Err() != nil:
		// A slow git is the work deadline's, not a layout.
		l.slow = true
		return "", "", false
	case err != nil || res.Truncated:
		return "", "", false
	case res.ExitCode == gitNotRepositoryExit &&
		bytes.Contains(bytes.ToLower(res.Stderr), []byte("not a git repository")):
		return "", cwd, true
	case res.ExitCode != 0:
		return "", "", false
	}
	top := filepath.Clean(filepath.FromSlash(strings.TrimRight(string(res.Stdout), "\r\n")))
	if top == "." || top == "" {
		return "", "", false
	}
	dotGit := filepath.Join(top, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return "", "", false
	}
	if info.IsDir() {
		return top, top, true
	}
	if !info.Mode().IsRegular() {
		return "", "", false
	}
	gitdir, ok := gitfile(dotGit, top)
	if !ok {
		return "", "", false
	}
	// gitdir must be <common>/worktrees/<name>, <common> a directory named .git.
	worktrees := filepath.Dir(gitdir)
	common := filepath.Dir(worktrees)
	if filepath.Base(worktrees) != "worktrees" || filepath.Base(common) != ".git" {
		return "", "", false
	}
	if ci, err := os.Stat(common); err != nil || !ci.IsDir() {
		return "", "", false
	}
	back, err := os.ReadFile(filepath.Join(gitdir, "gitdir"))
	if err != nil {
		return "", "", false
	}
	target := filepath.FromSlash(strings.TrimRight(string(back), "\r\n"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitdir, target)
	}
	if !samePath(target, dotGit) {
		return "", "", false
	}
	main := filepath.Dir(common)
	if mi, err := os.Stat(main); err != nil || !mi.IsDir() {
		return "", "", false
	}
	return top, main, true
}

// gitfile reads a `gitdir: <path>` file, a relative path resolved against dir.
func gitfile(path, dir string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	v, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
	if !ok || v == "" {
		return "", false
	}
	p := filepath.FromSlash(v)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p), true
}

// samePath compares two paths after resolving symlinks, so a temp directory reached through a
// link (macOS /var) compares equal to its target.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
