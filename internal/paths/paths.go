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

// Expand replaces a leading `~`, `~/` or `~\` with home. `~user` and every other form are returned
// verbatim. Components are kept as written: no lexical `..` removal, which could change the file
// opened through a symlink.
func Expand(home, p string) string {
	s, _ := tilde(home, p, isWindows)
	return s
}

// tilde expands the three `~` forms and reports whether p was one of them.
func tilde(home, p string, windows bool) (string, bool) {
	switch {
	case p == "~":
		return home, true
	case strings.HasPrefix(p, "~/"), strings.HasPrefix(p, `~\`):
		return join(home, p[2:], windows), true
	default:
		return p, false
	}
}

// Resolve turns p into an absolute path for opening, joining a relative p to cwd. A leading `~` is
// expanded only when expand is true (an unquoted shell word or a config value). Components are kept
// as written (no lexical `..` removal); Norm cleans for comparison. On Windows the Git Bash forms
// are mapped first: `/` becomes ShellRoot and `/<drive>[/…]` becomes `<drive>:\…`. ok is false when
// the result is unknown: a relative p with an empty or ShellRoot cwd, or, on Windows, a path under
// the Git Bash root other than a drive, one rooted on the current drive (`\x`), or a drive-relative
// one (`C:x`).
func Resolve(home, cwd, p string, expand bool) (string, bool) {
	return resolve(home, cwd, p, expand, isWindows)
}

func resolve(home, cwd, p string, expand, windows bool) (string, bool) {
	if expand {
		if s, ok := tilde(home, p, windows); ok {
			return s, true
		}
	}
	if windows && strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") {
		return gitBashToWindows(p)
	}
	if isAbs(p, windows) {
		return p, true
	}
	if windows && (hasDrive(p) || strings.HasPrefix(p, `\`)) {
		return "", false
	}
	if cwd == "" || cwd == ShellRoot {
		return "", false
	}
	return join(cwd, p, windows), true
}

// ResolvePowerShell turns p, a path a PowerShell command opens, into an absolute path, with the rules
// of the host PowerShell runs on (windows) and no Git Bash forms. A leading `~`, `~/` or `~\` is
// expanded when home is not "" (pass "" for a word whose `~` is literal); `~\` only on Windows,
// where `\` is a separator. On Windows `\x` and `/x`
// are rooted on the cwd's drive (unknown when the cwd has none) and `C:x` is unknown. A relative p is
// joined to cwd with the separator of the OS this code runs on, turning `\` into it on Windows, so
// the file opens on every OS a test runs on. ok is false when the result is unknown.
func ResolvePowerShell(home, cwd, p string, windows bool) (string, bool) {
	if home != "" && (windows || !strings.HasPrefix(p, `~\`)) {
		if s, ok := tilde(home, hostSeps(p, windows), isWindows); ok {
			return s, true
		}
	}
	switch {
	case !windows:
		if strings.HasPrefix(p, "/") {
			return p, true
		}
	case isAbs(p, true):
		return p, true
	case hasDrive(p):
		return "", false
	case p != "" && isSep(p[0], true):
		if !hasDrive(cwd) {
			return "", false
		}
		return cwd[:2] + strings.ReplaceAll(p, "/", `\`), true
	}
	if cwd == "" {
		return "", false
	}
	return join(cwd, hostSeps(p, windows), isWindows), true
}

// hostSeps turns the `\` separators of a Windows PowerShell path into the separator of the OS this
// code runs on.
func hostSeps(p string, windows bool) string {
	if !windows {
		return p
	}
	return filepath.FromSlash(strings.ReplaceAll(p, `\`, "/"))
}

// isAbs is filepath.IsAbs for the given OS, so both rule sets are testable everywhere.
func isAbs(p string, windows bool) bool {
	if !windows {
		return strings.HasPrefix(p, "/")
	}
	if hasDrive(p) {
		return len(p) > 2 && isSep(p[2], true)
	}
	return len(p) > 1 && isSep(p[0], true) && isSep(p[1], true) // UNC or \\?\
}

func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0])
}

func isSep(c byte, windows bool) bool {
	return c == '/' || (windows && c == '\\')
}

// join appends rel to base with one separator and no cleaning.
func join(base, rel string, windows bool) string {
	if windows {
		rel = strings.ReplaceAll(rel, "/", `\`) // one separator style; components (incl. `..`) kept
	}
	switch {
	case rel == "":
		return base
	case base != "" && isSep(base[len(base)-1], windows):
		return base + rel
	case windows:
		return base + `\` + rel
	default:
		return base + "/" + rel
	}
}

// gitBashToWindows maps a Git Bash absolute path (one leading `/`) to its Windows form: the root
// becomes ShellRoot and `/<drive>/…` becomes `<drive>:\…` with its components kept as written. A
// path whose `..` steps above the drive mount is classified by its cleaned form, as Git Bash does,
// so `/c/..` is ShellRoot. ok is false for any other path under the Git Bash root. It is pure string
// logic so it is testable on every OS.
func gitBashToWindows(p string) (string, bool) {
	first, rest, _ := strings.Cut(p[1:], "/")
	if len(first) == 1 && isASCIILetter(first[0]) && !stepsAbove(rest) {
		return strings.ToUpper(first) + `:\` + strings.ReplaceAll(rest, "/", `\`), true
	}
	switch clean := path.Clean(p); {
	case clean == "/":
		return ShellRoot, true
	case clean != p:
		return gitBashToWindows(clean) // clean is a fixed point of path.Clean, so this ends
	default:
		return "", false
	}
}

// stepsAbove reports whether the slash-separated relative path climbs above its start with `..`.
func stepsAbove(rel string) bool {
	depth := 0
	for c := range strings.SplitSeq(rel, "/") {
		switch c {
		case "", ".":
		case "..":
			if depth--; depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// Norm returns p lexically cleaned with `/` separators, case-folded where the platform file system
// is case-insensitive by default (darwin, windows). On Windows backslash is a separator too, and the
// volume (`C:` or `//host/share`) is kept whole so `..` never climbs out of it.
func Norm(p string) string {
	return norm(p, isWindows, foldCase)
}

func norm(p string, windows, fold bool) string {
	var vol string
	if windows {
		p = strings.ReplaceAll(p, `\`, "/")
		vol = windowsVolume(p)
		p = p[len(vol):]
	}
	if p != "" {
		p = path.Clean(p)
	}
	if p == "/" && strings.HasPrefix(vol, "//") {
		p = "" // `//host/share/` and `//host/share` name the same share root
	}
	s := vol + p
	if fold {
		s = strings.ToLower(s)
	}
	return s
}

// windowsVolume returns the volume of a slash-separated Windows path: `C:`, `//host/share`, or "".
func windowsVolume(p string) string {
	if hasDrive(p) {
		return p[:2]
	}
	if len(p) < 3 || !strings.HasPrefix(p, "//") || p[2] == '/' {
		return ""
	}
	host, rest, ok := strings.Cut(p[2:], "/")
	if !ok {
		return p
	}
	share, _, _ := strings.Cut(rest, "/")
	return "//" + host + "/" + share
}

// Same reports whether a and b name the same path after Norm.
func Same(a, b string) bool {
	return Norm(a) == Norm(b)
}

// NormFor is Norm with the path rules of a named host: Windows rules (case folded, `\` a separator,
// the volume kept whole) when windows, else POSIX rules with the case rule of the OS this code runs
// on (folded on darwin). A PowerShell check uses it to judge Windows paths on any CI OS.
func NormFor(p string, windows bool) string {
	return norm(p, windows, windows || runtime.GOOS == "darwin")
}

// SameFor reports whether a and b name the same path after NormFor.
func SameFor(a, b string, windows bool) bool {
	return NormFor(a, windows) == NormFor(b, windows)
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

// FromConfig resolves a path written in the config: `~` expands with Expand, and a relative result
// is taken against the config file's directory, so it does not depend on the working directory.
func FromConfig(home, p string) string {
	path := Expand(home, p)
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(ConfigFile(home)), path)
	}
	return path
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
	afterWorktrees := false
	for c := range strings.SplitSeq(norm(p, isWindows, false), "/") {
		if afterWorktrees && len(c) > len("agent-") && strings.HasPrefix(c, "agent-") {
			return true
		}
		afterWorktrees = strings.HasSuffix(c, "-worktrees")
	}
	return false
}
