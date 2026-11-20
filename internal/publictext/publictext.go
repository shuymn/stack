// Package publictext finds local home paths (POSIX, Windows, Git Bash and WSL forms) in text that
// is about to be published.
//
// A home-path hit starts a token: it is at the start of the text, or the rune before it is not a
// Unicode letter or digit and not `.`, `-` or `_`, so `資料/home/demo` and `dot_home/x` pass. It ends
// with a name component of Unicode letters, digits and `._-`, plus the separator after it if any.
// Placeholders are not names: a component that is exactly `...`, or one that starts with a rune
// outside the name set (`<name>`, `…`, `{name}`, `$USER`), which leaves the name empty. The forms are
//
//	/Users/<name>  /home/<name>  /Volumes/<name>
//	C:\Users\<name>  C:/Users/<name>     (any drive, case-insensitive, `\` repeated as in JSON)
//	/c/Users/<name>  /mnt/c/Users/<name> (Git Bash and WSL, case-insensitive)
//
// plus the per-user temp forms `/tmp/claude-<digits>/` and the macOS `/private/var/folders/<name>`,
// which need no token start. `~/x` and repository paths such as `home/dot_x` are not local paths.
package publictext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// noHit is what the matchers return when no hit starts at the given index.
const noHit = -1

// LocalPaths returns the local-path hits in s, in order and without overlaps.
func LocalPaths(s string) []string {
	var hits []string
	for i := 0; i < len(s); i++ {
		if end := matchAt(s, i); end != noHit {
			hits = append(hits, s[i:end])
			i = end - 1
		}
	}
	return hits
}

// matchAt returns the end of a hit starting at byte i, or noHit.
func matchAt(s string, i int) int {
	c := s[i]
	if c == '/' {
		if end := tempDir(s, i); end != noHit {
			return end
		}
	}
	if (c != '/' && !isASCIILetter(c)) || !tokenStart(s, i) {
		return noHit
	}
	if c == '/' {
		return homePOSIX(s, i)
	}
	return homeDrive(s, i)
}

// tempDir matches `/tmp/claude-<digits>/` and `/private/var/folders/<name>` at i.
func tempDir(s string, i int) int {
	if j, ok := prefix(s, i, "/private/var/folders/", false); ok {
		return name(s, j)
	}
	j, ok := prefix(s, i, "/tmp/claude-", false)
	if !ok {
		return noHit
	}
	k := j
	for k < len(s) && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k == j || k == len(s) || s[k] != '/' {
		return noHit
	}
	return k + 1
}

// homePOSIX matches /Users, /home, /Volumes, /<drive>/Users and /mnt/<drive>/Users at i.
func homePOSIX(s string, i int) int {
	for _, p := range []string{"/Users/", "/home/", "/Volumes/"} {
		if j, ok := prefix(s, i, p, false); ok {
			return name(s, j)
		}
	}
	j := i + 1
	if k, ok := prefix(s, i, "/mnt/", false); ok {
		j = k
	}
	if j+1 < len(s) && isASCIILetter(s[j]) && s[j+1] == '/' {
		if k, ok := prefix(s, j+len("c/"), "users/", true); ok {
			return name(s, k)
		}
	}
	return noHit
}

// homeDrive matches `<drive>:` then `Users` between separators at i.
func homeDrive(s string, i int) int {
	if i+1 >= len(s) || s[i+1] != ':' {
		return noHit
	}
	j, ok := separator(s, i+len("C:"))
	if !ok {
		return noHit
	}
	if j, ok = prefix(s, j, "users", true); !ok {
		return noHit
	}
	if j, ok = separator(s, j); !ok {
		return noHit
	}
	return name(s, j)
}

// prefix matches the ASCII word p at i, ignoring case when fold is set.
func prefix(s string, i int, p string, fold bool) (int, bool) {
	if len(s)-i < len(p) {
		return i, false
	}
	w := s[i : i+len(p)]
	if w == p || (fold && strings.EqualFold(w, p)) {
		return i + len(p), true
	}
	return i, false
}

// separator skips one `/` or a run of `\` at i.
func separator(s string, i int) (int, bool) {
	switch {
	case i < len(s) && s[i] == '/':
		return i + 1, true
	case i < len(s) && s[i] == '\\':
		for i < len(s) && s[i] == '\\' {
			i++
		}
		return i, true
	default:
		return i, false
	}
}

// name matches the name component at i and the separator after it, returning the end of the hit,
// or noHit for an empty name or the `...` placeholder.
func name(s string, i int) int {
	j := i
	for j < len(s) {
		r, size := utf8.DecodeRuneInString(s[j:])
		if !isNameRune(r) {
			break
		}
		j += size
	}
	if j == i || s[i:j] == "..." {
		return noHit
	}
	end, _ := separator(s, j)
	return end
}

// tokenStart reports whether a home-path hit may start at i.
func tokenStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return !isNameRune(r)
}

func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_'
}

func isASCIILetter(c byte) bool {
	c |= 0x20
	return c >= 'a' && c <= 'z'
}
