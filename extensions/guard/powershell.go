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
	if d, ok := rmRootPowerShell(parsed.Calls, cwd, opts); ok {
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
// the adapter did not mark as home is a literal name, and one it marked is unknown without a home.
func powerShellResolver(os powershell.OS) resolver {
	return func(home, cwd, p string, expand bool) (string, bool) {
		return paths.ResolvePowerShell(home, cwd, p, expand, os == windowsHost)
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
func rmRootPowerShell(calls []powershell.Call, cwd string, opts Options) (Decision, bool) {
	windows := opts.OS == windowsHost
	var declared drives // drives exist only on Windows
	if windows {
		declared = declaredDrives(calls, existingDrives(cwd, opts.Home, opts.Drives))
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

// drives are the drives a script declares with New-PSDrive (`ndr`, `mount`). A declared drive may
// be made the current one, so a bare root is opaque once any is declared; a declared name's own
// root is opaque too, and every letter's when a name is not static.
type drives struct {
	hasCustomDriveDeclaration, dynamic bool
	names                              map[string]bool
}

// existingDrives returns the lower-case letters of the host's volumes (letters) and of the drives
// the payload cwd and home lie on: volumes PowerShell has mounted as FileSystem drives before the
// script runs.
func existingDrives(cwd, home, letters string) map[string]bool {
	m := map[string]bool{}
	for _, p := range []string{cwd, home} {
		if len(p) >= 2 && p[1] == ':' && isLetter(p[0]) {
			m[strings.ToLower(p[:1])] = true
		}
	}
	for i := range len(letters) {
		if isLetter(letters[i]) {
			m[strings.ToLower(letters[i:i+1])] = true
		}
	}
	return m
}

// declaredDrives collects the drives calls declare. A declaration whose static name is an existing
// drive fails (DriveAlreadyExists) and declares nothing, as does a call pwsh refuses to bind.
func declaredDrives(calls []powershell.Call, existing map[string]bool) drives {
	var d drives
	for _, c := range calls {
		if c.Kind != powershell.Cmdlet || !c.Is("New-PSDrive") || !c.Bound || c.Refused {
			continue
		}
		var name *powershell.Word
		for i, w := range c.Args[1:] {
			if w.Param == "Name" && !w.IsParam {
				name = &c.Args[i+1]
			}
		}
		switch {
		case name == nil || !name.Static:
			d.dynamic = true
		case existing[strings.ToLower(name.Text)]:
			continue
		case len(name.Text) == 1 && isLetter(name.Text[0]): // only a letter can be looked up
			if d.names == nil {
				d.names = map[string]bool{}
			}
			d.names[strings.ToLower(name.Text)] = true
		}
		d.hasCustomDriveDeclaration = true
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
		w.Param == "Path" && strings.ContainsAny(p, "*?["), // a glob
		w.Param == "LiteralPath" && literalTildeOpaque(p, windows),
		strings.Contains(p, "::"),                // provider-qualified
		len(p) > 1 && isSep(p[0]) && isSep(p[1]): // UNC or `\\?\`
		return "", false
	}
	if windows && p != "" && isSep(p[0]) && onlyCurrent(p) {
		// The root of the current drive, whatever that drive is.
		if declared.hasCustomDriveDeclaration {
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
	full, ok := paths.ResolvePowerShell(home, cwd, p, true, windows)
	if !ok || (windows && declared.opaque(full)) { // after resolving: a cwd on a declared drive too
		return "", false
	}
	return rootKindFor(full, home, windows)
}

// literalTildeOpaque reports a `-LiteralPath` value whose `~` expansion is unmeasured, so it is not
// judged. On Windows only `~` with at most trailing separators was measured to name home (W3), so
// every other value starting with `~` is opaque; elsewhere nothing was measured, so the home forms
// `~`, `~/…` and `~\…` are opaque and `~user` is a literal name.
func literalTildeOpaque(p string, windows bool) bool {
	if windows {
		return strings.HasPrefix(p, "~") && strings.Trim(p[1:], `\/`) != ""
	}
	return paths.TildeForm(p, true)
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
// literal, and a plain unquoted leading one is home on every host (ShellCallRm); with no known home
// such an operand is not judged.
func nativeRm(c powershell.Call, opts Options, windows bool) (Decision, bool) {
	sc := c.ShellCallRm()
	recursive, operands := rmArgs(sc.Args[1:])
	if !recursive {
		return Decision{}, false
	}
	for _, op := range operands {
		p, ok := paths.ResolvePowerShell(opts.Home, sc.Cwd, op.Text, op.Tilde, windows)
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
