package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/safety"
)

// bothOS lists the hosts each case runs on: Unix (false) and Windows (true).
var bothOS = []bool{false, true}

func osName(windows bool) string {
	if windows {
		return "windows"
	}
	return "unix"
}

// psDecide decides a PowerShell command with the rules of a Windows or a Unix host and the default
// safety settings, or with secrets when it is not nil.
func (f *fixture) psDecide(cmd string, windows bool, secrets ...string) (Decision, bool) {
	f.t.Helper()
	opts := Options{Safety: safety.Defaults(), Home: f.home}
	if secrets != nil {
		opts.Safety.Secrets = secrets
	}
	if windows {
		opts.OS = windowsHost
	}
	return decidePowerShell(cmd, f.cwd, opts)
}

func wantDeny(t *testing.T, name string, d Decision, ok bool, rule string) {
	t.Helper()
	if !ok || d.Rule != rule || !strings.HasPrefix(d.Reason, "stack-guard ("+rule+"): ") {
		t.Errorf("%s: got deny=%v %+v, want a %s deny", name, ok, d, rule)
	}
}

func wantAllow(t *testing.T, name string, d Decision, ok bool) {
	t.Helper()
	if ok {
		t.Errorf("%s: got %+v, want allow", name, d)
	}
}

// psCases are deny and allow commands for one rule on one host.
type psCases struct {
	windows      bool
	deny, allow  []string
	rule         string
	secretsEntry []string
}

func (f *fixture) check(c psCases) {
	f.t.Helper()
	for _, cmd := range c.deny {
		d, ok := f.psDecide(cmd, c.windows, c.secretsEntry...)
		wantDeny(f.t, osName(c.windows)+": "+cmd, d, ok, c.rule)
	}
	for _, cmd := range c.allow {
		d, ok := f.psDecide(cmd, c.windows, c.secretsEntry...)
		wantAllow(f.t, osName(c.windows)+": "+cmd, d, ok)
	}
}

// TestSecretsPowerShell is C3 of docs/design/powershell-guard.md §7.2.
func TestSecretsPowerShell(t *testing.T) {
	f := newFixture(t)
	entries := []string{"~/.config/app/token", ".dev.vars", "/srv/keys/prod"}
	users := strings.Join([]string{"C:", "Users", "dev"}, "/")
	both := []string{
		"Get-Content .dev.vars",
		"type .dev.vars",
		`Get-Content ~\.config\app\token`,
		`Get-Content "$env:USERPROFILE\.config\app\token"`,
		`Get-Content /srv/keys/prod`,
		`Get-Content \srv\keys\prod`,
		"Get-Date # .dev.vars",
		"Write-Output 'Copy .dev.vars.example to .dev.vars'",
		"Get-Content .dev.vars; Write-Output \"",
		// No handoff form passes (option A).
		"$env:K = Get-Content -Raw -LiteralPath .dev.vars; wrangler deploy",
		"wrangler deploy --token (Get-Content -Raw -LiteralPath .dev.vars)",
		`wrangler deploy --token "$(Get-Content .dev.vars)"`,
	}
	allow := []string{"Get-Content .dev.vars.example", "Get-Content README.md"}
	// Windows folds case and counts a run of separators as one.
	windowsOnly := []string{
		`Get-Content .\.DEV.VARS`,
		`Get-Content .config\\app//token`,
		`Get-Content "$env:USERPROFILE\.config\\app\token"`,
		"Get-Content " + users + "/.CONFIG/APP/token",
		`Get-Content \SRV\keys\\prod`,
	}
	f.check(psCases{deny: both, allow: allow, rule: RuleSecrets, secretsEntry: entries})
	f.check(psCases{
		windows:      true,
		deny:         slices.Concat(both, windowsOnly),
		allow:        allow,
		rule:         RuleSecrets,
		secretsEntry: entries,
	})
	// On a host that does not fold case, `.DEV.VARS` is another file.
	d, ok := f.psDecide(`Get-Content .DEV.VARS`, false, entries...)
	if foldCase {
		wantDeny(t, "unix on a folding host: .DEV.VARS", d, ok, RuleSecrets)
	} else {
		wantAllow(t, "unix: .DEV.VARS", d, ok)
	}

	d, ok = f.psDecide("Get-Content .dev.vars", isWindows)
	if !ok || !strings.Contains(d.Reason, "No PowerShell form passes a secret file") ||
		!strings.Contains(d.Reason, `"$(cat <file>)" cmd`) {
		t.Errorf("secrets reason lacks the PowerShell advice: %+v", d)
	}
}

// TestSecretsPowerShellSeparatorRuns checks that a run of separators is one separator on Windows
// everywhere in the text, so a match cannot start inside a run, and that the scan stays linear.
func TestSecretsPowerShellSeparatorRuns(t *testing.T) {
	f := newFixture(t)
	entry := []string{"/srv/keys/prod"}
	for _, sep := range []string{`\`, `\\`, `/\/`} {
		cmd := "Get-Content 'C:" + `\tmp` + sep + `srv\keys\prod'`
		d, ok := f.psDecide(cmd, true, entry...)
		wantAllow(t, cmd, d, ok)
	}
	d, ok := f.psDecide(`Get-Content 'C:\tmp \\srv\\keys\prod'`, true, entry...)
	wantDeny(t, "a run after a space", d, ok, RuleSecrets)

	long := "Write-Output '" + strings.Repeat(`\`, 200_000) + "x'"
	start := time.Now()
	d, ok = f.psDecide(long, true, entry...)
	wantAllow(t, "a long separator run", d, ok)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a %d-byte separator run took %v", len(long), took)
	}
}

// TestGhPublishPowerShell is C4. Every hit is a path publictext recognizes, built at run time.
func TestGhPublishPowerShell(t *testing.T) {
	f := newFixture(t)
	lp := localPath()
	f.write("sub/b.md", "see "+lp+"\n")
	f.write("clean.md", "clean\n")
	f.write("b.md", "see "+lp+"\n") // the cwd copy, read only under a stale cwd
	f.write("~/b.md", "clean\n")    // what a literal `~/b.md` names under cwd
	homeFile := filepath.Join(f.home, "b.md")
	if err := os.WriteFile(homeFile, []byte("see "+lp+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSuffix(lp, "/notes.md")

	both := psCases{
		rule: RuleGhPublish,
		deny: []string{
			"gh pr create -t t -b '" + lp + "'",
			"gh pr create -t x -b '" + dir + "'",
			"cd sub && gh pr create -t t --body-file b.md",
			"@'\nsee " + lp + "\n'@ | gh pr comment 1 -F -",
			"gh pr create --bogus -b '" + lp + "'",
		},
		allow: []string{
			"cd sub; gh pr create -t t --body-file b.md",
			// A location change in a pipeline leaves the cwd unknown, so b.md (a hit) is not read.
			"Set-Location sub -PassThru | gh pr create -t t --body-file b.md",
			"cd sub | gh pr create -t t --body-file b.md",
			"@'\nclean\n'@ | gh pr comment 1 -F -",
			"$t = @'\nsee " + lp + "\n'@; gh pr comment 1 -b clean",
			"@\"\nsee $(Get-Date) " + lp + "\n\"@ | gh pr comment 1 -F -",
			"Get-Date | gh pr comment 1 -F -",
			"Get-Content sub/b.md | gh pr comment 1 -F -",
			`gh pr comment 1 -F "$env:SystemDrive\b.md"`,
			"cd '" + dir + "'; gh pr create -t x -b y",
			"gh pr create -t t --body-file clean.md",
		},
	}
	for _, windows := range bothOS {
		c := both
		c.windows = windows
		f.check(c)
	}
	// Files under home: the temp home is a path of the OS the test runs on, so each host's forms
	// are asserted on that host.
	if isWindows {
		f.check(psCases{
			windows: true, rule: RuleGhPublish,
			deny: []string{
				`gh pr comment 1 -F "$HOME/b.md"`,
				`gh pr comment 1 -F "$env:USERPROFILE\b.md"`,
			},
			allow: []string{`gh pr comment 1 -F ~\b.md`, "gh pr comment 1 -F ~/b.md"},
		})
	} else {
		f.check(psCases{
			windows: false, rule: RuleGhPublish,
			deny: []string{"gh pr comment 1 -F ~/b.md", `gh pr comment 1 -F "$HOME/b.md"`},
			allow: []string{
				"gh pr comment 1 -F '~/b.md'",
				`gh pr comment 1 -F "$env:USERPROFILE/b.md"`,
			},
		})
	}

	// The body file's own local path as its argument is not published; the same path inside it is.
	if !isWindows {
		own := strings.Join([]string{"C:", "Users", "dev", "b.md"}, "/")
		f.write(own, "clean\n")
		d, ok := f.psDecide("gh pr create -t t --body-file "+own, false)
		wantAllow(t, "body file argument is its own local path", d, ok)
		f.write(own, "written to "+own+"\n")
		d, ok = f.psDecide("gh pr create -t t --body-file "+own, false)
		wantDeny(t, "body file content holds its own path", d, ok, RuleGhPublish)
	}
}

// TestRmRootPowerShell is C5.
func TestRmRootPowerShell(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.cwd, ".git", "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.write("wt/.git", "gitdir: ../.git/worktrees/wt\n")

	both := psCases{
		rule: RuleRmRoot,
		deny: []string{
			"Remove-Item -Recurse -Force $HOME",
			"Remove-Item -r ~",
			"Remove-Item -Rec '~'",
			"Remove-Item -RECURSE ~/",
			`Remove-Item -Recurse ~\`,
			"Remove-Item -Recurse:$true ${HOME}",
			`Remove-Item -Recurse "$env:USERPROFILE\"`,
			`Remove-Item -Recurse $HOME\.`,
			"ri -r ~",
			"del -r ~",
			"erase -r ~",
			"rd -r ~",
			`Microsoft.PowerShell.Management\Remove-Item -Recurse ~`,
			"Remove-Item -Recurse -Path ~",
			"Remove-Item -Recurse -LiteralPath $HOME",
			"Remove-Item -Recurse -LP $HOME",
			"Remove-Item -Recurse -PSPath $HOME",
			`Remove-Item -Recurse -LiteralPath:"$HOME"`,
			"Remove-Item -Recurse -Path a, ~",
			"Remove-Item -Recurse -Path a,~",
			"Remove-Item -Recurse -- ~",
			"Remove-Item -Recurse -WhatIf:$false ~",
			"Remove-Item -Recurse .git",
			`Remove-Item -Recurse '.\.git'`,
			"Remove-Item -Recurse ./.git/",
			"Remove-Item -Recurse -LiteralPath .git/.",
			`Remove-Item -Recurse .git\.\`,
			"Remove-Item -Recurse .git/././",
			"Remove-Item -Recurse $HOME; Write-Output ok",
		},
		allow: []string{
			"Remove-Item -Recurse:$false ~",
			"Remove-Item -Recurse -WhatIf ~",
			"Remove-Item -Recurse -WhatIf:$true ~",
			"Remove-Item ~",
			"Remove-Item -Recurse '$HOME'",
			"Remove-Item -Recurse \"`$HOME\"",
			`$env:USERPROFILE = 'C:\work\x'; Remove-Item -Recurse $env:USERPROFILE`,
			"$HOME = 'x'; Remove-Item -Recurse $HOME",
			"Remove-Item -Recurse -Path 'a,~'",
			"Remove-Item -Recurse -LiteralPath '~'",
			`Remove-Item -Recurse .git\hooks`,
			`Remove-Item -Recurse wt\.git`,
			"Remove-Item -Recurse -LiteralPath wt/.git/.",
			`Remove-Item -Recurse -Exclude a, .git .\build`,
			`Remove-Item -Recurse .\build -Filter '.git'`,
			`Remove-Item -Recurse .\build ~`,
			"Remove-Item -p ~ -Recurse",
			`Remove-Item -Recurse .\build`,
			`Remove-Item ~\x -Recurse`,
			`Remove-Item -Recurse ~\src`,
			`echo "Remove-Item -Recurse ~"`,
			"Remove-Item -Recurse $env:SystemDrive",
			"Remove-Item -Recurse $x",
			"Remove-Item -Recurse:$y ~",
			"Remove-Item -Recurse $HOME; Write-Output \"",
		},
	}
	for _, windows := range bothOS {
		c := both
		c.windows = windows
		f.check(c)
	}
	// A static path equal to the payload home, under the comparison of the host the test runs on.
	home := f.home
	d, ok := f.psDecide("Remove-Item -Recurse '"+home+"'", isWindows)
	wantDeny(t, "the home path", d, ok, RuleRmRoot)

	f.check(psCases{
		windows: true, rule: RuleRmRoot,
		deny: []string{
			`ri -r C:\`,
			"Remove-Item -Recurse c:/",
			`Remove-Item -Recurse c:\.`,
			`Remove-Item -Recurse -Path a, C:\`,
			`Remove-Item -Recurse -Path a,C:\`,
			`Remove-Item -Recurse $env:SystemDrive\`,
			`Remove-Item -Recurse ${env:SystemDrive}\`,
			`Remove-Item -Recurse \`,
			"Remove-Item -Recurse /",
			`Remove-Item -Recurse \.`,
			"Remove-Item -Recurse /.",
			"rm -r ~",
			"rmdir -r ~",
			`& 'C:\x\rm.exe' -r C:\`,
			// A declared drive leaves only its own root opaque.
			`New-PSDrive -Name X -PSProvider FileSystem -Root C:\work\build; Remove-Item -Recurse C:\`,
			`Remove-Item -Recurse X:\`,
			// A call pwsh refuses declares no drive.
			`New-PSDrive -Name X -Name X -PSProvider FileSystem -Root C:\work\build; Remove-Item -Recurse C:\`,
			`New-PSDrive -Name X -Name X -PSProvider FileSystem -Root C:\w; Remove-Item -Recurse \`,
		},
		allow: []string{
			"Remove-Item -Recurse C:",
			`Remove-Item C:\`,
			`Remove-Item -Recurse -Path 'a,C:\'`,
			"rm -rf ~",
			`& 'C:\x\rm.exe' -r '~'`,
			`& 'C:\x\rm.exe' -r ~`,
			`New-PSDrive -Name X -PSProvider FileSystem -Root C:\x; Remove-Item -Recurse \`,
			`New-PSDrive -Name X -PSProvider FileSystem -Root C:\work\build; Remove-Item -Recurse X:\`,
			`New-PSDrive x FileSystem C:\work\build; Remove-Item -Recurse X:/`,
			`ndr -Name X -PSProvider FileSystem -Root C:\work\build; Remove-Item -Recurse X:\`,
			`New-PSDrive -Name $n -PSProvider FileSystem -Root C:\w; Remove-Item -Recurse X:\`,
			`New-PSDrive X FileSystem C:\work\build; cd X:\ && Remove-Item -Recurse .`,
			`Remove-Item -Recurse HKCU:\`,
			`Remove-Item -Recurse C:\*`,
			`Remove-Item -Recurse \\server\share`,
		},
	})
	f.check(psCases{
		windows: false, rule: RuleRmRoot,
		deny: []string{
			"rm -rf ~",
			"/bin/rm -rf ~",
			"ri -r /",
			"Remove-Item -Recurse /",
			"rm -rf .git",
		},
		allow: []string{
			"rm -rf '~'",
			"rm -rf ~/src",
			`Remove-Item -Recurse $env:SystemDrive\`,
			"rm -f .git",
			"rm -rf wt/.git",
		},
	})
	// A native `$HOME` gets the hook's home, a path of the OS the test runs on.
	if !isWindows {
		d, ok := f.psDecide("rm -rf $HOME", false)
		wantDeny(t, "unix: rm -rf $HOME", d, ok, RuleRmRoot)
	}

	f.cwd = ""
	for _, cmd := range []string{`Remove-Item -Recurse \`, "Remove-Item -Recurse /"} {
		d, ok := f.psDecide(cmd, true)
		wantDeny(t, "unknown cwd: "+cmd, d, ok, RuleRmRoot)
	}
	d, ok = f.psDecide("Remove-Item -Recurse .git", true)
	wantAllow(t, "relative operand under an unknown cwd", d, ok)
}

// TestRmRootPowerShellExistingDrive checks that declaring a drive whose letter is the payload cwd's
// or home's volume declares nothing: New-PSDrive fails with DriveAlreadyExists, so that root and
// bare roots stay judged.
func TestRmRootPowerShellExistingDrive(t *testing.T) {
	f := newFixture(t)
	f.cwd, f.home = `C:\work\repo`, `D:\work\home`
	f.check(psCases{
		windows: true, rule: RuleRmRoot,
		deny: []string{
			`mount C FileSystem C:\w; Remove-Item -Recurse C:\`,
			`New-PSDrive -Name C -PSProvider FileSystem -Root C:\w; Remove-Item -Recurse C:\`,
			`ndr d FileSystem C:\w; Remove-Item -Recurse D:\`,
			`mount C FileSystem C:\w; Remove-Item -Recurse \`,
		},
		allow: []string{
			`mount X FileSystem C:\w; Remove-Item -Recurse X:\`,
			`New-PSDrive -Name X -PSProvider FileSystem -Root C:\w; Remove-Item -Recurse X:\`,
			`mount X FileSystem C:\w; Remove-Item -Recurse \`,
		},
	})
}

// TestRmRootPowerShellGitSymlink checks that a terminal `.` does not follow a `.git` symlink: the
// link itself is what the path names, and it is not a directory.
func TestRmRootPowerShellGitSymlink(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(filepath.Join(f.cwd, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(f.cwd, ".git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, windows := range bothOS {
		for _, cmd := range []string{
			"Remove-Item -Recurse -LiteralPath .git/.",
			"Remove-Item -Recurse .git/./",
			"Remove-Item -Recurse .git",
		} {
			d, ok := f.psDecide(cmd, windows)
			wantAllow(t, osName(windows)+": "+cmd, d, ok)
		}
	}
}

// TestPowerShellFailOpen is C6 for stack-guard, through hook stdout.
func TestPowerShellFailOpen(t *testing.T) {
	f := newFixture(t)
	for _, input := range []any{
		"x",
		map[string]any{"command": 7},
		map[string]any{},
		map[string]any{"command": ""},
		map[string]any{"command": "Remove-Item -Recurse $HOME; Write-Output \""},
	} {
		assertAllow(t, "tool_input "+string(mustMarshal(t, input)), f.decide("PowerShell", input))
	}
	assertDeny(t, "lex failure still scans secrets",
		f.decide("PowerShell", map[string]any{"command": "Get-Content .dev.vars; Write-Output \""}),
		RuleSecrets)
	assertDeny(t, "nested Bash", f.decide("PowerShell",
		map[string]any{"command": "bash -c 'rm -rf ~'"}), RuleRmRoot)

	// Past the syntax recursion cap the script yields no calls, as for a lexing failure.
	deep := strings.Repeat("(", 201) + "Remove-Item -Recurse $HOME" + strings.Repeat(")", 201)
	assertAllow(t, "nesting cap", f.decide("PowerShell", map[string]any{"command": deep}))
	shallow := strings.Repeat("(", 199) + "Remove-Item -Recurse $HOME" + strings.Repeat(")", 199)
	assertDeny(t, "under the nesting cap", f.decide("PowerShell",
		map[string]any{"command": shallow}), RuleRmRoot)

	// A failing nested script drops only its own calls.
	for _, cmd := range []string{
		`Remove-Item -Recurse $HOME; iex 'Write-Output "'`,
		"Remove-Item -Recurse $HOME; bash -c 'if ('",
		"bash -c 'rm -rf ~\nif ('",
	} {
		assertDeny(t, cmd, f.decide("PowerShell", map[string]any{"command": cmd}), RuleRmRoot)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
