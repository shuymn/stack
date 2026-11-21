// Package paths resolves the file arguments and config values of the extensions: `~` expansion
// and the Git Bash `/c/…` forms on Windows. It copies that part of the core's internal/paths.
package paths

import (
	"path"
	"runtime"
	"strings"
)

// ShellRoot is what Resolve returns on Windows for the Git Bash root `/`, which is not a drive root.
// It contains a NUL byte, so it never names a real file.
const ShellRoot = "\x00git-bash-root"

const isWindows = runtime.GOOS == "windows"

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
