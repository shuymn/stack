package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/powershell"
)

func (f *fixture) ps(cmd string, os powershell.OS) (Decision, bool) {
	f.t.Helper()
	raw, err := json.Marshal(map[string]any{"command": cmd})
	if err != nil {
		f.t.Fatal(err)
	}
	opts := f.opts
	opts.OS = os
	return Decide(hookio.Input{
		CWD:           f.cwd,
		HookEventName: "PreToolUse",
		ToolName:      "PowerShell",
		ToolInput:     raw,
	}, opts)
}

var bothOS = []powershell.OS{powershell.Unix, powershell.Windows}

func osName(os powershell.OS) string {
	if os == powershell.Windows {
		return "windows"
	}
	return "unix"
}

// TestRawMergePowerShell is C2 of docs/design/powershell-guard.md §7.2.
func TestRawMergePowerShell(t *testing.T) {
	f := newFixture(t)
	mutation := `mutation { mergePullRequest(input: {pullRequestId: "x"}) { clientMutationId } }`
	f.write("sub/q.graphql", mutation)

	deny := []string{
		"gh pr merge 1",
		"gh.exe pr merge 1",
		"& gh pr merge 1",
		"& 'gh' pr merge 1",
		"gh -R o/r pr merge 1",
		"gh pr merge 1 --help=false",
		"gh pr merge 1 --disable-auto=false",
		"gh api -X PUT repos/o/r/pulls/1/merge",
		"gh api graphql -f query='" + mutation + "'",
		"cd sub && gh api graphql -F query=@q.graphql",
		`Write-Output "$(gh pr merge 1)"`,
		"bash -c 'gh pr merge 1'",
		"bash -c 'gh pr merge 7\nif ('",
		"git --% status && gh pr merge 7",
		"gh pr merge 7\n@'\ntext\n'@ | Write-Output",
		`iex "bash -c 'gh pr merge 7'"`,
		`gh pr merge 7; iex 'Write-Output "'`,
		// A quoted `--%` is dropped from argv and the later arguments are joined as written.
		"gh '--%' pr merge 7",
		`& gh.exe "--%" pr merge 7`,
		`gh pr '--%' "merge 7"`,
		`& gh.exe '--%' pr "merge 7 --squash"`,
		"git `--% x && gh pr merge 7",
	}
	allow := []string{
		"gh pr merge 7 2>&1 --help",
		"gh pr view 1",
		"gh pr merge 1 --help",
		"gh pr merge 1 --disable-auto",
		`echo "gh pr merge"`,
		"# gh pr merge 1",
		"cd sub; gh api graphql -F query=@q.graphql",
		`iex 'bash -c ''bash -c "gh pr merge 7"'''`,
		"gh '--%' pr view 7",
		"gh pr merge 7 '--%' --help",
		// The backtick-escaped `--%` is the stop-parsing token: `;` stays in its tail.
		"Write-Output `--% ; gh pr merge 7",
	}
	for _, os := range bothOS {
		for _, cmd := range deny {
			d, ok := f.ps(cmd, os)
			wantDeny(t, osName(os)+": "+cmd, d, ok)
		}
		for _, cmd := range allow {
			d, ok := f.ps(cmd, os)
			wantAllow(t, osName(os)+": "+cmd, d, ok)
		}
	}
	d, ok := f.ps("GH.EXE pr merge 1", powershell.Windows)
	wantDeny(t, "windows GH.EXE", d, ok)
	if ok && !strings.Contains(d.Reason, "stack pr merge <pr> <head-sha>") {
		t.Errorf("raw merge reason does not name stack pr merge: %q", d.Reason)
	}

	// The depth allowance is shared across the dialect boundary: the inner Bash text reaches the
	// Bash path at depth 2, where its own nested script is beyond the limit, while the same text
	// at depth 0 denies.
	const beyond = `iex 'bash -c ''bash -c "gh pr merge 7"'''`
	r := powershell.Parse(beyond, f.cwd, f.opts.Home, powershell.Unix)
	if len(r.Nested) != 2 || r.Nested[1].Dialect != powershell.Bash ||
		r.Nested[1].Text != `bash -c "gh pr merge 7"` || r.Nested[1].Depth != 2 {
		t.Fatalf("nested = %+v, want the inner Bash script at depth 2", r.Nested)
	}
	if _, ok := decideBash(r.Nested[1].Text, f.cwd, 0, f.opts); !ok {
		t.Errorf("the inner Bash text at depth 0 does not deny")
	}
}

// TestRawMergePowerShellTilde checks a native `~` `--input` body: home on non-Windows hosts, and on
// Windows opaque, since the file gh opens depends on the engine (7.6+ expands `~`, 5.1 and 7.0–7.5
// pass it as written). The merge mutation is only in the home copy; the cwd's literal `~` copy is a
// query.
func TestRawMergePowerShellTilde(t *testing.T) {
	f := newFixture(t)
	body := func(q string) string {
		raw, err := json.Marshal(map[string]string{"query": q})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	merge := body(`mutation { mergePullRequest(input: {pullRequestId: "x"}) { clientMutationId } }`)
	if err := os.WriteFile(filepath.Join(f.opts.Home, "q.json"), []byte(merge), 0o600); err != nil {
		t.Fatal(err)
	}
	f.write("~/q.json", body(`query { viewer { login } }`))

	d, ok := f.ps("gh api graphql --input ~/q.json", powershell.Unix)
	wantDeny(t, "unix: --input ~/q.json", d, ok)
	d, ok = f.ps("gh api graphql --input '~/q.json'", powershell.Unix)
	wantAllow(t, "unix: --input '~/q.json'", d, ok)
	for _, cmd := range []string{`gh api graphql --input ~\q.json`, "gh api graphql --input ~/q.json"} {
		d, ok = f.ps(cmd, powershell.Windows)
		wantAllow(t, "windows: "+cmd, d, ok)
	}
}

// TestRawMergePowerShellCurrentDrive checks that a gh call's file is read through the PowerShell
// resolver: a current-drive-rooted `\…` path that the Git Bash resolver rejects.
func TestRawMergePowerShellCurrentDrive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a current-drive-rooted path opens only on Windows")
	}
	f := newFixture(t)
	f.write("q.graphql", `mutation { mergePullRequest(input: {}) { clientMutationId } }`)
	rooted := strings.TrimPrefix(filepath.Join(f.cwd, "q.graphql"), filepath.VolumeName(f.cwd))
	d, ok := f.ps("gh api graphql -F query=@"+rooted, powershell.Windows)
	wantDeny(t, rooted, d, ok)
}

// TestPowerShellFailOpen is C6 for the core hook, through hook stdout.
func TestPowerShellFailOpen(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	run := func(input string) string {
		t.Helper()
		stdin := `{"hook_event_name":"PreToolUse","tool_name":"PowerShell","cwd":` +
			jsonString(t, cwd) + `,"tool_input":` + input + `}`
		var stdout, stderr bytes.Buffer
		hookio.Run(strings.NewReader(stdin), &stdout, &stderr, false,
			func(in hookio.Input) ([]byte, error) {
				d, ok := Decide(in, Options{Home: home, OS: powershell.HostOS()})
				if !ok {
					return nil, nil
				}
				return hookio.Deny(d.Reason)
			})
		return stdout.String()
	}
	for _, input := range []string{
		`"x"`,
		`{"command":7}`,
		`{}`,
		`{"command":""}`,
		`{"command":` + jsonString(t, `gh pr merge 7; Write-Output "`) + `}`,
		// Past the syntax recursion cap the script yields no calls.
		`{"command":` + jsonString(t, nested(201, "gh pr merge 7")) + `}`,
	} {
		if out := run(input); out != "" {
			t.Errorf("tool_input %.60s: stdout %q, want none", input, out)
		}
	}
	for _, cmd := range []string{
		`gh pr merge 7; iex 'Write-Output "'`,
		nested(199, "gh pr merge 7"),
	} {
		input := `{"command":` + jsonString(t, cmd) + `}`
		if out := run(input); !strings.Contains(out, "stack guard (raw-merge): ") {
			t.Errorf("%.60s: stdout %q, want the raw-merge deny", cmd, out)
		}
	}
}

// nested wraps cmd in n levels of parentheses.
func nested(n int, cmd string) string {
	return strings.Repeat("(", n) + cmd + strings.Repeat(")", n)
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
