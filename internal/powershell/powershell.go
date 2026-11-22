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
// known variable is dynamic; an unquoted leading `~` is Tilde on non-Windows hosts and dynamic on
// Windows; a script block is dynamic.
func (c Call) ShellCall() shell.Call {
	out := shell.Call{Args: make([]shell.Word, len(c.Args)), Stdin: c.Stdin, Cwd: c.Cwd}
	for i, w := range c.Args {
		out.Args[i] = c.nativeWord(w)
	}
	return out
}

func (c Call) nativeWord(w Word) shell.Word {
	switch {
	case w.Block:
		return shell.Word{}
	case w.Known == KnownHome || (w.Known == KnownUserProfile && c.os == Windows):
		if c.home == "" {
			return shell.Word{}
		}
		return shell.Word{Text: w.Flag + c.home + w.Text, Static: true}
	case w.Known != NotKnown, !w.Static:
		return shell.Word{}
	}
	if w.Flag == "" && !w.Verbatim && w.Quote == Bare && strings.HasPrefix(w.Text, "~") {
		if c.os == Windows {
			return shell.Word{}
		}
		return shell.Word{Text: w.Text, Static: true, Tilde: true}
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
