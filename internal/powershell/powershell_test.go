package powershell

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/shuymn/stack/internal/shell"
)

var knownTag = map[Known]string{
	KnownHome: "<HOME>", KnownUserProfile: "<USERPROFILE>", KnownSystemDrive: "<SYSTEMDRIVE>",
}

// argv renders a call's words: static text, a known variable as <NAME> plus its static rest, "{}"
// for a script block and "$" for a dynamic word.
func argv(c Call) []string {
	out := make([]string, len(c.Args))
	for i, w := range c.Args {
		switch {
		case w.Block:
			out[i] = "{}"
		case w.Known != NotKnown:
			out[i] = w.Flag + knownTag[w.Known] + w.Text
		case w.Static:
			out[i] = w.Flag + w.Text
		default:
			out[i] = "$"
		}
	}
	return out
}

func argvs(r Result) [][]string {
	out := make([][]string, len(r.Calls))
	for i, c := range r.Calls {
		out[i] = argv(c)
	}
	return out
}

func parse(t *testing.T, cmd string, os OS) Result {
	t.Helper()
	r := Parse(cmd, "", "", os)
	if r.Err != nil {
		t.Fatalf("Parse(%q) error = %v", cmd, r.Err)
	}
	return r
}

func find(t *testing.T, r Result, name string) Call {
	t.Helper()
	for _, c := range r.Calls {
		if c.Is(name) {
			return c
		}
	}
	t.Fatalf("no %s call in %q", name, argvs(r))
	return Call{}
}

type calls = [][]string

// parseTests are the C1 cases of powershell-guard §7.2: the exact argv of every call per form.
var parseTests = []struct {
	name string
	cmd  string
	os   OS
	want calls
}{
	{
		"semicolon",
		`gh pr view 1; gh pr merge 2`,
		Unix,
		calls{{"gh", "pr", "view", "1"}, {"gh", "pr", "merge", "2"}},
	},
	{"pipe", `Get-Date | gh pr merge 1`, Unix, calls{{"Get-Date"}, {"gh", "pr", "merge", "1"}}},
	{
		"and or",
		`git status && gh pr merge 1 || gh x`,
		Unix,
		calls{{"git", "status"}, {"gh", "pr", "merge", "1"}, {"gh", "x"}},
	},
	{
		"newline",
		"git status\r\ngh pr merge 1",
		Unix,
		calls{{"git", "status"}, {"gh", "pr", "merge", "1"}},
	},
	{"continuation", "gh pr `\n  merge 1", Unix, calls{{"gh", "pr", "merge", "1"}}},
	{"background", `gh pr merge 1 &`, Unix, calls{{"gh", "pr", "merge", "1"}}},
	{
		"call quoted path",
		`& 'x y\gh.exe' pr merge 1`,
		Unix,
		calls{{`x y\gh.exe`, "pr", "merge", "1"}},
	},
	{"dot source", `. gh pr merge 1`, Unix, calls{{"gh", "pr", "merge", "1"}}},
	{"backtick in name", "g`h pr merge 1", Unix, calls{{"gh", "pr", "merge", "1"}}},
	{"return", `return gh pr merge 7`, Unix, calls{{"gh", "pr", "merge", "7"}}},
	{
		"equals is argument text",
		`Write-Output note=gh pr merge 7`,
		Unix,
		calls{{"Write-Output", "note=gh", "pr", "merge", "7"}},
	},

	{"invoked block", `& { gh pr merge 7 }`, Unix, calls{{"gh", "pr", "merge", "7"}}},
	{"if body", `if ($ok) { gh pr merge 7 }`, Unix, calls{{"gh", "pr", "merge", "7"}}},
	{
		"elseif else",
		"if ($a) { gh a } elseif ($b) { gh b }\nelse { gh c }",
		Unix,
		calls{{"gh", "a"}, {"gh", "b"}, {"gh", "c"}},
	},
	{"try catch", `try { gh pr merge 7 } catch {}`, Unix, calls{{"gh", "pr", "merge", "7"}}},
	{
		"try catch type finally",
		`try { gh a } catch [System.Exception] { gh b } finally { gh c }`,
		Unix,
		calls{{"gh", "a"}, {"gh", "b"}, {"gh", "c"}},
	},
	{
		"foreach object",
		`1..3 | ForEach-Object { gh pr merge 7 }`,
		Unix,
		calls{{"ForEach-Object", "{}"}, {"gh", "pr", "merge", "7"}},
	},
	{
		"foreach alias no space",
		`1..3 | %{ gh pr merge 7 }`,
		Unix,
		calls{{"%", "{}"}, {"gh", "pr", "merge", "7"}},
	},
	{
		"foreach process",
		`$a | foreach -Process { gh a } -Begin { gh b }`,
		Unix,
		calls{{"foreach", "-Process", "{}", "-Begin", "{}"}, {"gh", "a"}, {"gh", "b"}},
	},
	{
		"foreach remaining block",
		`$a | % { gh a } { gh b }`,
		Unix,
		calls{{"%", "{}", "{}"}, {"gh", "a"}},
	},
	{
		"where object",
		`$a | ? { gh pr merge 7 }`,
		Unix,
		calls{{"?", "{}"}, {"gh", "pr", "merge", "7"}},
	},
	{
		"foreach loop",
		`foreach ($x in $list) { gh pr merge $x }`,
		Unix,
		calls{{"gh", "pr", "merge", "$"}},
	},
	{
		"for while do",
		`for ($i=0; $i -lt 2; $i++) { gh a }; while ($true) { gh b }; do { gh c } until ($x)`,
		Unix,
		calls{{"gh", "a"}, {"gh", "b"}, {"gh", "c"}},
	},
	{
		"switch",
		`switch ($x) { 'a' { gh a } default { gh b } }`,
		Unix,
		calls{{"gh", "a"}, {"gh", "b"}},
	},
	{"assigned block", `$p = { gh pr merge 7 }`, Unix, calls{}},
	{
		"block argument",
		`Write-Output { gh pr merge 7 } | Get-Member`,
		Unix,
		calls{{"Write-Output", "{}"}, {"Get-Member"}},
	},
	{"function body", `function f { gh pr merge 7 }`, Unix, calls{}},

	{
		"icm script block",
		`icm -ScriptBlock { gh pr merge 7 }`,
		Unix,
		calls{{"icm", "-ScriptBlock", "{}"}, {"gh", "pr", "merge", "7"}},
	},
	{
		"icm positional block",
		`Invoke-Command { gh pr merge 7 } -ArgumentList 1 -NoNewScope`,
		Unix,
		calls{
			{"Invoke-Command", "{}", "-ArgumentList", "1", "-NoNewScope"},
			{"gh", "pr", "merge", "7"},
		},
	},
	{
		"icm argument list block",
		`icm -ScriptBlock { 'x' } -ArgumentList { gh pr merge 7 }`,
		Unix,
		calls{{"icm", "-ScriptBlock", "{}", "-ArgumentList", "{}"}},
	},
	{
		"icm computer name",
		`icm -ComputerName h { gh pr merge 7 }`,
		Unix,
		calls{{"icm", "-ComputerName", "h", "{}"}},
	},
	{
		"icm host name",
		`icm -HostName h -ScriptBlock { gh pr merge 7 }`,
		Unix,
		calls{{"icm", "-HostName", "h", "-ScriptBlock", "{}"}},
	},
	{
		"icm positional host",
		`icm h -ScriptBlock { gh pr merge 7 }`,
		Unix,
		calls{{"icm", "h", "-ScriptBlock", "{}"}},
	},
	{"start job", `Start-Job { gh pr merge 7 }`, Unix, calls{{"Start-Job", "{}"}}},

	{
		"merging redirection",
		`gh pr merge 7 2>&1 --help`,
		Unix,
		calls{{"gh", "pr", "merge", "7", "--help"}},
	},
	{
		"file redirections",
		`gh pr view 1 > out.txt 2>> err.txt *> all.txt`,
		Unix,
		calls{{"gh", "pr", "view", "1"}},
	},
	{
		"redirection target subexpression",
		`Write-Output ok > $(gh pr merge 7; 'out.txt')`,
		Unix,
		calls{{"gh", "pr", "merge", "7"}, {"Write-Output", "ok"}},
	},
	{
		"hash value command",
		`@{ Result = gh pr merge 7 }`,
		Unix,
		calls{{"gh", "pr", "merge", "7"}},
	},
	{"hash value string", `@{ Result = 'gh pr merge 7' }`, Unix, calls{}},
	{"assignment", `$x = gh pr merge 7`, Unix, calls{{"gh", "pr", "merge", "7"}}},
	{"typed assignment", `[string]$x = gh pr merge 7`, Unix, calls{{"gh", "pr", "merge", "7"}}},
	{
		"argument equals",
		`echo result = gh pr merge 7`,
		Unix,
		calls{{"echo", "result", "=", "gh", "pr", "merge", "7"}},
	},
	{
		"comparison is no assignment",
		`$x -eq (gh pr view 1)`,
		Unix,
		calls{{"gh", "pr", "view", "1"}},
	},
	{
		"native words",
		`gh pr merge 7 --disable-auto=false -f k=v a,b -Name:value`,
		Unix,
		calls{
			{
				"gh",
				"pr",
				"merge",
				"7",
				"--disable-auto=false",
				"-f",
				"k=v",
				"a,b",
				"-Name:value",
			},
		},
	},
	{
		"cmdlet list",
		`Remove-Item -Path a,C:\`,
		Unix,
		calls{{"Remove-Item", "-Path", "a", `C:\`}},
	},
	{
		"cmdlet list spaced",
		`Remove-Item -Path a, C:\`,
		Unix,
		calls{{"Remove-Item", "-Path", "a", `C:\`}},
	},
	{
		"cmdlet quoted comma",
		`Remove-Item -Path 'a,C:\'`,
		Unix,
		calls{{"Remove-Item", "-Path", `a,C:\`}},
	},

	{
		"here-string closer at column 0",
		"Write-Output @'\n  '@\ngh pr merge 7\n'@\ngh pr view 1",
		Unix,
		calls{{"Write-Output", "  '@\ngh pr merge 7"}, {"gh", "pr", "view", "1"}},
	},
	{
		"here-string opener with blanks",
		"Write-Output @'   \nx\n'@",
		Unix,
		calls{{"Write-Output", "x"}},
	},
	{"here-string empty", "Write-Output @\"\n\"@", Unix, calls{{"Write-Output", ""}}},
	{
		"expandable here-string subexpression",
		"Write-Output @\"\n$(gh pr merge 7)\n\"@",
		Unix,
		calls{{"gh", "pr", "merge", "7"}, {"Write-Output", "$"}},
	},

	{
		"verbatim pipe",
		`gh --% pr merge 7 | Write-Output x`,
		Unix,
		calls{{"gh", "pr", "merge", "7"}, {"Write-Output", "x"}},
	},
	{
		"verbatim keeps semicolon and quoted pipe",
		`git --% a; "b|c" d && gh pr merge 7`,
		Unix,
		calls{{"git", "a;", "$", "d"}, {"gh", "pr", "merge", "7"}},
	},
	{
		"verbatim ends at a newline inside quotes",
		"git --% \"x\ngit status\r\ngit --% a \"b\r\ngit diff",
		Unix,
		calls{{"git", "$"}, {"git", "status"}, {"git", "a", "$"}, {"git", "diff"}},
	},
	{
		"verbatim or",
		`git --% status || gh pr merge 7`,
		Unix,
		calls{{"git", "status"}, {"gh", "pr", "merge", "7"}},
	},
	{
		"verbatim newline",
		"git --% status\ngh pr merge 7",
		Unix,
		calls{{"git", "status"}, {"gh", "pr", "merge", "7"}},
	},
	{
		"verbatim percent variable",
		`cmd --% /c echo %USERPROFILE%\x`,
		Windows,
		calls{{"cmd", "/c", "echo", "$"}},
	},

	{
		"string literal",
		`echo "gh pr merge 1"; Write-Output 'gh pr merge 1'`,
		Unix,
		calls{{"echo", "gh pr merge 1"}, {"Write-Output", "gh pr merge 1"}},
	},
	{"string statement", `"gh pr merge 1"`, Unix, calls{}},
	{
		"subexpression in string",
		`echo "$(gh pr merge 1)"`,
		Unix,
		calls{{"gh", "pr", "merge", "1"}, {"echo", "$"}},
	},
	{"quoted call", `& 'gh' pr merge 1`, Unix, calls{{"gh", "pr", "merge", "1"}}},
	{
		"comments",
		"# gh pr merge 1\n<# gh pr merge 2\n#> gh pr view 3 # gh pr merge 4",
		Unix,
		calls{{"gh", "pr", "view", "3"}},
	},
	{
		"smart quotes",
		"Write-Output \u2018gh pr merge 1\u2019 \u201cx\u201d",
		Unix,
		calls{{"Write-Output", "gh pr merge 1", "x"}},
	},
	{
		"escapes",
		"Write-Output \"a`tb`\"c\"\"d\" 'it''s'",
		Unix,
		calls{{"Write-Output", "a\tb\"c\"d", "it's"}},
	},
	{"number statement", `7; 1..3 | gh x`, Unix, calls{{"gh", "x"}}},
	{
		"dynamic words",
		`gh $x @a (Get-Date) $(Get-Date) "a$x" $x.y`,
		Unix,
		calls{{"Get-Date"}, {"Get-Date"}, {"gh", "$", "$", "$", "$", "$", "$"}},
	},
	{"array subexpression", `@(gh pr merge 1)`, Unix, calls{{"gh", "pr", "merge", "1"}}},
	{
		"leading subexpression ends its word",
		`gh pr merge (7)--help $(1)x @(2)y a$(3)b`,
		Unix,
		calls{{"gh", "pr", "merge", "$", "--help", "$", "x", "$", "y", "$"}},
	},
	{
		"method arguments are opaque",
		`$x.Invoke(gh pr view 7); [IO.File]::ReadAllText((gh pr view 1)); (Get-Item a).Delete(gh x); git status`,
		Unix,
		calls{{"Get-Item", "a"}, {"git", "status"}},
	},
	{
		"method argument subexpression",
		`$x.M($(gh pr merge 1)); Write-Output $x.Invoke(gh pr view 7)`,
		Unix,
		calls{{"gh", "pr", "merge", "1"}, {"Write-Output", "$"}},
	},
	{
		"member access and index make a variable word dynamic",
		`gh -F ${HOME}.Length $HOME.Length ${HOME}[0] $HOME[0] "${HOME}.x" "$HOME[0]"`,
		Unix,
		calls{{"gh", "-F", "$", "$", "$", "$", "<HOME>.x", "<HOME>[0]"}},
	},
	{
		"using directive consumed whole",
		"using module @{ ModuleName = 'Microsoft.PowerShell.Management'; ModuleVersion = '1.0' }\nusing namespace System.IO; git status\ngit diff",
		Unix,
		calls{{"git", "status"}, {"git", "diff"}},
	},
	{
		"foreach object other parameter set",
		`1 | ForEach-Object -Parallel { 'x' } -Process { git status }; $a | % { git a } -ArgumentList 1`,
		Unix,
		calls{
			{"ForEach-Object", "-Parallel", "{}", "-Process", "{}"},
			{"%", "{}", "-ArgumentList", "1"},
		},
	},
	{
		"where object other parameter set",
		`$a | ? -Property x { git status }; $a | Where-Object -FilterScript { gh x } -InputObject $b`,
		Unix,
		calls{
			{"?", "-Property", "x", "{}"},
			{"Where-Object", "-FilterScript", "{}", "-InputObject", "$"},
			{"gh", "x"},
		},
	},
	{
		"native wildcard dynamic on unix",
		"git add *.txt a?b [x]y '*.q' \"*.d\" a`*b; Remove-Item *.txt",
		Unix,
		calls{{"git", "add", "$", "$", "$", "*.q", "*.d", "$"}, {"Remove-Item", "*.txt"}},
	},
	{
		"native wildcard static on windows",
		`git add *.txt a?b`,
		Windows,
		calls{{"git", "add", "*.txt", "a?b"}},
	},
	{
		"trap bodies",
		`trap { gh a }; trap [System.Exception] { gh b }`,
		Unix,
		calls{{"gh", "a"}, {"gh", "b"}},
	},
	{
		"throw and exit pipelines",
		"throw gh a\nexit gh b | git c",
		Unix,
		calls{{"gh", "a"}, {"gh", "b"}, {"git", "c"}},
	},
	{
		"postfix access after a leading subexpression",
		`gh pr view ('abc').Length $(1)[0] @(2)::X (7)--help`,
		Unix,
		calls{{"gh", "pr", "view", "$", "$", "$", "$", "--help"}},
	},
	{
		"end of parameters in an executing cmdlet",
		`1 | ForEach-Object -- { git status }`,
		Unix,
		calls{{"ForEach-Object", "--", "{}"}, {"git", "status"}},
	},
	{
		"verbatim smart double quotes",
		"git --% log “text | git status # ” x „a”",
		Unix,
		calls{{"git", "log", "$", "x", "$"}},
	},
	{
		"keyword prefix is a command",
		`foreach-thing x; iffy y`,
		Unix,
		calls{{"foreach-thing", "x"}, {"iffy", "y"}},
	},
}

func TestParse(t *testing.T) {
	for _, tt := range parseTests {
		t.Run(tt.name, func(t *testing.T) {
			got := argvs(parse(t, tt.cmd, tt.os))
			if !slices.EqualFunc(got, tt.want, slices.Equal) {
				t.Errorf("%q calls = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestParseCommaGroups(t *testing.T) {
	c := find(t, parse(t, `Remove-Item -Path a, C:\ -Exclude 'a,b'`, Unix), "Remove-Item")
	if c.Args[2].Group == 0 || c.Args[2].Group != c.Args[3].Group {
		t.Errorf("list groups = %d, %d; want one shared group", c.Args[2].Group, c.Args[3].Group)
	}
	if c.Args[5].Group != 0 || c.Args[5].Text != "a,b" {
		t.Errorf("quoted word = %+v; want one literal", c.Args[5])
	}
}

func TestParseKind(t *testing.T) {
	tests := []struct {
		cmd  string
		os   OS
		kind Kind
		name string
	}{
		{`Remove-Item x`, Unix, Cmdlet, "Remove-Item"},
		{`remove-item x`, Unix, Cmdlet, "Remove-Item"},
		{`Microsoft.PowerShell.Management\Remove-Item x`, Unix, Cmdlet, "Remove-Item"},
		{`ri x`, Unix, Cmdlet, "Remove-Item"},
		{`del x`, Unix, Cmdlet, "Remove-Item"},
		{`rm -r x`, Windows, Cmdlet, "Remove-Item"},
		{`rm -rf x`, Unix, Native, "rm"},
		{`rmdir x`, Unix, Native, "rmdir"},
		{`& 'C:\x\rm.exe' -r '~'`, Windows, Native, "rm"},
		{`rm.exe -r x`, Windows, Native, "rm"},
		{`/bin/rm -r x`, Unix, Native, "rm"},
		{`GH.EXE pr view`, Windows, Native, "GH"},
		{`Write-Output x`, Unix, Cmdlet, "Write-Output"},
		{`pre-commit run`, Unix, Native, "pre-commit"},
	}
	for _, tt := range tests {
		r := parse(t, tt.cmd, tt.os)
		if len(r.Calls) != 1 {
			t.Fatalf("%q calls = %q", tt.cmd, argvs(r))
		}
		if c := r.Calls[0]; c.Kind != tt.kind || c.Name != tt.name {
			t.Errorf(
				"%q (os %d) = kind %d name %q; want %d %q",
				tt.cmd,
				tt.os,
				c.Kind,
				c.Name,
				tt.kind,
				tt.name,
			)
		}
	}
	if !parse(t, `GH.EXE pr view`, Windows).Calls[0].Is("gh") {
		t.Error("GH.EXE is not gh on Windows")
	}
	if parse(t, `GH pr view`, Unix).Calls[0].Is("gh") {
		t.Error("GH is gh on Unix")
	}
	if c := parse(t, `& 'C:\x\rm.exe' -r '~'`, Windows).Calls[0]; c.Bound {
		t.Error("a native rm was bound as a cmdlet")
	}
}

func TestParseKnownVariables(t *testing.T) {
	cmd := "Write-Output $HOME \"${HOME}\\x\" $env:USERPROFILE ${env:SystemDrive}\\ '$HOME' \"`$HOME\" \"a$HOME\" $Env:userprofile\\y"
	c := find(t, parse(t, cmd, Unix), "Write-Output")
	want := []string{
		"Write-Output",
		"<HOME>",
		`<HOME>\x`,
		"<USERPROFILE>",
		`<SYSTEMDRIVE>\`,
		"$HOME",
		"$HOME",
		"$",
		`<USERPROFILE>\y`,
	}
	if got := argv(c); !slices.Equal(got, want) {
		t.Errorf("words = %q, want %q", got, want)
	}
	if c.Args[2].Quote != Double || c.Args[5].Quote != Single || c.Args[1].Quote != Bare {
		t.Errorf("quotes = %d %d %d", c.Args[2].Quote, c.Args[5].Quote, c.Args[1].Quote)
	}

	for _, cmd := range []string{
		`$HOME = 'x'; Write-Output $HOME`,
		`$env:USERPROFILE = 'x'; Write-Output $HOME $env:USERPROFILE`,
		`Set-Variable HOME x; Write-Output $HOME`,
		`Set-Item env:USERPROFILE x; Write-Output $env:USERPROFILE`,
		`iex '$HOME = 1'; Write-Output $HOME`,
	} {
		r := parse(t, cmd, Unix)
		c := find(t, r, "Write-Output")
		last := c.Args[len(c.Args)-1]
		if last.Known != NotKnown {
			t.Errorf("%q: assigned variable still known: %+v", cmd, last)
		}
	}
	if c := find(
		t,
		parse(t, `$p = { $HOME = 1 }; Write-Output $HOME`, Unix),
		"Write-Output",
	); c.Args[1].Known != KnownHome {
		t.Errorf("an assignment in an opaque block made $HOME dynamic")
	}

	r := parse(t, "Remove-Item -LiteralPath:\"$HOME\"; Remove-Item -LiteralPath:\"`$HOME\"", Unix)
	a, b := r.Calls[0].Args[1], r.Calls[1].Args[1]
	if a.Known != KnownHome || a.Flag != "-LiteralPath:" || a.Param != "LiteralPath" ||
		a.Quote != Double {
		t.Errorf("colon-bound known word = %+v", a)
	}
	if !b.Static || b.Text != "$HOME" || b.Known != NotKnown || b.Param != "LiteralPath" {
		t.Errorf("colon-bound escaped word = %+v", b)
	}
}

func TestParseNested(t *testing.T) {
	tests := []struct {
		cmd    string
		calls  calls
		nested []Script
	}{
		{
			`iex 'gh pr merge 7'`,
			calls{{"iex", "gh pr merge 7"}, {"gh", "pr", "merge", "7"}},
			[]Script{{Dialect: PowerShell, Text: "gh pr merge 7", Depth: 1}},
		},
		{
			`bash -c 'gh pr merge 1'`,
			calls{{"bash", "-c", "gh pr merge 1"}},
			[]Script{{Dialect: Bash, Text: "gh pr merge 1", Depth: 1}},
		},
		{
			`pwsh -NoProfile -Command 'gh pr merge 1'`,
			calls{{"pwsh", "-NoProfile", "-Command", "gh pr merge 1"}, {"gh", "pr", "merge", "1"}},
			[]Script{{Dialect: PowerShell, Text: "gh pr merge 1", Depth: 1}},
		},
		{
			`iex "bash -c 'gh pr merge 7'"`,
			calls{{"iex", "bash -c 'gh pr merge 7'"}, {"bash", "-c", "gh pr merge 7"}},
			[]Script{
				{Dialect: PowerShell, Text: "bash -c 'gh pr merge 7'", Depth: 1},
				{Dialect: Bash, Text: "gh pr merge 7", Depth: 2},
			},
		},
		{
			`iex 'bash -c ''bash -c "gh pr merge 7"'''`,
			calls{
				{"iex", `bash -c 'bash -c "gh pr merge 7"'`},
				{"bash", "-c", `bash -c "gh pr merge 7"`},
			},
			[]Script{
				{Dialect: PowerShell, Text: `bash -c 'bash -c "gh pr merge 7"'`, Depth: 1},
				{Dialect: Bash, Text: `bash -c "gh pr merge 7"`, Depth: 2},
			},
		},
		{
			`iex "iex 'iex ''gh pr merge 7'''"`,
			calls{
				{"iex", "iex 'iex ''gh pr merge 7'''"},
				{"iex", "iex 'gh pr merge 7'"},
				{"iex", "gh pr merge 7"},
			},
			[]Script{
				{Dialect: PowerShell, Text: "iex 'iex ''gh pr merge 7'''", Depth: 1},
				{Dialect: PowerShell, Text: "iex 'gh pr merge 7'", Depth: 2},
			},
		},
		{`iex $x; bash -c "gh $x"`, calls{{"iex", "$"}, {"bash", "-c", "$"}}, nil},
	}
	for _, tt := range tests {
		r := parse(t, tt.cmd, Unix)
		if got := argvs(r); !slices.EqualFunc(got, tt.calls, slices.Equal) {
			t.Errorf("%q calls = %q, want %q", tt.cmd, got, tt.calls)
		}
		if !slices.Equal(r.Nested, tt.nested) {
			t.Errorf("%q nested = %+v, want %+v", tt.cmd, r.Nested, tt.nested)
		}
	}

	r := parse(t, `gh pr merge 7; iex 'Write-Output "'`, Unix)
	if len(r.Calls) != 2 || !r.Calls[0].Is("gh") || len(r.Nested) != 1 || r.Nested[0].Err == nil {
		t.Errorf("failed nested script: calls %q nested %+v", argvs(r), r.Nested)
	}
	// The valid prefix of a nested script that fails to lex is dropped with it: pwsh runs none of it.
	r = parse(t, `iex 'git status; Write-Output "'; git diff`, Unix)
	if want := (calls{{"iex", `git status; Write-Output "`}, {"git", "diff"}}); !slices.EqualFunc(
		argvs(r),
		want,
		slices.Equal,
	) {
		t.Errorf("failed nested script calls = %q, want %q", argvs(r), want)
	}
}

func TestParseFailure(t *testing.T) {
	for _, cmd := range []string{
		`gh pr merge 7; Write-Output "`,
		`gh pr merge 7; Write-Output 'x`,
		"gh pr merge 7; Write-Output @'\nx",
		`gh pr merge 7; if ($x) { gh y`,
		`gh pr merge 7; (gh x`,
		`gh pr merge 7 }`,
		`gh pr merge 7 <# x`,
		`gh pr merge 7 < in.txt`,
		"gh pr merge 7; using namespace \"\ngit status",
		`$x.M(}); git status`,
		`[T]::M(]); git status`,
	} {
		if r := Parse(cmd, "", "", Unix); r.Err == nil || len(r.Calls) != 0 || len(r.Nested) != 0 {
			t.Errorf(
				"Parse(%q) = calls %q err %v; want an error and no calls",
				cmd,
				argvs(r),
				r.Err,
			)
		}
	}
}

func TestParseStdin(t *testing.T) {
	tests := []struct {
		cmd  string
		kind shell.StdinKind
		text string
	}{
		{`'body' | gh pr comment 1 -F -`, shell.StdinText, "body\n"},
		{"@'\nline 1\nline 2\n'@ | gh pr comment 1 -F -", shell.StdinText, "line 1\nline 2\n"},
		{"@\"\n$(Get-Date)\n\"@ | gh pr comment 1 -F -", shell.StdinDynamic, ""},
		{`Get-Date | gh pr comment 1 -F -`, shell.StdinDynamic, ""},
		{`Get-Content b.md | gh pr comment 1 -F -`, shell.StdinDynamic, ""},
		{`gh pr comment 1 -F -`, shell.StdinNone, ""},
	}
	for _, tt := range tests {
		c := find(t, parse(t, tt.cmd, Unix), "gh")
		if c.Stdin.Kind != tt.kind || c.Stdin.Text != tt.text {
			t.Errorf("%q stdin = %+v, want %d %q", tt.cmd, c.Stdin, tt.kind, tt.text)
		}
	}
}

func TestParseCwd(t *testing.T) {
	cwd := t.TempDir()
	sub := filepath.Join(cwd, "sub")
	tests := []struct {
		cmd  string
		want string
	}{
		{`gh x`, cwd},
		{`cd sub && gh x`, sub},
		{`Set-Location -Path sub && git a && gh x`, sub},
		{`sl sub; gh x`, ""},
		{`cd sub && git a; gh x`, ""},
		{`cd; gh x`, ""},
		{`cd $x && gh x`, ""},
		{`pushd sub && gh x`, ""},
		{`cd sub || gh x`, ""},
		{`Set-Location sub -PassThru | gh x`, ""},
		{`cd sub | gh x`, ""},
		{`cd sub | Write-Output; gh x`, ""},
		{`cd sub | Write-Output && gh x`, ""},
		{`if ($a) { cd sub }; gh x`, ""},
		{`$p = { cd sub }; gh x`, cwd},
		{`foreach ($d in $l) { gh x; cd sub }`, ""},
		{`& ./script.ps1; gh x`, ""},
		// FileSystem-provider separators on every host; history, wildcards and providers unknown.
		{
			`Set-Location '.\sub' && gh x`,
			cwd + string(filepath.Separator) + filepath.FromSlash("./sub"),
		},
		{`cd sub\a && gh x`, filepath.Join(sub, "a")},
		{`cd - && gh x`, ""},
		{`cd + && gh x`, ""},
		{`cd s* && gh x`, ""},
		{`cd su? && gh x`, ""},
		{`cd [s]ub && gh x`, ""},
		{`Set-Location -Path '[s]ub' && gh x`, ""},
		{`Set-Location -LiteralPath '[s]ub' && gh x`, cwd + string(filepath.Separator) + "[s]ub"},
		{`cd Env:\ && gh x`, ""},
		{`cd FileSystem::/tmp && gh x`, ""},
	}
	for _, tt := range tests {
		r := Parse(tt.cmd, cwd, "", Unix)
		var got []string
		for _, c := range r.Calls {
			if c.Is("gh") {
				got = append(got, c.Cwd)
			}
		}
		if len(got) != 1 || got[0] != tt.want {
			t.Errorf("%q gh cwd = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

func TestParseBinding(t *testing.T) {
	tests := []struct {
		cmd     string
		refused bool
		params  []string // Param of each word after the name
	}{
		{`Remove-Item -r -Path a`, false, []string{"Recurse", "Path", "Path"}},
		{`Remove-Item -Rec -RECURSE x`, true, nil},
		{`Remove-Item x -Recurse:$false -WhatIf`, false, []string{"Path", "Recurse", "WhatIf"}},
		{
			"Remove-Item \u2013Recurse -LP a",
			false,
			[]string{"Recurse", "LiteralPath", "LiteralPath"},
		},
		{
			`Remove-Item -PSPath a -Exclude b`,
			false,
			[]string{"LiteralPath", "LiteralPath", "Exclude", "Exclude"},
		},
		{`Remove-Item -p x`, true, nil},
		{`Remove-Item -Bogus x`, true, nil},
		{`Remove-Item .\build ~`, true, nil},
		{`Remove-Item -Path`, true, nil},
		{`Remove-Item -Recurse -- -x`, false, []string{"Recurse", "", "Path"}},
		{`ndr X FileSystem C:\w`, false, []string{"Name", "PSProvider", "Root"}},
	}
	for _, tt := range tests {
		c := parse(t, tt.cmd, Unix).Calls[0]
		if !c.Bound || c.Refused != tt.refused {
			t.Errorf(
				"%q bound %v refused %v, want refused %v",
				tt.cmd,
				c.Bound,
				c.Refused,
				tt.refused,
			)
			continue
		}
		if tt.params == nil {
			continue
		}
		var got []string
		for _, w := range c.Args[1:] {
			got = append(got, w.Param)
		}
		if !slices.Equal(got, tt.params) {
			t.Errorf("%q params = %q, want %q", tt.cmd, got, tt.params)
		}
	}
}

func TestShellCall(t *testing.T) {
	home := t.TempDir()
	cmd := `gh -F ~/b.md '~/c' "$HOME/d" "$env:USERPROFILE/e" "$env:SystemDrive\f" $x`
	tests := []struct {
		os   OS
		want []shell.Word
	}{
		{Unix, []shell.Word{
			{Text: "gh", Static: true},
			{Text: "-F", Static: true},
			{Text: "~/b.md", Static: true, Tilde: true},
			{Text: "~/c", Static: true},
			{Text: home + "/d", Static: true},
			{},
			{},
			{},
		}},
		{Windows, []shell.Word{
			{Text: "gh", Static: true},
			{Text: "-F", Static: true},
			{},
			{Text: "~/c", Static: true},
			{Text: home + "/d", Static: true},
			{Text: home + "/e", Static: true},
			{},
			{},
		}},
	}
	for _, tt := range tests {
		r := Parse(cmd, "", home, tt.os)
		sc := find(t, r, "gh").ShellCall()
		if !slices.Equal(sc.Args, tt.want) {
			t.Errorf("os %d ShellCall args = %+v, want %+v", tt.os, sc.Args, tt.want)
		}
	}
	if sc := find(t, Parse(`gh "$HOME"`, "", "", Unix), "gh").ShellCall(); sc.Args[1].Static {
		t.Error("$HOME with no known home is static")
	}
	sc := find(t, Parse(`gh --% ~ x`, "", home, Unix), "gh").ShellCall()
	if want := []shell.Word{
		{Text: "gh", Static: true},
		{Text: "~", Static: true},
		{Text: "x", Static: true},
	}; !slices.Equal(
		sc.Args,
		want,
	) {
		t.Errorf("verbatim ShellCall = %+v", sc.Args)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`gh pr merge 1; & { gh x } | % { $_ }`,
		"@'\nx\n'@ | gh -F -",
		`iex "bash -c 'gh x'"; Remove-Item -Recurse -Path a,b -WhatIf:$false`,
		`git --% "a|b" && $x = @{ k = gh y }`,
		"if ($a) { cd x } elseif ($b) { } else { [int]$c = (1..3) }",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r := Parse(s, "/w", "/h", Unix)
		if r.Err != nil && (len(r.Calls) > 0 || len(r.Nested) > 0) {
			t.Fatalf("error with calls: %v", r.Err)
		}
		for _, c := range r.Calls {
			if sc := c.ShellCall(); len(sc.Args) != len(c.Args) || len(c.Args) == 0 {
				t.Fatalf("call %q adapted to %d words", argv(c), len(sc.Args))
			}
		}
		_ = Parse(s, `C:\w`, `C:\h`, Windows)
	})
}
