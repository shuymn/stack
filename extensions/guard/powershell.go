package guard

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/powershell"
)

const secretsAdvicePowerShell = "Commands must not show a secret file. No PowerShell form passes a " +
	"secret file; use the Bash tool's `K=\"$(cat <file>)\" cmd` handoff where it exists, or ask " +
	"the user."

// windowsHost is the Options.OS of a Windows host; any other value is a Unix one.
const windowsHost = powershell.Windows

// decidePowerShell judges a PowerShell command (docs/design/powershell-guard.md §4): secrets over
// the whole text, then gh-publish and rmroot over the recognizer's calls, then each nested Bash
// script through the Bash path at the depth it runs at. A command the recognizer cannot lex yields
// no calls, so only secrets judges it.
func decidePowerShell(command, cwd string, opts Options) (Decision, bool) {
	if d, ok := secretsPowerShell(command, opts); ok {
		return d, true
	}
	parsed := powershell.Parse(command, cwd, opts.Home, opts.OS)
	resolve := powerShellResolver(opts.OS)
	var calls []ghCall
	for _, c := range parsed.Calls {
		if c.Is("gh") {
			calls = appendGh(calls, c.ShellCall(), resolve)
		}
	}
	if d, ok := ghPublish(calls, opts); ok {
		return d, true
	}
	if d, ok := rmRootPowerShell(parsed.Calls, opts); ok {
		return d, true
	}
	for _, s := range parsed.Nested {
		if s.Dialect != powershell.Bash {
			continue
		}
		if d, ok := decideBash(s.Text, s.Cwd, s.Depth, opts); ok {
			return d, true
		}
	}
	return Decision{}, false
}

// powerShellResolver resolves with PowerShell's path rules on host os: no Git Bash forms, and a `~`
// the adapter did not mark as home is a literal name.
func powerShellResolver(os powershell.OS) resolver {
	return func(home, cwd, p string, expand bool) (string, bool) {
		if !expand {
			home = ""
		}
		return paths.ResolvePowerShell(home, cwd, p, os == windowsHost)
	}
}

// secretsPowerShell denies a command whose text mentions a secret anywhere: there is no handoff
// form (powershell-guard §5). On Windows case is folded and a run of separators is one: the text
// and each entry are collapsed once, so a match cannot start inside a run and the scan is linear in
// the text. Only an entry's text is used; no path is resolved.
func secretsPowerShell(command string, opts Options) (Decision, bool) {
	windows := opts.OS == windowsHost
	fold := foldCase || windows
	// The text is normalized once; each needle the same way.
	norm := func(s string) string {
		if windows {
			s = collapseSeps(s)
		}
		if fold {
			s = strings.ToLower(s)
		}
		return s
	}
	text := norm(command)
	for _, e := range opts.Safety.Secrets {
		if mentionsFor(text, norm(mentionText(e)), false) {
			return deny(RuleSecrets, "the command mentions the secret %q. %s", e,
				secretsAdvicePowerShell)
		}
	}
	return Decision{}, false
}

// rmRootPowerShell denies a recursive `Remove-Item` (name or alias) of the home directory, a drive
// root or an existing `.git` directory, and a native `rm` the Bash rule would deny
// (powershell-guard §4.4).
func rmRootPowerShell(calls []powershell.Call, opts Options) (Decision, bool) {
	windows := opts.OS == windowsHost
	var declared drives // drives exist only on Windows
	if windows {
		declared = declaredDrives(calls)
	}
	for _, c := range calls {
		switch {
		case c.Kind == powershell.Cmdlet && c.Is("Remove-Item"):
			if d, ok := removeItem(c, opts, windows, declared); ok {
				return d, true
			}
		case c.Kind == powershell.Native && c.Is("rm"):
			if d, ok := nativeRm(c, opts, windows); ok {
				return d, true
			}
		}
	}
	return Decision{}, false
}

// drives are the drives a script declares with New-PSDrive (`ndr`). A declared drive may be made
// the current one, so a bare root is opaque once any is declared; a declared name's own root is
// opaque too, and every letter's when a name is not static.
type drives struct {
	any, dynamic bool
	names        map[string]bool
}

func declaredDrives(calls []powershell.Call) drives {
	var d drives
	for _, c := range calls {
		// A call pwsh refuses to bind declares nothing.
		if c.Kind != powershell.Cmdlet || !c.Is("New-PSDrive") || !c.Bound || c.Refused {
			continue
		}
		d.any = true
		named := false
		for _, w := range c.Args[1:] {
			if w.Param != "Name" || w.IsParam {
				continue
			}
			named = true
			switch {
			case !w.Static:
				d.dynamic = true
			case len(w.Text) == 1 && isLetter(w.Text[0]): // only a letter can be looked up
				if d.names == nil {
					d.names = map[string]bool{}
				}
				d.names[strings.ToLower(w.Text)] = true
			}
		}
		if !named {
			d.dynamic = true
		}
	}
	return d
}

// opaque reports whether p, a Windows path, lies on a drive the script declares.
func (d drives) opaque(p string) bool {
	if len(p) < 2 || p[1] != ':' || !isLetter(p[0]) {
		return false
	}
	return d.dynamic || d.names[strings.ToLower(p[:1])]
}

func isLetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// removeItem judges one Remove-Item call. A call pwsh refuses to bind, a -WhatIf run and a
// -Recurse or -WhatIf value that is not a literal $true or $false are not judged.
func removeItem(c powershell.Call, opts Options, windows bool, declared drives) (Decision, bool) {
	if !c.Bound || c.Refused {
		return Decision{}, false
	}
	recursive := false
	for _, w := range c.Args[1:] {
		if w.Param != "Recurse" && w.Param != "WhatIf" {
			continue
		}
		on, ok := switchValue(w)
		if !ok || (w.Param == "WhatIf" && on) {
			return Decision{}, false
		}
		recursive = recursive || (w.Param == "Recurse" && on)
	}
	if !recursive {
		return Decision{}, false
	}
	for _, w := range c.Args[1:] {
		if (w.Param != "Path" && w.Param != "LiteralPath") || w.IsParam {
			continue
		}
		if what, ok := removeItemTarget(w, c.Cwd, opts.Home, windows, declared); ok {
			return deny(RuleRmRoot, "Remove-Item would recursively delete %s (%s). Delete only "+
				"what the task needs, by its own path.", what, w.Raw)
		}
	}
	return Decision{}, false
}

// switchValue returns a switch parameter's value: true for `-Name`, the literal of `-Name:$true`
// or `-Name:$false`, and false for any other colon-bound value.
func switchValue(w powershell.Word) (bool, bool) {
	if w.Flag == "" {
		return true, true
	}
	switch strings.ToLower(strings.TrimPrefix(w.Raw, w.Flag)) {
	case "$true":
		return true, true
	case "$false":
		return false, true
	}
	return false, false
}

// removeItemTarget names what a Remove-Item path operand is when it must not be deleted. Operands
// use FileSystem-provider syntax on every host (`\` and `/` both separators).
func removeItemTarget(
	w powershell.Word,
	cwd, home string,
	windows bool,
	declared drives,
) (string, bool) {
	if w.Block {
		return "", false
	}
	switch w.Known {
	case powershell.KnownHome, powershell.KnownUserProfile:
		if onlyCurrent(w.Text) {
			return "the home directory", true
		}
		return "", false
	case powershell.KnownSystemDrive:
		if windows && w.Text != "" && isSep(w.Text[0]) && onlyCurrent(w.Text) {
			return "a drive root", true
		}
		return "", false
	case powershell.NotKnown:
	}
	p := w.Text
	switch {
	case !w.Static,
		w.Param == "Path" && strings.ContainsAny(p, "*?["),    // a glob
		w.Param == "LiteralPath" && strings.HasPrefix(p, "~"), // expansion unconfirmed (W3)
		strings.Contains(p, "::"),                             // provider-qualified
		len(p) > 1 && isSep(p[0]) && isSep(p[1]):              // UNC or `\\?\`
		return "", false
	}
	if windows && p != "" && isSep(p[0]) && onlyCurrent(p) {
		// The root of the current drive, whatever that drive is.
		if declared.any {
			return "", false
		}
		return "a drive root", true
	}
	if i := strings.IndexByte(p, ':'); i > 1 {
		return "", false // a drive other than a letter: another provider (`HKCU:\`)
	}
	if !windows {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	full, ok := paths.ResolvePowerShell(home, cwd, p, windows)
	if !ok || (windows && declared.opaque(full)) { // after resolving: a cwd on a declared drive too
		return "", false
	}
	return rootKindFor(full, home, windows)
}

// onlyCurrent reports whether p is only separators and `.` components.
func onlyCurrent(p string) bool {
	for c := range strings.FieldsFuncSeq(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c != "." {
			return false
		}
	}
	return true
}

// nativeRm judges a native `rm` call with the Bash rule's options and native argv: a quoted `~` is
// literal, and an unquoted one is home on non-Windows hosts (the adapter leaves it dynamic on
// Windows).
func nativeRm(c powershell.Call, opts Options, windows bool) (Decision, bool) {
	sc := c.ShellCall()
	recursive, operands := rmArgs(sc.Args[1:])
	if !recursive {
		return Decision{}, false
	}
	for _, op := range operands {
		home := ""
		if op.Tilde {
			home = opts.Home
		}
		p, ok := paths.ResolvePowerShell(home, sc.Cwd, op.Text, windows)
		if !ok {
			continue
		}
		if what, ok := rootKindFor(p, opts.Home, windows); ok {
			return deny(RuleRmRoot, "rm would recursively delete %s (%s). Delete only what "+
				"the task needs, by its own path.", what, op.Text)
		}
	}
	return Decision{}, false
}

// rootKindFor is rootKind with the path rules of the host PowerShell runs on.
func rootKindFor(p, home string, windows bool) (string, bool) {
	n := paths.NormFor(p, windows)
	switch {
	case !windows && n == "/":
		return "the root directory", true
	case windows && len(n) == 3 && n[1] == ':' && n[2] == '/':
		return "a drive root", true
	case home != "" && n == paths.NormFor(home, windows):
		return "the home directory", true
	default:
		// The host's own path rules, since only a host path can be an existing directory. The
		// same trimmed path is checked, so a `.git` symlink is not followed through `/.`.
		t := trimCurrent(p, windows)
		if paths.NormFor(filepath.Base(t), windows) != ".git" {
			return "", false
		}
		if fi, err := os.Lstat(t); err == nil && fi.IsDir() {
			return "a .git directory", true
		}
	}
	return "", false
}

// trimCurrent removes p's terminal `.` components and separators, keeping a root.
func trimCurrent(p string, windows bool) string {
	isSep := func(c byte) bool {
		return c == '/' || ((windows || filepath.Separator == '\\') && c == '\\')
	}
	for {
		end := len(p)
		for end > 1 && isSep(p[end-1]) {
			end--
		}
		if end >= 2 && p[end-1] == '.' && isSep(p[end-2]) {
			p = p[:end-1]
			continue
		}
		return p[:end]
	}
}
