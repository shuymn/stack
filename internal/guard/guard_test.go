package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/hookio"
)

const isWindows = runtime.GOOS == "windows"

// localPath returns a home path that publictext reports, built at run time so this file holds no
// literal one.
func localPath() string {
	return strings.Join([]string{"", "Users", "dev", "notes.md"}, "/")
}

// fixture is a home and a cwd under t.TempDir with the default config.
type fixture struct {
	t         *testing.T
	home, cwd string
	opts      Options
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	cwd := t.TempDir()
	return &fixture{
		t:    t,
		home: home,
		cwd:  cwd,
		opts: Options{Config: config.Defaults(), Home: home},
	}
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

func (f *fixture) decide(tool string, input any) (Decision, bool) {
	f.t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		f.t.Fatal(err)
	}
	return Decide(hookio.Input{
		CWD:           f.cwd,
		HookEventName: "PreToolUse",
		ToolName:      tool,
		ToolInput:     raw,
	}, f.opts)
}

func (f *fixture) bash(cmd string) (Decision, bool) {
	f.t.Helper()
	return f.decide("Bash", map[string]any{"command": cmd})
}

func wantDeny(t *testing.T, name string, d Decision, ok bool, rule string) {
	t.Helper()
	if !ok || d.Rule != rule || !strings.Contains(d.Reason, "("+rule+")") {
		t.Errorf("%s: got deny=%v %+v, want a %s deny", name, ok, d, rule)
	}
}

func wantAllow(t *testing.T, name string, d Decision, ok bool) {
	t.Helper()
	if ok {
		t.Errorf("%s: got %+v, want allow", name, d)
	}
}

func TestSecretsFileTools(t *testing.T) {
	f := newFixture(t)
	secretDir := filepath.Join(f.home, ".t3", "userdata", "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(secretDir, "token")
	if err := os.WriteFile(token, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	example := filepath.Join(f.home, ".t3", "userdata", "secrets-example", "token")

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
		{"Read ~ form", "Read", map[string]any{"file_path": "~/.t3/userdata/secrets/token"}},
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
			map[string]any{"pattern": "userdata/secrets/*", "path": filepath.Join(f.home, ".t3")},
		},
		{
			"Glob.pattern absolute",
			"Glob",
			map[string]any{"pattern": filepath.ToSlash(secretDir) + "/*"},
		},
	}
	for _, tt := range deny {
		d, ok := f.decide(tt.tool, tt.input)
		wantDeny(t, tt.name, d, ok, RuleSecrets)
	}

	if err := os.Symlink(token, filepath.Join(f.cwd, "link")); err == nil {
		d, ok := f.decide("Read", map[string]any{"file_path": "link"})
		wantDeny(t, "Read symlink into secrets", d, ok, RuleSecrets)
		d, ok = f.decide("Grep", map[string]any{"pattern": "x", "path": "link"})
		wantAllow(t, "Grep does not follow symlinks", d, ok)
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
	}
	for _, tt := range allow {
		d, ok := f.decide(tt.tool, tt.input)
		wantAllow(t, tt.name, d, ok)
	}

	d, ok := f.decide("Read", map[string]any{"file_path": ".DEV.VARS"})
	if runtime.GOOS == "linux" {
		wantAllow(t, ".DEV.VARS on Linux", d, ok)
	} else {
		wantDeny(t, ".DEV.VARS on a case-insensitive OS", d, ok, RuleSecrets)
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
		"cat ~/.t3/userdata/secrets/token",
		`grep -r KEY "$HOME/.t3/userdata/secrets"`,
		"cat <<'EOF'\n.dev.vars\nEOF",
		"cut -d= -f1 app/.dev.vars",
		"bash <<'EOF'\ncat .dev.vars\nEOF",
		"bash <<EOF\nK=\"$(cat .dev.vars)\" cmd $X\nEOF",
		`make switch <<< "$(cat .dev.vars)"`,
	}
	for _, cmd := range deny {
		d, ok := f.bash(cmd)
		wantDeny(t, cmd, d, ok, RuleSecrets)
	}
	allow := []string{
		`K="$(cat .dev.vars)" cmd`,
		`export K="$(< .dev.vars)"`,
		`cmd --token "$(cat .dev.vars)"`,
		`cmd --token="$(cat .dev.vars)"`,
		`cmd -H "Bearer $(cat ~/.t3/userdata/secrets/token)"`,
		"cat .dev.vars.example",
		"ls .t3/userdata/secrets-example",
		"bash <<'EOF'\ncd app\nexport K=\"$(cat ~/.t3/userdata/secrets/token)\"\nwrangler deploy\nEOF",
	}
	for _, cmd := range allow {
		d, ok := f.bash(cmd)
		wantAllow(t, cmd, d, ok)
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
	}
	for _, cmd := range deny {
		d, ok := f.bash(cmd)
		wantDeny(t, cmd, d, ok, RuleGhPublish)
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
	}
	for _, cmd := range allow {
		d, ok := f.bash(cmd)
		wantAllow(t, cmd, d, ok)
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
	d, ok := f.bash("gh pr create --body-file " + own)
	wantAllow(t, "body file argument is its own local path", d, ok)
	f.write(own, "written to "+own+"\n")
	d, ok = f.bash("gh pr create --body-file " + own)
	wantDeny(t, "body file content holds its own path", d, ok, RuleGhPublish)
}

func TestRawMerge(t *testing.T) {
	f := newFixture(t)
	mutation := `mutation { mergePullRequest(input: {pullRequestId: "x"}) { clientMutationId } }`
	f.write("q.graphql", mutation)
	f.write("in.json", `{"query": `+strconv.Quote(mutation)+`}`)
	f.write("read.graphql", `query { repository(owner: "o", name: "r") { id } }`)

	deny := []string{
		"gh pr merge 1",
		"gh -R o/r pr merge 1",
		"gh pr -R o/r merge 1",
		"gh pr merge -R o/r 1 --squash",
		"GH_REPO=o/r gh pr merge 1",
		"gh pr merge 1 --disable-auto=false",
		"gh pr merge 1 --subject --help",
		"gh pr merge 1 -- --help",
		"gh pr merge -h=false 1",
		"gh pr merge --help=false 1",
		"gh pr merge 1 --auto",
		"/usr/local/bin/gh pr merge 1",
		"mise exec -- gh pr merge 1",
		"echo $(gh pr merge 1)",
		"gh pr merge 1\nif (",
		"gh api -X PUT /repos/o/r/pulls/1/merge",
		"gh api -X PUT repos/o/r/pulls/1/merge",
		"gh api --method put repos/o/r/pulls/1/merge -f merge_method=squash",
		"gh api graphql -f query='" + mutation + "'",
		"gh api graphql -F query=@q.graphql",
		"gh api graphql --input in.json",
		`gh api graphql -f query='mutation { enablePullRequestAutoMerge(input: {}) { clientMutationId } }'`,
		`gh api graphql -f query='mutation { ...M } fragment M on Mutation { mergePullRequest(input: {}) { clientMutationId } }'`,
		`gh api graphql -f query='mutation { ... on Mutation { mergePullRequest(input: {}) { clientMutationId } } }'`,
		`gh api graphql -f query='mutation { m: mergePullRequest(input: {}) { clientMutationId } }'`,
	}
	for _, cmd := range deny {
		d, ok := f.bash(cmd)
		wantDeny(t, cmd, d, ok, RuleRawMerge)
	}
	allow := []string{
		"gh pr merge --help",
		"gh pr merge -h",
		"gh pr merge 1 --disable-auto",
		"gh pr merge 1 --disable-auto=true",
		"gh pr view 1",
		"gh api repos/o/r/pulls/1/merge",
		"gh api -X POST repos/o/r/pulls/1/merge",
		"gh api graphql -F query=@read.graphql",
		`gh api graphql -f query='mutation { addComment(input: {}) { clientMutationId } } fragment M on Mutation { mergePullRequest(input: {}) { clientMutationId } }'`,
		`gh api graphql -f query='query { search(query: "mergePullRequest") { issueCount } }'`,
		"gh api graphql -f query='query {\n# mergePullRequest\nviewer { login } }'",
		`gh api graphql -f query='mutation { mergePullRequest: addComment(input: {}) { clientMutationId } }'`,
		`gh api graphql -f query='mutation { mergePullRequest('`,
		`echo "gh pr merge 1"`,
		"cat <<'EOF'\ngh pr merge 1\nEOF",
		"stack pr merge 1 abc1234 --squash",
	}
	for _, cmd := range allow {
		d, ok := f.bash(cmd)
		wantAllow(t, cmd, d, ok)
	}
	d, ok := f.bash("gh pr merge 1")
	if ok && !strings.Contains(d.Reason, "stack pr merge <pr> <head-sha>") {
		t.Errorf("raw merge reason does not name stack pr merge: %q", d.Reason)
	}
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
		d, ok := f.bash(cmd)
		wantDeny(t, cmd, d, ok, RuleRmRoot)
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
	}
	for _, cmd := range allow {
		d, ok := f.bash(cmd)
		wantAllow(t, cmd, d, ok)
	}

	f.cwd = ""
	d, ok := f.bash("rm -rf .git")
	wantAllow(t, "relative operand under an unknown cwd", d, ok)
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
		{"optional call", execute(`api?.generate?.()`)},
		{"token fallback: real call", execute(`Generate(a) ) (`)},
		{"edit replace call", execute(`ok()`, `Generate(x)`)},
	}
	for _, tt := range deny {
		d, ok := f.decide("mcp__pencil__execute", tt.input)
		wantDeny(t, tt.name, d, ok, RulePencil)
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
		{"token fallback: regex after =", execute(`x = /Generate(a)/ ) (`)},
		{"edit replace string", execute(`ok()`, `"Generate(x)"`)},
		{"edit replace closing a string (documented gap)", execute(`ok()`, `"); Generate(x); ("`)},
	}
	for _, tt := range allow {
		d, ok := f.decide("mcp__pencil__execute", tt.input)
		wantAllow(t, tt.name, d, ok)
	}
	d, ok := f.decide("mcp__pencil__execute", map[string]any{
		"input": "ok()", "description": "Generate(icon)",
	})
	wantAllow(t, "Generate only in description", d, ok)
	d, ok = f.decide("mcp__pencil__get_style", map[string]any{"input": "Generate()"})
	wantAllow(t, "another pencil tool", d, ok)

	f.opts.Config.Safety.DenyPencilGenerate = false
	d, ok = f.decide("mcp__pencil__execute", execute(`Generate({})`))
	wantAllow(t, "deny_pencil_generate = false", d, ok)
}
