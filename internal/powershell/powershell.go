// Package powershell recognizes the commands a PowerShell tool command statically runs.
//
// It is a bounded recognizer, not a PowerShell parser (docs/design/powershell-guard.md §3): it
// tokenizes as PowerShell does (quotes, here-strings, comments, backtick escapes, redirections) and
// returns calls only for a finite list of command positions: a statement's first bareword, the
// target of `&` and `.`, the pipeline after `return`, `throw` and `exit`, the first token of each
// statement inside `(…)`, `$(…)` and `@(…)` (also inside expandable strings), the right side of an
// assignment and of a key inside `@{…}`, and the script blocks of `& {…}`, `. {…}`, control-flow
// bodies, `ForEach-Object`, `Where-Object` and a local `Invoke-Command`. Every other construct is
// opaque: it yields no call. A script that fails to tokenize yields no calls at all.
//
// Nested scripts carry their dialect: `Invoke-Expression` and `pwsh -c` with one static string are
// parsed here, and their calls join Calls; `bash|sh|zsh -c` with one static string is returned in
// Nested for the Bash path (shell.ParseDepth) to judge. The depth allowance (2) is shared by both.
package powershell

import (
	"runtime"
	"strings"

	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/shell"
)

// maxDepth bounds the recursion into nested scripts, as in package shell.
const maxDepth = 2

// OS is the host PowerShell runs on, which decides aliases, case and path rules.
type OS int

// Hosts.
const (
	// Unix is any host other than Windows: Linux, macOS, WSL.
	Unix OS = iota
	// Windows is a Windows host.
	Windows
)

// HostOS returns the OS this code runs on.
func HostOS() OS {
	if runtime.GOOS == "windows" {
		return Windows
	}
	return Unix
}

// Kind is a call's command kind.
type Kind int

// Command kinds.
const (
	// Native is any command that is not a cmdlet: an executable, a script, an explicit path, a
	// name ending in `.exe`, or a dynamic name.
	Native Kind = iota
	// Cmdlet is a cmdlet name, module-qualified or not, or a built-in alias of one on the host.
	Cmdlet
)

// Quote is the quoting of a word's first part.
type Quote int

// Quotings.
const (
	Bare Quote = iota
	Single
	Double
	HereSingle
	HereDouble
)

// Known names a variable whose value a check can recognise symbolically.
type Known int

// Known variables.
const (
	NotKnown Known = iota
	// KnownHome is `$HOME` or `${HOME}`.
	KnownHome
	// KnownUserProfile is `$env:USERPROFILE` or `${env:USERPROFILE}`.
	KnownUserProfile
	// KnownSystemDrive is `$env:SystemDrive` or `${env:SystemDrive}`.
	KnownSystemDrive
)

// knownSet marks known variables, indexed by Known.
type knownSet [KnownSystemDrive + 1]bool

// Word is one argument of a call.
type Word struct {
	// Raw is the word's source text.
	Raw string
	// Text is the word's value when Static; for a Known word, the static text after the variable;
	// else "".
	Text string
	// Static reports that the word holds no unescaped `$`, no leading `@` and no subexpression, so
	// Text is exactly what the command receives.
	Static bool
	// Quote is the quoting of the word's first part.
	Quote Quote
	// Known marks a bare or double-quoted word that starts with a known variable (one the script
	// has not assigned before the word) followed only by static text.
	Known Known
	// Flag is the written `-Name:` prefix of a colon-bound cmdlet argument; the rest of the word is
	// the value.
	Flag string
	// IsParam marks a cmdlet parameter token (`-Recurse`) or the `--` that ends parameters.
	IsParam bool
	// Param is the canonical parameter the word binds to after cmdlet binding (for a value or a
	// parameter token), "" when unbound or when the cmdlet has no binding table.
	Param string
	// Group is shared by the elements of one unquoted `,` list of a cmdlet argument; 0 for a word
	// that is not in a list.
	Group int
	// Block marks a `{…}` script block argument.
	Block bool
	// Verbatim marks a word of a `--%` tail.
	Verbatim bool

	blockStart, blockEnd int
	joined               bool // a native argument after a quoted `--%`, passed as written
	blockMoved           bool // the block holds a location change
	glob                 bool // an unquoted wildcard (`*`, `?`, `[`)
}

// Call is one command call.
type Call struct {
	// Args is argv: the command name word, then the arguments without redirections and `--%`.
	// A cmdlet argument's `,` list is one word per element.
	Args []Word
	Kind Kind
	// Name is the normalised command name: for a cmdlet the canonical cmdlet name (alias resolved,
	// module qualifier stripped), for a native command the base name without `.exe`; "" when the
	// name is dynamic.
	Name string
	// Stdin is the pipeline input: StdinText when the element before the call is one static string
	// or here-string, StdinDynamic for any other pipe, StdinNone otherwise.
	Stdin shell.Stdin
	// Cwd is the directory the call runs in, or "" when it is unknown.
	Cwd string
	// Bound reports that the cmdlet has a binding table, so each word's Param is set.
	Bound bool
	// Refused reports that binding failed (an unknown or ambiguous parameter, a missing value, a
	// parameter bound twice, or a positional argument with no slot): pwsh does not run the call.
	Refused bool

	home string
	os   OS
}

// Is reports whether the call's Name is name: case-insensitive for cmdlets and for native commands
// on Windows, exact for native commands elsewhere.
func (c Call) Is(name string) bool {
	if c.Kind == Cmdlet || c.os == Windows {
		return strings.EqualFold(c.Name, name)
	}
	return c.Name == name
}

// ShellCall adapts the call to a shell.Call with native-argv semantics: `$HOME`/`${HOME}` (and on
// Windows `$env:USERPROFILE`/`${env:USERPROFILE}`) are replaced by the hook's home, every other
// known variable is dynamic; a script block is dynamic. A home `~` word (see tildeOf) is Tilde on
// non-Windows hosts and dynamic on Windows, where whether it is expanded depends on the engine
// (PowerShell 7.6+ expands it, 5.1 and 7.0–7.5 pass it as written), so the file it names is
// unknown. A quoted `~`, `~user`, `x~` and a home form holding a blank are literal.
func (c Call) ShellCall() shell.Call {
	return c.shellCall(false)
}

// ShellCallRm is ShellCall for the native-`rm` rule only: on Windows a home `~` word is Tilde too,
// since a miss there deletes home on the expanding engine and the only false deny is a directory
// literally named `~` on an older one.
func (c Call) ShellCallRm() shell.Call {
	return c.shellCall(true)
}

func (c Call) shellCall(windowsTilde bool) shell.Call {
	out := shell.Call{Args: make([]shell.Word, len(c.Args)), Stdin: c.Stdin, Cwd: c.Cwd}
	for i, w := range c.Args {
		out.Args[i] = c.nativeWord(w, windowsTilde)
	}
	return out
}

// tildeKind is how PowerShell 7 treats a native word starting with `~`.
type tildeKind int

const (
	tildeNone    tildeKind = iota // passed as written
	tildeHome                     // expanded as home (on Windows from 7.6)
	tildeUnknown                  // a home form holding `"`: unmeasured
)

// tildeOf classifies a native word by its decoded value, as PowerShell 7.7 does (argv stub): a
// word whose first part is unquoted, outside a `--%` tail and not after a quoted `--%`, and whose
// value is exactly `~`, `~/…` or, on Windows, `~\…` (paths.TildeForm) is home, whatever quotes or
// backtick escapes spell it (“ `~ “, “ ~\`. “, `~'\x'`, `~"\x"`), unless the value holds a blank
// (`~'\a b'` is passed as written). `~'foo'/.git` is the literal `~foo/.git`.
func (c Call) tildeOf(w Word) tildeKind {
	switch {
	case w.Flag != "", w.Verbatim, w.joined, w.Quote != Bare,
		!paths.TildeForm(w.Text, c.os == Windows):
		return tildeNone
	case strings.Contains(w.Text, `"`):
		return tildeUnknown
	case strings.ContainsAny(w.Text, " \t"):
		return tildeNone
	default:
		return tildeHome
	}
}

func (c Call) nativeWord(w Word, windowsTilde bool) shell.Word {
	switch {
	case w.Block:
		return shell.Word{}
	case w.Known == KnownHome || (w.Known == KnownUserProfile && c.os == Windows):
		v := c.home + w.Text
		if c.home == "" || (w.joined && strings.ContainsAny(v, " \t\"\r\n")) {
			return shell.Word{} // a joined value with blanks splits where the hook cannot see
		}
		return shell.Word{Text: w.Flag + v, Static: true}
	case w.Known != NotKnown, !w.Static:
		return shell.Word{}
	}
	switch c.tildeOf(w) {
	case tildeHome:
		if c.os == Windows && !windowsTilde {
			return shell.Word{}
		}
		return shell.Word{Text: w.Text, Static: true, Tilde: true}
	case tildeUnknown:
		return shell.Word{}
	case tildeNone:
	}
	return shell.Word{Text: w.Flag + w.Text, Static: true}
}

// Dialect is the language of a nested script.
type Dialect int

// Dialects.
const (
	PowerShell Dialect = iota
	Bash
)

// Script is a nested script with one static text.
type Script struct {
	Dialect Dialect
	Text    string
	// Cwd is the cwd of the call that runs it, or "" when unknown.
	Cwd string
	// Depth is the recursion depth the script runs at: pass it to shell.ParseDepth.
	Depth int
	// Err is set when a nested PowerShell script fails to tokenize; its calls are then dropped.
	Err error
}

// Result is what Parse returns.
type Result struct {
	Calls []Call
	// Nested lists every nested script in order, PowerShell ones (already parsed into Calls) and
	// Bash ones (left to the caller).
	Nested []Script
	// Err is set when the command fails to tokenize; Calls and Nested are then empty.
	Err error
}

// Parse recognizes cmd, run in cwd ("" when unknown) by a user whose home is home ("" when
// unknown), on host os.
func Parse(cmd, cwd, home string, os OS) Result {
	p := newParser(cmd, cwd, home, os, 0, knownSet{})
	if err := p.run(); err != nil {
		return Result{Err: err}
	}
	return p.res
}
