// Package guard is the stack-guard hook (docs/design/core-stages.md §4.5): it decides a Claude Code
// PreToolUse payload by the secrets, gh-publish, rmroot and pencil rules of
// docs/design/phase2-core.md §4.3 and allows everything else. Rules run in that order; the first
// deny wins. Settings come from ~/.config/stack/safety.toml (§4.6).
//
// The rules are a copy of the core hook's (internal/guard), which keeps them until the core removal
// (§7 PR 10); the drift test runs every golden through both binaries.
package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/shuymn/stack/extensions/internal/safety"
	"github.com/shuymn/stack/internal/ghargs"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/shell"
)

// Rule names, as they appear in deny reasons.
const (
	RuleSecrets   = "secrets"
	RuleGhPublish = "gh-publish"
	RuleRmRoot    = "rmroot"
	RulePencil    = "pencil"
)

const (
	exitOK    = 0
	exitUsage = 2
	usage     = "usage: stack-guard hook claude pre-tool-use\n"
)

// Env is what the hook reads from its process. Home is the user's home directory, or "" when
// unknown (then safety.toml is not read and `~` entries and operands are not resolved).
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Home           string
}

// Main runs `stack-guard hook claude pre-tool-use`: it prints a deny for the guarded hazards and
// nothing otherwise, and fails open (hookio.Run). Any other argv, including none, prints usage and
// exits 2, so a guard listed in a stage by mistake shows up as a warn.
func Main(args []string, env Env) int {
	if !slices.Equal(args, []string{"hook", "claude", "pre-tool-use"}) {
		_, _ = io.WriteString(env.Stderr, usage)
		return exitUsage
	}
	getenv := env.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	hookio.Run(env.Stdin, env.Stdout, env.Stderr, hookio.Debug(getenv),
		func(in hookio.Input) ([]byte, error) {
			d, ok := Decide(in, Options{Safety: loadSafety(env.Home), Home: env.Home})
			if !ok {
				return nil, nil
			}
			return hookio.Deny(d.Reason)
		})
	return exitOK
}

// loadSafety reads safety.toml under home. A missing or broken file yields the defaults (a broken
// file's custom entries are not kept), so a typo cannot turn a rule off.
func loadSafety(home string) safety.Config {
	if home == "" {
		return safety.Defaults()
	}
	cfg, _, err := safety.Load(safety.File(home))
	if err != nil {
		return safety.Defaults()
	}
	return cfg
}

// Options carries what a decision needs besides the payload.
type Options struct {
	Safety safety.Config
	// Home is the user's home directory, or "" when unknown.
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
		Reason: fmt.Sprintf("stack-guard (%s): ", rule) + fmt.Sprintf(format, args...),
	}, true
}

// Decide returns the deny for the payload, or false to allow it.
func Decide(in hookio.Input, opts Options) (Decision, bool) {
	switch in.ToolName {
	case "Read", "NotebookRead", "Grep", "Glob":
		return secretsFileTool(in, opts)
	case "Bash":
		var tin struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(in.ToolInput, &tin) != nil || tin.Command == "" {
			return Decision{}, false
		}
		return decideBash(tin.Command, in.CWD, opts)
	case "mcp__pencil__execute":
		return pencil(in.ToolInput, opts.Safety)
	default:
		return Decision{}, false
	}
}

func decideBash(command, cwd string, opts Options) (Decision, bool) {
	parsed := shell.Parse(command, cwd, opts.Home)
	if d, ok := secretsBash(command, parsed.Handoffs, opts); ok {
		return d, true
	}
	if d, ok := ghPublish(ghCalls(parsed.Calls), opts); ok {
		return d, true
	}
	return rmRoot(parsed.Calls, opts)
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
