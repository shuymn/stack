package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/ghargs"
)

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	return runIn(t, t.TempDir(), env, args...)
}

// runIn is run with a given home.
func runIn(t *testing.T, home string, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Getenv:  func(k string) string { return env[k] },
		Home:    home,
		Version: "v1.2.3",
	})
	return result{code, stdout.String(), stderr.String()}
}

func TestVersion(t *testing.T) {
	for _, arg := range []string{"--version", "-v"} {
		r := run(t, nil, arg)
		if r.code != exitOK || r.stdout != "v1.2.3\n" || r.stderr != "" {
			t.Errorf("%s = %+v", arg, r)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	for _, arg := range []string{"--version", "-v"} {
		r := run(t, nil, arg, "--json")
		if r.code != exitOK || r.stderr != "" {
			t.Fatalf("%s --json = %+v", arg, r)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
			t.Fatalf("%s --json stdout %q: %v", arg, r.stdout, err)
		}
		want := map[string]string{"version": "v1.2.3", "gh_model": ghargs.GhVersion}
		if !maps.Equal(got, want) {
			t.Errorf("%s --json = %v, want %v", arg, got, want)
		}
	}
	// Any other trailing argument keeps the plain output, as before --json existed.
	for _, args := range [][]string{{"--version", "--yaml"}, {"--version", "x", "--json"}} {
		if r := run(t, nil, args...); r.code != exitOK || r.stdout != "v1.2.3\n" || r.stderr != "" {
			t.Errorf("%q = %+v", args, r)
		}
	}
}

func TestUsage(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		r := run(t, nil, arg)
		if r.code != exitOK || !strings.Contains(r.stdout, "stack pr merge <pr> <head-sha>") {
			t.Errorf("%s = %+v", arg, r)
		}
	}
	r := run(t, nil)
	if r.code != exitUsage || r.stdout != "" || !strings.Contains(r.stderr, "usage: stack") {
		t.Errorf("no args = %+v", r)
	}
}

func TestUnknownCommand(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"nope"}, `unknown command "nope"`},
		{[]string{"kit"}, "stack kit: missing or unknown subcommand"},
		{[]string{"kit", "shut", "x"}, "stack kit: missing or unknown subcommand"},
		{[]string{"hook", "claude"}, "stack hook claude: missing or unknown subcommand"},
		{[]string{"hook", "codex", "x"}, "stack hook: missing or unknown subcommand"},
		{[]string{"budget"}, "stack budget: missing or unknown subcommand"},
	}
	for _, tt := range tests {
		r := run(t, nil, tt.args...)
		if r.code != exitUsage || r.stdout != "" || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("%q = %+v, want exit 2 and %q", tt.args, r, tt.want)
		}
	}
	if r := run(t, nil, "kit"); !strings.Contains(r.stderr, "stack kit open <slug>") ||
		strings.Contains(r.stderr, "stack budget status") {
		t.Errorf("group usage not limited to the group: %q", r.stderr)
	}
}

// TestRouting checks that `run` reaches its command with interspersed flags. Not parallel: it
// changes the working directory, so the dry run's git reads see no repository.
func TestRouting(t *testing.T) {
	t.Chdir(t.TempDir())
	r := run(t, nil, "run", "--dry-run", "outbound", "--ext", "demo", "--ext", "other")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "stage: outbound\nplan:\n  demo -> ") ||
		!strings.Contains(r.stdout, "\n  other -> ") {
		t.Errorf("run --dry-run = %+v", r)
	}
}

func TestArgumentErrors(t *testing.T) {
	tests := [][]string{
		{"kit", "open"},
		{"kit", "open", "a", "b"},
		{"pr", "merge", "12"},
		{"pr", "merge", "12", "abc", "--admin"},
		{"pr", "merge", "12", "abc", "--subject"},
		{"pr", "merge", "12", "abc", "--squash=maybe"},
		{"check", "public"},
		{"budget", "status", "extra"},
		{"run", "nope"},
		{"run"},
	}
	for _, args := range tests {
		r := run(t, nil, args...)
		if r.code != exitUsage || r.stdout != "" || !strings.Contains(r.stderr, "usage: stack") {
			t.Errorf("%q = %+v, want a usage error (exit 2)", args, r)
		}
	}
}

func TestRunRepeatedExtReachesDispatch(t *testing.T) {
	cmd, _, _ := route([]string{"run"})
	orig := cmd.run
	t.Cleanup(func() { cmd.run = orig })
	var got invocation
	cmd.run = func(_ context.Context, _ Env, inv invocation) error {
		got = inv
		return nil
	}

	r := run(t, nil, "run", "--ext", "first", "entry", "--ext=second")
	if r.code != exitOK {
		t.Fatalf("run = %+v", r)
	}
	ext, ok := got.flags.Lookup("ext").Value.(*stringList)
	if !ok || !slices.Equal(*ext, []string{"first", "second"}) ||
		!slices.Equal(got.args, []string{"entry"}) {
		t.Fatalf(
			"dispatched ext = %v, args = %q; want [first second], [entry]",
			got.flags.Lookup("ext").Value,
			got.args,
		)
	}
}

func TestCommandHelp(t *testing.T) {
	r := run(t, nil, "pr", "merge", "--help")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "usage: stack pr merge <pr> <head-sha>") {
		t.Fatalf("pr merge --help = %+v", r)
	}
}

func TestHooksExitZeroSilently(t *testing.T) {
	for _, sub := range []string{"session-start", "pre-tool-use"} {
		r := run(t, nil, "hook", "claude", sub, "unexpected")
		if r.code != exitOK || r.stdout != "" || r.stderr != "" {
			t.Errorf("hook %s = %+v, want exit 0 and no output", sub, r)
		}
		// Empty stdin is not a payload: the error reaches stderr only with STACK_DEBUG=1.
		r = run(t, map[string]string{"STACK_DEBUG": "1"}, "hook", "claude", sub)
		if r.code != exitOK || r.stdout != "" || r.stderr == "" {
			t.Errorf("hook %s with STACK_DEBUG=1 = %+v", sub, r)
		}
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{fmt.Errorf("check failed: %w", ErrRefused), exitRefused},
		{errors.New("config broken"), exitUsage},
	}
	for _, tt := range tests {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestParseInterspersed(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.Bool("squash", false, "")
		fs.String("body", "", "")
		return fs
	}
	tests := []struct {
		args       []string
		positional []string
		body       string
		squash     string
		err        error
	}{
		{[]string{"12", "abc", "--squash"}, []string{"12", "abc"}, "", "true", nil},
		{[]string{"--body", "--x", "12"}, []string{"12"}, "--x", "false", nil},
		{[]string{"-body", "-", "12"}, []string{"12"}, "-", "false", nil},
		{[]string{"--body=a=b", "--squash=false"}, nil, "a=b", "false", nil},
		{
			[]string{"12", "--", "--squash", "-x"},
			[]string{"12", "--squash", "-x"},
			"",
			"false",
			nil,
		},
		{[]string{"-", "12"}, []string{"-", "12"}, "", "false", nil},
		{[]string{"--nope"}, nil, "", "false", errUnknownFlag},
		{[]string{"12", "--body"}, nil, "", "false", errMissingValue},
		{[]string{"-h"}, nil, "", "false", flag.ErrHelp},
		{[]string{"x", "--help"}, nil, "", "false", flag.ErrHelp},
	}
	for _, tt := range tests {
		fs := newFS()
		got, err := parseInterspersed(fs, tt.args)
		if tt.err != nil {
			if !errors.Is(err, tt.err) {
				t.Errorf("%q: err = %v, want %v", tt.args, err, tt.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tt.positional) {
			t.Errorf("%q = %q, %v; want %q", tt.args, got, err, tt.positional)
			continue
		}
		if b := fs.Lookup("body").Value.String(); b != tt.body {
			t.Errorf("%q: body = %q, want %q", tt.args, b, tt.body)
		}
		if s := fs.Lookup("squash").Value.String(); s != tt.squash {
			t.Errorf("%q: squash = %q, want %q", tt.args, s, tt.squash)
		}
	}
}
