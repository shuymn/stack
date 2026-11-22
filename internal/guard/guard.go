// Package guard decides a Claude Code PreToolUse payload: it denies a raw merge, which would skip
// `stack pr merge` and its merged stage, and allows everything else (docs/design/core-stages.md
// §4.5). The other hard-boundary rules live in the stack-guard hook (extensions/guard).
package guard

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/shuymn/stack/internal/ghargs"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/powershell"
	"github.com/shuymn/stack/internal/shell"
)

// RuleRawMerge is the rule name, as it appears in deny reasons.
const RuleRawMerge = "raw-merge"

// Options carries what a decision needs besides the payload.
type Options struct {
	// Home is the user's home directory, or "" when unknown (then `~` in a file a gh call reads is
	// not resolved).
	Home string
	// OS is the host a PowerShell command runs on (powershell.HostOS() in the hook): it decides the
	// command's aliases, case and path rules.
	OS powershell.OS
}

// Decision is a deny.
type Decision struct {
	Rule   string
	Reason string
}

func deny(format string, args ...any) (Decision, bool) {
	return Decision{
		Rule:   RuleRawMerge,
		Reason: "stack guard (" + RuleRawMerge + "): " + fmt.Sprintf(format, args...),
	}, true
}

// Decide returns the deny for the payload, or false to allow it. Only a Bash or PowerShell command
// can merge.
func Decide(in hookio.Input, opts Options) (Decision, bool) {
	if in.ToolName != "Bash" && in.ToolName != "PowerShell" {
		return Decision{}, false
	}
	var tin struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &tin) != nil || tin.Command == "" {
		return Decision{}, false
	}
	if in.ToolName == "PowerShell" {
		return decidePowerShell(tin.Command, in.CWD, opts)
	}
	return decideBash(tin.Command, in.CWD, 0, opts)
}

// decideBash judges a Bash command nested depth levels deep (0 for the Bash tool's own command).
func decideBash(command, cwd string, depth int, opts Options) (Decision, bool) {
	parsed := shell.ParseDepth(command, cwd, opts.Home, depth)
	var calls []ghCall
	for _, c := range parsed.Calls {
		if c.Name() == "gh" {
			calls = appendGh(calls, c, paths.Resolve)
		}
	}
	return rawMerge(calls, opts)
}

// decidePowerShell judges the gh calls the recognizer finds (docs/design/powershell-guard.md §4.1),
// then each nested Bash script through the Bash path at the depth it runs at. A command that fails
// to lex yields no calls.
func decidePowerShell(command, cwd string, opts Options) (Decision, bool) {
	parsed := powershell.Parse(command, cwd, opts.Home, opts.OS)
	resolve := powerShellResolver(opts.OS)
	var calls []ghCall
	for _, c := range parsed.Calls {
		if c.Is("gh") {
			calls = appendGh(calls, c.ShellCall(), resolve)
		}
	}
	if d, ok := rawMerge(calls, opts); ok {
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

// resolver turns a path a command opens into an absolute path; expand reports an unquoted `~`.
type resolver func(home, cwd, p string, expand bool) (string, bool)

// powerShellResolver resolves with PowerShell's path rules on host os: no Git Bash forms, and a `~`
// the adapter did not mark as home is a literal name.
func powerShellResolver(os powershell.OS) resolver {
	return func(home, cwd, p string, expand bool) (string, bool) {
		if !expand {
			home = ""
		}
		return paths.ResolvePowerShell(home, cwd, p, os == powershell.Windows)
	}
}

// ghCall is a gh call with its parsed argv and the resolver of its dialect.
type ghCall struct {
	call    shell.Call
	gh      ghargs.Call
	resolve resolver
}

// appendGh appends c, a gh call, when its argv parses.
func appendGh(out []ghCall, c shell.Call, resolve resolver) []ghCall {
	args := make([]ghargs.Arg, len(c.Args)-1)
	for i, a := range c.Args[1:] {
		args[i] = ghargs.Arg{Text: a.Text, Static: a.Static, Tilde: a.Tilde}
	}
	if g, ok := ghargs.Parse(args); ok {
		out = append(out, ghCall{call: c, gh: g, resolve: resolve})
	}
	return out
}

// readInput returns the content of a file or stdin input of c, or false when it is not statically
// known (unknown cwd, a pipe, an unreadable file).
func readInput(in ghargs.Input, c ghCall, home string) (string, bool) {
	switch in.Kind {
	case ghargs.Text:
		return in.Text, true
	case ghargs.File:
		return readFile(c.resolve, home, c.call.Cwd, in.Path, in.Expand)
	case ghargs.Stdin:
		switch c.call.Stdin.Kind {
		case shell.StdinText:
			return c.call.Stdin.Text, true
		case shell.StdinFile:
			return readFile(c.resolve, home, c.call.Stdin.Dir, c.call.Stdin.File.Text,
				c.call.Stdin.File.Tilde)
		case shell.StdinNone, shell.StdinDynamic:
		}
	}
	return "", false
}

// readFile reads a regular file named in a command, resolved against the call's cwd, whole: there is
// no size cap, since a hit past any cap would otherwise pass. Anything else (a FIFO, a device, a
// directory) is skipped before opening, because opening a FIFO blocks until a writer appears.
func readFile(resolve resolver, home, cwd, p string, expand bool) (string, bool) {
	full, ok := resolve(home, cwd, p, expand)
	if !ok || full == paths.ShellRoot {
		return "", false
	}
	if fi, err := os.Stat(full); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	return string(data), true
}
