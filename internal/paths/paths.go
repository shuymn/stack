// Package paths holds the home directory, `~` expansion, path resolution and comparison, the state and
// config locations, and the slug, session-id and agent-worktree rules shared by every command.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ShellRoot is what Resolve returns on Windows for the Git Bash root `/`, which is not a drive root.
// It contains a NUL byte, so it never names a real file.
const ShellRoot = "\x00git-bash-root"

const isWindows = runtime.GOOS == "windows"

// foldCase reports whether the platform file system is case-insensitive by default.
const foldCase = runtime.GOOS == "darwin" || isWindows

var (
	// ErrNoHome is returned when the home directory cannot be determined.
	ErrNoHome = errors.New("home directory is not set")

	slugRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	deviceRe    = regexp.MustCompile(`^(con|nul|aux|prn|com[1-9]|lpt[1-9])$`)
)

// Home returns the user's home directory ($HOME; %USERPROFILE% on Windows).
func Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoHome, err)
	}
	if home == "" {
		return "", ErrNoHome
	}
	return home, nil
}

// Expand replaces a leading `~`, `~/` or `~\` with home and cleans the result. `~user` and every
// other form are returned cleaned but otherwise verbatim.
func Expand(home, p string) string {
	switch {
	case p == "~":
		return filepath.Clean(home)
	case strings.HasPrefix(p, "~/"), strings.HasPrefix(p, `~\`):
		return filepath.Join(home, p[2:])
	default:
		return filepath.Clean(p)
	}
}

// Resolve turns p into an absolute, cleaned path, joining a relative p to cwd. A leading `~` is
// expanded only when expand is true (an unquoted shell word or a config value). On Windows the Git
// Bash forms are mapped first: `/` becomes ShellRoot and `/<drive>[/…]` becomes `<drive>:\…`.
// ok is false when the result is unknown: a relative p with an empty or ShellRoot cwd, or, on
// Windows, a path under the Git Bash root other than a drive, or one rooted on the current drive.
func Resolve(home, cwd, p string, expand bool) (string, bool) {
	if expand {
		if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
			return Expand(home, p), true
		}
	}
	if isWindows {
		if mapped, kind := gitBashToWindows(p); kind != gitBashNone {
			return mapped, kind != gitBashUnknown
		}
		if (strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/")) && !filepath.IsAbs(p) {
			return "", false
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), true
	}
	if cwd == "" || cwd == ShellRoot {
		return "", false
	}
	return filepath.Join(cwd, p), true
}

type gitBashKind int

const (
	gitBashNone gitBashKind = iota
	gitBashRoot
	gitBashDrive
	gitBashUnknown
)

// gitBashToWindows maps a Git Bash absolute path (forward slash, not `//`) to its Windows form.
// It is pure string logic so it is testable on every OS.
func gitBashToWindows(p string) (string, gitBashKind) {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "", gitBashNone
	}
	clean := path.Clean(p)
	if clean == "/" {
		return ShellRoot, gitBashRoot
	}
	first, rest, _ := strings.Cut(clean[1:], "/")
	if len(first) != 1 || !isASCIILetter(first[0]) {
		return "", gitBashUnknown
	}
	return strings.ToUpper(first) + `:\` + strings.ReplaceAll(rest, "/", `\`), gitBashDrive
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// Norm returns p cleaned with `/` separators, case-folded where the platform file system is
// case-insensitive by default (darwin, windows). Backslash is a separator only on Windows.
func Norm(p string) string {
	return norm(p, isWindows, foldCase)
}

func norm(p string, windows, fold bool) string {
	if windows {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	var lead, vol string
	if windows && strings.HasPrefix(p, "//") {
		lead = "/" // UNC: path.Clean would collapse the leading `//`
	}
	if windows && len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		vol, p = p[:2], p[2:]
	}
	if p != "" {
		p = path.Clean(p)
	}
	s := lead + vol + p
	if fold {
		s = strings.ToLower(s)
	}
	return s
}

// Same reports whether a and b name the same path after Norm.
func Same(a, b string) bool {
	return Norm(a) == Norm(b)
}

// Within reports whether p is root or lies under it, comparing whole components, so
// `a/secrets-example` is not within `a/secrets`.
func Within(p, root string) bool {
	return within(Norm(p), Norm(root))
}

func within(np, nr string) bool {
	if nr == "" {
		return false
	}
	if np == nr {
		return true
	}
	if strings.HasSuffix(nr, "/") {
		return strings.HasPrefix(np, nr)
	}
	return strings.HasPrefix(np, nr+"/")
}

// HasComponent reports whether one of p's components equals name after Norm.
func HasComponent(p, name string) bool {
	return hasComponent(Norm(p), Norm(name))
}

func hasComponent(np, nname string) bool {
	for c := range strings.SplitSeq(np, "/") {
		if c == nname {
			return true
		}
	}
	return false
}

// ConfigFile is `~/.config/stack/config.toml`. XDG variables are not consulted.
func ConfigFile(home string) string {
	return filepath.Join(home, ".config", "stack", "config.toml")
}

// StateRoot is `~/.local/state/stack`.
func StateRoot(home string) string {
	return filepath.Join(home, ".local", "state", "stack")
}

// SessionsDir holds the per-session `.kits` files and their locks.
func SessionsDir(home string) string {
	return filepath.Join(StateRoot(home), "sessions")
}

// SessionKitsFile is `<sessions>/<id>.kits`, using the lower-cased id so one id in two cases maps
// to one file on every OS. The caller validates id with ValidSessionID.
func SessionKitsFile(home, id string) string {
	return filepath.Join(SessionsDir(home), strings.ToLower(id)+".kits")
}

// SessionLockFile is the lock guarding SessionKitsFile.
func SessionLockFile(home, id string) string {
	return filepath.Join(SessionsDir(home), strings.ToLower(id)+".lock")
}

// KitDir is `<root>/<slug>`, where root is the expanded `[kit].root`. The caller validates slug with
// ValidSlug.
func KitDir(root, slug string) string {
	return filepath.Join(root, slug)
}

// ValidSlug reports whether s is a safe kit name on every OS: `^[a-z0-9][a-z0-9._-]{0,63}$`, no
// `..`, no trailing `.`, and no Windows device name before the first `.`.
func ValidSlug(s string) bool {
	if !slugRe.MatchString(s) || strings.Contains(s, "..") || strings.HasSuffix(s, ".") {
		return false
	}
	base, _, _ := strings.Cut(s, ".")
	return !deviceRe.MatchString(base)
}

// ValidSessionID reports whether id is safe as a file name and as a value sourced by bash.
func ValidSessionID(id string) bool {
	return sessionIDRe.MatchString(id)
}

// IsAgentWorktree reports whether p has a component ending in `-worktrees` followed by a component
// `agent-<name>` with a non-empty name.
func IsAgentWorktree(p string) bool {
	return isAgentWorktree(norm(p, isWindows, false))
}

func isAgentWorktree(np string) bool {
	comps := strings.Split(np, "/")
	for i := range len(comps) - 1 {
		if strings.HasSuffix(comps[i], "-worktrees") && len(comps[i+1]) > len("agent-") &&
			strings.HasPrefix(comps[i+1], "agent-") {
			return true
		}
	}
	return false
}
