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
	"github.com/shuymn/stack/internal/shell"
)

// RuleRawMerge is the rule name, as it appears in deny reasons.
const RuleRawMerge = "raw-merge"

// Options carries what a decision needs besides the payload.
type Options struct {
	// Home is the user's home directory, or "" when unknown (then `~` entries and operands are not
	// resolved).
	Home string
}

// Decision is a deny.
type Decision struct {
	Rule   string
	Reason string
}

func deny(rule, format string, args ...any) (Decision, bool) {
	return Decision{
		Rule:   rule,
		Reason: fmt.Sprintf("stack guard (%s): ", rule) + fmt.Sprintf(format, args...),
	}, true
}

// Decide returns the deny for the payload, or false to allow it. Only a Bash command can merge.
func Decide(in hookio.Input, opts Options) (Decision, bool) {
	if in.ToolName != "Bash" {
		return Decision{}, false
	}
	var tin struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &tin) != nil || tin.Command == "" {
		return Decision{}, false
	}
	parsed := shell.Parse(tin.Command, in.CWD, opts.Home)
	return rawMerge(ghCalls(parsed.Calls), opts)
}

// ghCall is a gh call with its parsed argv.
type ghCall struct {
	call shell.Call
	gh   ghargs.Call
}

func ghCalls(calls []shell.Call) []ghCall {
	var out []ghCall
	for _, c := range calls {
		if c.Name() != "gh" {
			continue
		}
		args := make([]ghargs.Arg, len(c.Args)-1)
		for i, a := range c.Args[1:] {
			args[i] = ghargs.Arg{Text: a.Text, Static: a.Static, Tilde: a.Tilde}
		}
		if g, ok := ghargs.Parse(args); ok {
			out = append(out, ghCall{call: c, gh: g})
		}
	}
	return out
}

// readInput returns the content of a file or stdin input of call, or false when it is not statically
// known (unknown cwd, a pipe, an unreadable file).
func readInput(in ghargs.Input, call shell.Call, home string) (string, bool) {
	switch in.Kind {
	case ghargs.Text:
		return in.Text, true
	case ghargs.File:
		return readFile(home, call.Cwd, in.Path, in.Expand)
	case ghargs.Stdin:
		switch call.Stdin.Kind {
		case shell.StdinText:
			return call.Stdin.Text, true
		case shell.StdinFile:
			return readFile(home, call.Stdin.Dir, call.Stdin.File.Text, call.Stdin.File.Tilde)
		case shell.StdinNone, shell.StdinDynamic:
		}
	}
	return "", false
}

// readFile reads a regular file named in a command, resolved against the call's cwd, whole: there is
// no size cap, since a hit past any cap would otherwise pass. Anything else (a FIFO, a device, a
// directory) is skipped before opening, because opening a FIFO blocks until a writer appears.
func readFile(home, cwd, p string, expand bool) (string, bool) {
	full, ok := paths.Resolve(home, cwd, p, expand)
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
