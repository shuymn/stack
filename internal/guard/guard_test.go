package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/hookio"
)

// fixture is a cwd and a home under t.TempDir.
type fixture struct {
	t    *testing.T
	cwd  string
	opts Options
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, cwd: t.TempDir(), opts: Options{Home: t.TempDir()}}
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

func (f *fixture) bash(cmd string) (Decision, bool) {
	f.t.Helper()
	raw, err := json.Marshal(map[string]any{"command": cmd})
	if err != nil {
		f.t.Fatal(err)
	}
	return Decide(hookio.Input{
		CWD:           f.cwd,
		HookEventName: "PreToolUse",
		ToolName:      "Bash",
		ToolInput:     raw,
	}, f.opts)
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
		"ids=$(gh api graphql -f query='query{viewer{id}}'); for t in ${(f)ids}; do echo $t; done; " +
			"gh pr merge 877 --merge --auto 2>&1 | tail -2",
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
