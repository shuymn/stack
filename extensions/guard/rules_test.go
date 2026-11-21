package guard

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fixture is a home and a cwd under t.TempDir with no safety.toml (the defaults).
type fixture struct {
	t         *testing.T
	home, cwd string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, home: t.TempDir(), cwd: t.TempDir()}
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.cwd, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) decide(tool string, input any) result {
	f.t.Helper()
	return runHook(f.t, f.home, payload(f.t, f.cwd, tool, input))
}

func (f *fixture) bash(cmd string) result {
	f.t.Helper()
	return f.decide("Bash", map[string]any{"command": cmd})
}

func TestSecretsFileTools(t *testing.T) {
	f := newFixture(t)
	secretDir := filepath.Join(f.home, filepath.FromSlash(t3Rel))
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(secretDir, "token")
	if err := os.WriteFile(token, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	example := secretDir + "-example" + string(filepath.Separator) + "token"
	t3Dir := filepath.Dir(filepath.Dir(secretDir))
	underT3 := filepath.ToSlash(strings.TrimPrefix(secretDir, t3Dir+string(filepath.Separator)))

	deny := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{"Read relative to cwd", "Read", map[string]any{"file_path": ".dev.vars"}},
		{
			"Read nested",
			"Read",
			map[string]any{"file_path": filepath.Join(f.cwd, "app", ".dev.vars")},
		},
		{"Read in secrets dir", "Read", map[string]any{"file_path": token}},
		{"Read ~ form", "Read", map[string]any{"file_path": t3Secrets + "/token"}},
		{"Read secrets dir itself", "Read", map[string]any{"file_path": secretDir}},
		{"NotebookRead", "NotebookRead", map[string]any{"notebook_path": ".dev.vars"}},
		{"Grep.path", "Grep", map[string]any{"pattern": "KEY", "path": secretDir}},
		{"Grep.path bare entry", "Grep", map[string]any{"pattern": "KEY", "path": ".dev.vars"}},
		{"Grep.glob", "Grep", map[string]any{"pattern": "KEY", "glob": ".dev.vars*"}},
		{"Grep.glob nested", "Grep", map[string]any{"pattern": "KEY", "glob": "**/.dev.vars"}},
		{"Glob.pattern", "Glob", map[string]any{"pattern": "**/.dev.vars"}},
		{
			"Glob.pattern under home",
			"Glob",
			map[string]any{"pattern": underT3 + "/*", "path": t3Dir},
		},
		{
			"Glob.pattern absolute",
			"Glob",
			map[string]any{"pattern": filepath.ToSlash(secretDir) + "/*"},
		},
	}
	for _, tt := range deny {
		assertDeny(t, tt.name, f.decide(tt.tool, tt.input), RuleSecrets)
	}

	if err := os.Symlink(token, filepath.Join(f.cwd, "link")); err == nil {
		assertDeny(t, "Read symlink into secrets",
			f.decide("Read", map[string]any{"file_path": "link"}), RuleSecrets)
		assertAllow(t, "Grep does not follow symlinks",
			f.decide("Grep", map[string]any{"pattern": "x", "path": "link"}))
	} else if !isWindows {
		t.Fatal(err)
	}

	allow := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{".dev.vars.example", "Read", map[string]any{"file_path": ".dev.vars.example"}},
		{"secrets-example", "Read", map[string]any{"file_path": example}},
		{"ordinary file", "Read", map[string]any{"file_path": "main.go"}},
		{"Grep.glob *", "Grep", map[string]any{"pattern": "KEY", "glob": "*"}},
		{"Grep.glob *.example", "Grep", map[string]any{"pattern": "KEY", "glob": "*.example"}},
		{"Glob *.go", "Glob", map[string]any{"pattern": "**/*.go"}},
		{"Glob .dev.vars.example", "Glob", map[string]any{"pattern": "**/.dev.vars.example"}},
		{"Grep negated glob", "Grep", map[string]any{"pattern": "KEY", "glob": "!**/.dev.vars"}},
		{
			"Grep negated globs only",
			"Grep",
			map[string]any{"pattern": "KEY", "glob": "!**/.dev.vars, !*.md"},
		},
		{
			"Grep negated brace group",
			"Grep",
			map[string]any{"pattern": "KEY", "glob": "!{*.md,.dev.vars}"},
		},
	}
	for _, tt := range allow {
		assertAllow(t, tt.name, f.decide(tt.tool, tt.input))
	}
	for name, input := range map[string]map[string]any{
		"path beside a negated glob":      {"pattern": "KEY", "path": ".dev.vars", "glob": "!*.md"},
		"glob after a negated one, comma": {"pattern": "KEY", "glob": "!*.md,.dev.vars"},
		"glob after a negated one, space": {"pattern": "KEY", "glob": "!*.md **/.dev.vars*"},
	} {
		assertDeny(t, name, f.decide("Grep", input), RuleSecrets)
	}

	r := f.decide("Read", map[string]any{"file_path": ".DEV.VARS"})
	if runtime.GOOS == "linux" {
		assertAllow(t, ".DEV.VARS on Linux", r)
	} else {
		assertDeny(t, ".DEV.VARS on a case-insensitive OS", r, RuleSecrets)
	}
}

func TestSplitGlobs(t *testing.T) {
	for field, want := range map[string][]string{
		"*.go":               {"*.go"},
		"!*.md,*.go":         {"!*.md", "*.go"},
		"*.ts  *.go":         {"*.ts", "", "*.go"},
		"*.{ts,tsx},!x/**":   {"*.{ts,tsx}", "!x/**"},
		"!{a,b} c":           {"!{a,b}", "c"},
		"a{b,{c,d}}e,f":      {"a{b,{c,d}}e", "f"},
		"unbalanced{a,b c,d": {"unbalanced{a,b c,d"},
	} {
		if got := splitGlobs(field); !slices.Equal(got, want) {
			t.Errorf("splitGlobs(%q) = %q, want %q", field, got, want)
		}
	}
}

func TestSecretsBash(t *testing.T) {
	f := newFixture(t)
	deny := []string{
		"cat .dev.vars",
		"cat < .dev.vars",
		"bash -c 'cat .dev.vars'",
		`printf %s "$(cat .dev.vars)"`,
		"ls .dev.vars",
		`echo "never read .dev.vars"`,
		`K="$(cat .dev.vars | tee x)" cmd`,
		`K="$(cat .dev.vars)" cmd < .dev.vars`,
		"cat .dev.vars\nif (",
		`echo -n "$(cat .dev.vars)"`,
		`cmd -- "$(cat .dev.vars)"`,
		"cat " + t3Secrets + "/token",
		`grep -r KEY "$HOME/` + t3Rel + `"`,
		"cat <<'EOF'\n.dev.vars\nEOF",
		"cut -d= -f1 app/.dev.vars",
		"bash <<'EOF'\ncat .dev.vars\nEOF",
		"bash <<EOF\nK=\"$(cat .dev.vars)\" cmd $X\nEOF",
		`printf '%s\n' "$(cat .dev.vars)"`,
		`echo "$(cat .dev.vars)" > out.txt`,
		`echo "$(cat .dev.vars)" 2>&1 | cat`,
		`cmd | echo "$(cat .dev.vars)"`,
		`cmd <<< "pw: $(cat .dev.vars)"`,
		`: <<< ""; cat .dev.vars`,
	}
	for _, cmd := range deny {
		assertDeny(t, cmd, f.bash(cmd), RuleSecrets)
	}
	allow := []string{
		`K="$(cat .dev.vars)" cmd`,
		`export K="$(< .dev.vars)"`,
		`cmd --token "$(cat .dev.vars)"`,
		`cmd --token="$(cat .dev.vars)"`,
		`cmd -H "Bearer $(cat ` + t3Secrets + `/token)"`,
		"cat .dev.vars.example",
		"ls " + t3Rel + "-example",
		"bash <<'EOF'\ncd app\nexport K=\"$(cat " + t3Secrets + "/token)\"\nwrangler deploy\nEOF",
		`make switch SUDO="sudo -S -p ''" <<< "$(cat ` + t3Secrets + `/pw)" > /tmp/log 2>&1`,
		`printf '%s\n' "$(cat ` + t3Secrets + `/pw)" | sudo -S -p '' -v && { sudo true; }`,
		`echo "$(cat .dev.vars)" | wrangler secret put KEY`,
	}
	for _, cmd := range allow {
		assertAllow(t, cmd, f.bash(cmd))
	}
}

func TestGhPublish(t *testing.T) {
	f := newFixture(t)
	lp := localPath()
	f.write("sub/b.md", "see "+lp+"\n")
	f.write("f.txt", "value "+lp+"\n")
	f.write("clean.md", "clean\n")

	deny := []string{
		"gh pr create --title t --body '" + lp + "'",
		"cd sub && gh pr create --body-file b.md",
		"gh -R o/r issue comment 1 --body-file - <<'EOF'\nsee " + lp + "\nEOF",
		"gh issue comment 1 --body-file - < sub/b.md",
		"gh pr new -b '" + lp + "'",
		"gh release new v1 --notes '" + lp + "'",
		"gh api -X POST repos/o/r/issues -F body=@f.txt",
		"gh api -X POST repos/o/r/issues -f a=" + lp + " -f b=clean",
		"gh pr create --attach './s.png#" + lp + "'",
		"gh pr create --newflag x --body '" + lp + "'",
		"gh pr edit 1 -t '" + lp + "'",
		"gh pr review 1 -c -b '" + lp + "'",
		"gh release edit v1 -F sub/b.md",
		"env GH_REPO=o/r gh issue create -t x -b '" + lp + "'",
		"bash -c \"gh pr comment 1 -b '" + lp + "'\"",
		"{ gh issue comment 1 --body-file -; } <<'EOF'\nsee " + lp + "\nEOF",
		"if true; then gh issue comment 1 -F -; fi <<'EOF'\nsee " + lp + "\nEOF",
		"gh pr create -t t -b $'see\\n" + lp + "'",
		"gh pr create --dry-run=false -t t --body '" + lp + "'",
	}
	for _, cmd := range deny {
		assertDeny(t, cmd, f.bash(cmd), RuleGhPublish)
	}
	allow := []string{
		"gh release create v1 " + lp + "/dist/a.tgz --notes clean",
		"gh api repos/o/r/pulls --jq '.[] | \"" + lp + "\"'",
		"gh api repos/o/r/contents/x -H 'X-Note: " + lp + "'",
		"gh pr create --body-file b.md",
		"gh pr create --body-file " + lp,
		"gh pr create --attach '" + lp + "/s.png#alt'",
		"cd sub; gh pr create --body-file b.md",
		"cat sub/b.md | gh pr create --body-file -",
		"gh pr create -R " + lp + " --body-file clean.md",
		"echo '" + lp + "' && gh pr view 1",
		"gh pr create --fill",
		"gh pr create -h --body '" + lp + "'",
		"gh pr create --dry-run -t t --body '" + lp + "'",
	}
	for _, cmd := range allow {
		assertAllow(t, cmd, f.bash(cmd))
	}
}

// TestGhPublishOwnPath checks that only the argument naming a body file is exempt: the same text
// inside the file denies. The body file needs a real path that publictext reports, which on Unix a
// relative `C:/Users/<name>/…` (a directory named `C:` under cwd) gives.
func TestGhPublishOwnPath(t *testing.T) {
	if isWindows {
		t.Skip("a directory named C: cannot exist on Windows")
	}
	f := newFixture(t)
	own := strings.Join([]string{"C:", "Users", "dev", "b.md"}, "/")
	f.write(own, "clean\n")
	assertAllow(t, "body file argument is its own local path",
		f.bash("gh pr create --body-file "+own))
	f.write(own, "written to "+own+"\n")
	assertDeny(t, "body file content holds its own path",
		f.bash("gh pr create --body-file "+own), RuleGhPublish)
}

// TestGhPublishStdin checks where a stdin redirection is read: `( cd sub && … ) < F` reads F in
// the outer cwd, and `<&N`, `0<&N` and `<&-` replace an inherited stdin with an unknown one.
func TestGhPublishStdin(t *testing.T) {
	lp := localPath()
	for rule, files := range map[string]map[string]string{
		RuleGhPublish: {"body.md": "see " + lp + "\n", "docs/body.md": "clean\n"},
		"":            {"body.md": "clean\n", "docs/body.md": "see " + lp + "\n"},
	} {
		f := newFixture(t)
		for rel, content := range files {
			f.write(rel, content)
		}
		r := f.bash("(cd docs && gh issue comment 1 -F -) < body.md")
		if rule == "" {
			assertAllow(t, "clean outer body.md", r)
		} else {
			assertDeny(t, "local path in the outer body.md", r, rule)
		}
	}

	f := newFixture(t)
	f.write("body.md", "see "+lp+"\n")
	f.write("clean.md", "ok\n")
	for _, redir := range []string{"<&3", "0<&3", "<&-"} {
		cmd := "{ gh issue comment 1 -F - " + redir + "; } 3<clean.md <body.md"
		assertAllow(t, cmd, f.bash(cmd))
	}
	assertDeny(t, "a dup of another fd keeps stdin",
		f.bash("{ gh issue comment 1 -F - 2<&3; } 3<clean.md <body.md"), RuleGhPublish)
}

func TestRmRoot(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(filepath.Join(f.cwd, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.write("wt/.git", "gitdir: ../.git/worktrees/wt\n")

	deny := []string{
		"rm -rf ~",
		"rm -fr ~",
		"rm -r -- ~/",
		"rm -rf .git",
		"rm -R ./.git/",
		"rm --recursive --force .git",
		"rm .git -rf",
		"sudo true && rm -rf ~",
		"rm -rf -- " + strings.ReplaceAll(f.home, `\`, `/`),
	}
	if isWindows {
		deny = append(deny, `rm -Rf 'C:\'`, "rm -rf C:/", "rm -rf /", "rm -rf /c", "rm -rf /c/")
	} else {
		deny = append(deny, "rm -rf /", "rm -rf -- /", "cd / && rm -rf .", "rm -rf /usr/..")
	}
	for _, cmd := range deny {
		assertDeny(t, cmd, f.bash(cmd), RuleRmRoot)
	}
	allow := []string{
		"rm -rf '~'",
		`rm -rf "~"`,
		"rm -rf wt/.git",
		"cd /; rm -rf .",
		"cd / || rm -rf .",
		"rm -rf .git/hooks",
		"rm -f .git",
		"rm -rf ./build",
		"echo rm -rf /",
		"rm -rf /*",
		"rm -rf ~/src",
		"rm -rf $HOME",
		"rm -- -rf /",
		"command -V rm -rf ~",
	}
	for _, cmd := range allow {
		assertAllow(t, cmd, f.bash(cmd))
	}

	f.cwd = ""
	assertAllow(t, "relative operand under an unknown cwd", f.bash("rm -rf .git"))
}

func TestPencil(t *testing.T) {
	f := newFixture(t)
	execute := func(input string, replaces ...string) map[string]any {
		edits := make([]map[string]string, len(replaces))
		for i, r := range replaces {
			edits[i] = map[string]string{"find": "x", "replace": r}
		}
		m := map[string]any{"input": input}
		if len(edits) > 0 {
			m["edits"] = edits
		}
		return m
	}
	deny := []struct {
		name  string
		input map[string]any
	}{
		{"Generate call", execute(`const img = Generate({prompt: "icon"})`)},
		{"nested template call", execute("const s = `a ${`b ${Generate(x)}`}`")},
		{"regex literal then call", execute(`const r = /"/; Generate()`)},
		{"top-level await", execute(`await Generate({})`)},
		{"top-level return", execute(`return Generate({})`)},
		{"member call", execute(`x.Generate()`)},
		{"generateImage", execute(`generateImage({})`)},
		{"constant string index", execute(`api['generateImage']({})`)},
		{"constant template index", execute("api[`Generate`]({})")},
		{"optional call", execute(`api?.generate?.()`)},
		{"token fallback: real call", execute(`Generate(a) ) (`)},
		{"edit replace call", execute(`ok()`, `Generate(x)`)},
	}
	for _, tt := range deny {
		assertDeny(t, tt.name, f.decide("mcp__pencil__execute", tt.input), RulePencil)
	}
	allow := []struct {
		name  string
		input map[string]any
	}{
		{"string", execute(`Print("Generate()")`)},
		{"template text", execute("Print(`Generate()`)")},
		{"line comment", execute("// Generate()\nok()")},
		{"block comment", execute("/* Generate() */ ok()")},
		{"regex test", execute(`/Generate(x)/.test(s)`)},
		{"regenerate", execute(`regenerate()`)},
		{"property not called", execute(`const g = x.Generate`)},
		{"dynamic index", execute(`api[name]({})`)},
		{"index with another name", execute(`api["render"]({})`)},
		{"token fallback: regex after =", execute(`x = /Generate(a)/ ) (`)},
		{"edit replace string", execute(`ok()`, `"Generate(x)"`)},
		{"edit replace closing a string (documented gap)", execute(`ok()`, `"); Generate(x); ("`)},
	}
	for _, tt := range allow {
		assertAllow(t, tt.name, f.decide("mcp__pencil__execute", tt.input))
	}
	assertAllow(t, "Generate only in description", f.decide("mcp__pencil__execute",
		map[string]any{"input": "ok()", "description": "Generate(icon)"}))
	assertAllow(t, "another pencil tool",
		f.decide("mcp__pencil__get_style", map[string]any{"input": "Generate()"}))

	writeSafety(t, f.home, "deny_pencil_generate = false\n")
	assertAllow(t, "deny_pencil_generate = false",
		f.decide("mcp__pencil__execute", execute(`Generate({})`)))
}

// TestNoRawMerge checks that stack-guard leaves raw merges to the core hook: it holds no raw-merge
// rule, so `gh pr merge` alone passes it.
func TestNoRawMerge(t *testing.T) {
	f := newFixture(t)
	for _, cmd := range []string{
		"gh pr merge 1",
		"gh api -X PUT repos/o/r/pulls/1/merge",
		"if cd docs; then gh pr merge -R o/r 1 --squash; fi",
	} {
		assertAllow(t, cmd, f.bash(cmd))
	}
}
