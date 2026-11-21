package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// hookRepoVars exports into the test process the variables a git hook would set for another
// fixture repository. Not parallel: it changes the process environment.
func hookRepoVars(t *testing.T) *gittest.Repo {
	t.Helper()
	other := gittest.New(t)
	other.Commit("other")
	gitDir := other.Git("rev-parse", "--absolute-git-dir")
	t.Setenv("GIT_DIR", gitDir)
	t.Setenv("GIT_WORK_TREE", other.Dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(gitDir, "index"))
	return other
}

// TestRunIgnoresHookRepoVars checks that a stage request is built from the repository around the
// working directory, not the one a git hook's GIT_DIR and GIT_WORK_TREE name.
func TestRunIgnoresHookRepoVars(t *testing.T) {
	_, r := extBin(t)
	top := r.Git("rev-parse", "--show-toplevel")
	other := hookRepoVars(t)
	res := run(t, nil, "run", "outbound", "--dry-run", "--ext", "demo")
	if res.code != exitOK || !strings.Contains(res.stdout, `"repo":"`+top+`"`) ||
		strings.Contains(res.stdout, other.Dir) {
		t.Fatalf("run = %+v, want repo %q", res, top)
	}
}

// TestPrMergeIgnoresHookRepoVars checks that gh, which pr merge runs on the repository around the
// working directory, never gets a git hook's GIT_DIR or GIT_WORK_TREE.
func TestPrMergeIgnoresHookRepoVars(t *testing.T) {
	fakeGh(t, prHead) // the fixture repository as the working directory
	logged := proctest.Install(t, "gh", proctest.Scenario{Responses: []proctest.Response{{
		LogEnv: []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"}, Exit: 1,
	}}})
	t.Setenv("PATH", filepath.Dir(logged)+string(filepath.ListSeparator)+os.Getenv("PATH"))
	hookRepoVars(t)
	res := run(t, nil, "pr", "merge", "7", prHead)
	calls := proctest.Calls(t, logged)
	if res.code != exitRefused || len(calls) == 0 {
		t.Fatalf("pr merge = %+v, calls %+v; want a refusal after gh ran", res, calls)
	}
	for _, c := range calls {
		if len(c.Env) != 0 {
			t.Errorf("gh %q got %v", c.Args, c.Env)
		}
	}
}
