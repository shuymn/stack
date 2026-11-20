// Package cli routes `stack` subcommands, parses their flags, prints usage, and maps errors to
// exit codes.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Exit codes shared by every command. Hook commands always exit exitOK.
const (
	exitOK      = 0
	exitRefused = 1
	exitUsage   = 2
)

var (
	// ErrRefused marks a refusal, finding or block (exit 1). Every other command error exits 2.
	ErrRefused = errors.New("refused")

	errNotImplemented = errors.New("not implemented yet")
	errArgs           = errors.New("wrong arguments")
)

// Env is everything a command may take from the process.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	Home    string
	Version string
}

// invocation is a parsed command line.
type invocation struct {
	args  []string
	flags *flag.FlagSet
}

type command struct {
	path    []string
	args    string // positional synopsis, one word per positional, e.g. "<pr> <head-sha>"
	flagDoc string // flag synopsis
	summary string
	hook    bool
	choices []string // allowed values of the first positional, when set
	define  func(fs *flag.FlagSet)
	run     func(ctx context.Context, env Env, inv invocation) error // nil: not implemented yet
}

func (c *command) name() string {
	return "stack " + strings.Join(c.path, " ")
}

func (c *command) synopsis() string {
	return strings.Join(
		slices.DeleteFunc([]string{c.name(), c.args, c.flagDoc}, func(s string) bool {
			return s == ""
		}),
		" ",
	)
}

var commands = []*command{
	{
		path:    []string{"kit", "open"},
		args:    "<slug>",
		summary: "record <slug> for this session and print its context",
		run:     runKitOpen,
	},
	{
		path:    []string{"kit", "close"},
		args:    "<slug>",
		summary: "remove <slug> from this session's kits",
		run:     runKitClose,
	},
	{
		path:    []string{"hook", "claude", "session-start"},
		summary: "SessionStart hook: export STACK_SESSION_ID; restore kits on compact",
		hook:    true,
	},
	{
		path:    []string{"hook", "claude", "pre-tool-use"},
		summary: "PreToolUse hook: deny the hard-boundary hazards",
		hook:    true,
	},
	{
		path: []string{"pr", "merge"},
		args: "<pr> <head-sha>",
		flagDoc: "[--merge|--squash|--rebase] [--delete-branch] [--subject S] [--body B] " +
			"[--allow-no-checks]",
		summary: "merge the checked head SHA after its checks pass",
		define: func(fs *flag.FlagSet) {
			fs.Bool("merge", false, "")
			fs.Bool("squash", false, "")
			fs.Bool("rebase", false, "")
			fs.Bool("delete-branch", false, "")
			fs.String("subject", "", "")
			fs.String("body", "", "")
			fs.Bool("allow-no-checks", false, "")
		},
	},
	{
		path:    []string{"check", "public"},
		args:    "<base>..<head>",
		flagDoc: "[--title-file F] [--body-file F] [--branch B]",
		summary: "scan a commit range and PR metadata for local paths and private terms",
		define: func(fs *flag.FlagSet) {
			fs.String("title-file", "", "")
			fs.String("body-file", "", "")
			fs.String("branch", "", "")
		},
		run: runCheckPublic,
	},
	{
		path:    []string{"budget", "status"},
		summary: "display kit sizes and attention item counts",
	},
	{
		path:    []string{"run"},
		args:    "<entry|outbound|cycle>",
		flagDoc: "[--dry-run] [--replay FILE] [--ext NAME]... [--range R] [--prompt-file F]",
		summary: "run the extensions of a stage",
		choices: []string{"entry", "outbound", "cycle"},
		define: func(fs *flag.FlagSet) {
			fs.Bool("dry-run", false, "")
			fs.String("replay", "", "")
			fs.Var(&stringList{}, "ext", "") // repeatable
			fs.String("range", "", "")
			fs.String("prompt-file", "", "")
		},
	},
}

// Run executes the command line args and returns the process exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		writeUsage(env.Stderr, nil)
		return exitUsage
	}
	switch args[0] {
	case "--version", "-v":
		_, _ = fmt.Fprintln(env.Stdout, env.Version)
		return exitOK
	case "help", "--help", "-h":
		writeUsage(env.Stdout, nil)
		return exitOK
	}

	cmd, rest, prefix := route(args)
	if cmd == nil {
		if prefix == nil {
			_, _ = fmt.Fprintf(env.Stderr, "stack: unknown command %q\n", args[0])
		} else {
			_, _ = fmt.Fprintf(env.Stderr, "stack %s: missing or unknown subcommand\n",
				strings.Join(prefix, " "))
		}
		writeUsage(env.Stderr, prefix)
		return exitUsage
	}
	if cmd.hook {
		return runHook(ctx, cmd, rest, env)
	}

	inv, err := parse(cmd, rest)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprintf(env.Stdout, "usage: %s\n", cmd.synopsis())
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %v\nusage: %s\n", cmd.name(), err, cmd.synopsis())
		return exitUsage
	}
	if cmd.run == nil {
		err = errNotImplemented
	} else {
		err = cmd.run(ctx, env, inv)
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %v\n", cmd.name(), err)
		return exitCode(err)
	}
	return exitOK
}

// route finds the command named by the leading words of args. When none matches, prefix is the
// longest group of commands that the leading words name, or nil when even the first word is unknown.
func route(args []string) (*command, []string, []string) {
	longest := 0
	for _, c := range commands {
		n := len(c.path)
		if len(args) >= n && slices.Equal(args[:n], c.path) {
			return c, args[n:], nil
		}
		for i := longest + 1; i < n && i <= len(args) && slices.Equal(args[:i], c.path[:i]); i++ {
			longest = i
		}
	}
	if longest == 0 {
		return nil, nil, nil
	}
	return nil, nil, args[:longest]
}

func parse(cmd *command, args []string) (invocation, error) {
	fs := flag.NewFlagSet(cmd.name(), flag.ContinueOnError)
	if cmd.define != nil {
		cmd.define(fs)
	}
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return invocation{}, err
	}
	if nargs := len(strings.Fields(cmd.args)); len(positional) != nargs {
		return invocation{}, fmt.Errorf("%w: want %d positional, got %d", errArgs, nargs,
			len(positional))
	}
	if cmd.choices != nil && !slices.Contains(cmd.choices, positional[0]) {
		return invocation{}, fmt.Errorf("%w: %q is not one of %s", errArgs, positional[0],
			strings.Join(cmd.choices, ", "))
	}
	return invocation{args: positional, flags: fs}, nil
}

// runHook never fails a session: it prints nothing on stdout and exits 0 whatever happens. Errors
// reach stderr only with STACK_DEBUG=1.
func runHook(ctx context.Context, cmd *command, args []string, env Env) int {
	debug := env.Getenv != nil && env.Getenv("STACK_DEBUG") == "1"
	defer func() {
		if r := recover(); r != nil && debug {
			_, _ = fmt.Fprintf(env.Stderr, "%s: panic: %v\n", cmd.name(), r)
		}
	}() // a recovered panic returns the zero value, exitOK
	err := errNotImplemented
	if cmd.run != nil {
		err = cmd.run(ctx, env, invocation{args: args})
	}
	if err != nil && debug {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %v\n", cmd.name(), err)
	}
	return exitOK
}

// exitCode maps a command error: refusals exit 1, every other error (usage, input validation,
// config, environment, not implemented) exits 2.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, ErrRefused):
		return exitRefused
	default:
		return exitUsage
	}
}

// writeUsage prints the commands under prefix, or all of them when prefix is nil.
func writeUsage(w io.Writer, prefix []string) {
	_, _ = fmt.Fprintln(w, "usage: stack <command> [arguments]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "commands:")
	for _, c := range commands {
		if len(c.path) >= len(prefix) && slices.Equal(c.path[:len(prefix)], prefix) {
			_, _ = fmt.Fprintf(w, "  %s\n      %s\n", c.synopsis(), c.summary)
		}
	}
	if prefix == nil {
		_, _ = fmt.Fprintln(w, "  stack --version\n      print the version")
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "exit codes: 0 ok; 1 refused, finding or block; "+
			"2 usage, input, config or environment error. Hooks always exit 0.")
	}
}
