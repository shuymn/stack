package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/github"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

const (
	prHead     = "0123456789abcdef0123456789abcdef01234567"
	prMergeOID = "fedcba9876543210fedcba9876543210fedcba98"
)

// fakeGh puts a fake gh that answers a passing merge of PR 7 at prHead first on PATH, inside a
// fixture repository that becomes the working directory. viewHead is the head the PR reports.
// Not parallel: it changes PATH and the working directory.
func fakeGh(t *testing.T, viewHead string) string {
	t.Helper()
	r := gittest.New(t)
	r.SetProcessEnv(t)
	t.Chdir(r.Dir)
	repo := github.Repo{Owner: "octo", Name: "demo"}
	resp := func(args []string, stdout string) proctest.Response {
		return proctest.Response{Args: args, Stdout: stdout}
	}
	gh := proctest.Install(t, "gh", proctest.Scenario{Responses: []proctest.Response{
		resp(
			[]string{"pr", "view", "7", "--json", "headRefOid,baseRefName,state,isDraft,mergeable"},
			`{"headRefOid":"`+viewHead+`","state":"OPEN","isDraft":false,"mergeable":"MERGEABLE"}`,
		),
		resp([]string{
			"repo", "view", "--json",
			"nameWithOwner,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed",
		},
			`{"nameWithOwner":"octo/demo","mergeCommitAllowed":true}`),
		resp([]string{"pr", "view", "7", "--json", "commits"}, `{"commits":[]}`),
		resp(github.ChecksArgs(repo, prHead, ""), `{"data":{"repository":{"object":{"oid":"`+
			prHead+`","statusCheckRollup":{"contexts":{"totalCount":1,"pageInfo":{"hasNextPage":`+
			`false},"nodes":[{"__typename":"CheckRun","name":"test","status":"COMPLETED",`+
			`"conclusion":"SUCCESS"}]}}}}}}`),
		resp(github.MergeQueueArgs(repo, 7),
			`{"data":{"repository":{"pullRequest":{"isMergeQueueEnabled":false}}}}`),
		resp([]string{"pr", "merge", "7", "--merge", "--match-head-commit", prHead}, ""),
		resp([]string{"pr", "view", "7", "--json", "state,headRefOid,mergeCommit"},
			`{"state":"MERGED","headRefOid":"`+prHead+`","mergeCommit":{"oid":"`+prMergeOID+`"}}`),
	}})
	t.Setenv("PATH", filepath.Dir(gh)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return gh
}

func ghMerged(t *testing.T, gh string) bool {
	t.Helper()
	return slices.ContainsFunc(proctest.Calls(t, gh), func(c proctest.Call) bool {
		return len(c.Args) > 1 && c.Args[0] == "pr" && c.Args[1] == "merge"
	})
}

func TestPrMerge(t *testing.T) {
	t.Run("merges", func(t *testing.T) {
		gh := fakeGh(t, prHead)
		r := run(t, nil, "pr", "merge", "7", prHead[:9], "--merge")
		if r.code != exitOK || r.stdout != "MERGED "+prHead+" "+prMergeOID+"\n" ||
			!ghMerged(t, gh) {
			t.Errorf("pr merge = %+v", r)
		}
	})
	t.Run("refuses a moved head", func(t *testing.T) {
		moved := strings.Repeat("9", 40)
		gh := fakeGh(t, moved)
		r := run(t, nil, "pr", "merge", "7", prHead[:9])
		want := "stack pr merge: refused: head is " + moved + ", not " + prHead[:9]
		if r.code != exitRefused || r.stdout != "" || !strings.HasPrefix(r.stderr, want) ||
			strings.Count(r.stderr, "\n") != 1 || ghMerged(t, gh) {
			t.Errorf("pr merge = %+v, want exit 1 with %q", r, want)
		}
	})
	t.Run("usage errors exit 2 before gh", func(t *testing.T) {
		gh := fakeGh(t, prHead)
		home := t.TempDir()
		writeFile(t, filepath.Join(home, ".config", "stack", "private-terms"), []byte("Zebra\n"))
		for _, tt := range []struct {
			env  map[string]string
			args []string
			want string
		}{
			{nil, []string{"12", "abc"}, "7-40 lower-case hex"},
			{nil, []string{"12", "abc", "--auto"}, "7-40 lower-case hex"},
			{nil, []string{"7", prHead, "--merge", "--rebase"}, "pass one merge method"},
			{map[string]string{"GH_REPO": "o/r"}, []string{"7", prHead}, "GH_REPO"},
			{nil, []string{"7", prHead, "--body", "see /" + "home/bob/x"}, "--body: local path"},
			{nil, []string{"7", prHead, "--subject", "zebra"}, "--subject: private term 'Zebra'"},
		} {
			r := runIn(t, home, tt.env, append([]string{"pr", "merge"}, tt.args...)...)
			if r.code != exitUsage || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("%q = %+v, want exit 2 with %q", tt.args, r, tt.want)
			}
		}
		if calls := proctest.Calls(t, gh); len(calls) != 0 {
			t.Errorf("gh calls = %+v, want none", calls)
		}
	})
}

// TestPrMergeMergedStage covers the wiring of C5 and C7 through the CLI: [stages].merged from
// config.toml, extensions found on PATH, the session's kits, and today's output when nothing is
// runnable, with the `.kits` lock held so that a read would show. PATH holds only the test's fakes,
// so no host `stack-*` binary can start. Not parallel: it changes PATH.
func TestPrMergeMergedStage(t *testing.T) {
	longStageDeadlines(t)
	gh := fakeGh(t, prHead)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// pr merge's one git call finds the top level of the fixture repository it runs in.
	proctest.InstallAt(t, bin, "git", proctest.Scenario{Responses: []proctest.Response{{
		Args: []string{"rev-parse", "--show-toplevel"}, Stdout: filepath.ToSlash(cwd) + "\n",
	}}})
	t.Setenv("PATH", filepath.Dir(gh)+string(os.PathListSeparator)+bin)
	demo := fakeExt(t, bin, "demo", `{"status":"warn","context":"  DUE\n"}`)
	home := t.TempDir()
	withID := map[string]string{"STACK_SESSION_ID": "s1"}
	config := func(merged string) {
		writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
			[]byte("[stages]\nentry = []\nmerged = ["+merged+"]\n"))
	}
	config(`"demo"`)
	if res := runIn(t, home, withID, "kit", "open", "a"); res.code != exitOK {
		t.Fatalf("kit open = %+v", res)
	}
	merge := func() result {
		t.Helper()
		return runIn(t, home, withID, "pr", "merge", "7", prHead[:9], "--merge")
	}
	line := "MERGED " + prHead + " " + prMergeOID + "\n"

	res := merge()
	if res.code != exitOK || res.stdout != line+"[extension demo: warn: warn]\nDUE\n" ||
		res.stderr != "[demo] note\n" {
		t.Fatalf("pr merge = %+v", res)
	}
	calls := proctest.Calls(t, demo)
	var req map[string]json.RawMessage
	if len(calls) != 1 || json.Unmarshal([]byte(calls[0].Stdin), &req) != nil {
		t.Fatalf("demo calls = %+v", calls)
	}
	dir := filepath.Join(home, ".local", "state", "stack", "kits", "a")
	if string(req["stage"]) != `"merged"` ||
		string(req["kits"]) != `[{"slug":"a","dir":`+strconv.Quote(dir)+`}]` ||
		string(req["merge"]) != `{"pr":7,"head":"`+prHead+`","oid":"`+prMergeOID+`"}` {
		t.Fatalf("request = %s", calls[0].Stdin)
	}

	holdKitsLock(t, home, "s1")
	config(`"absent"`)
	if res := merge(); res.code != exitOK || res.stdout != line || res.stderr != "" {
		t.Fatalf("pr merge without an installed extension = %+v, want only %q", res, line)
	}
	odd := filepath.Join(bin, "stack-odd")
	if runtime.GOOS == "windows" {
		odd += ".cmd"
	}
	writeFile(t, odd, []byte("not a program\n"))
	config(`"odd", "absent"`)
	res = merge()
	if res.code != exitOK || !strings.HasPrefix(res.stdout, line+"[extension odd: warn: ") ||
		strings.Count(res.stdout, "\n") != 2 || res.stderr != "" {
		t.Fatalf("pr merge with an extension not runnable = %+v", res)
	}

	n := len(proctest.Calls(t, gh))
	config(`"../x"`)
	if res := merge(); res.code != exitUsage || !strings.Contains(res.stderr, "[stages].merged") ||
		len(proctest.Calls(t, gh)) != n {
		t.Fatalf("pr merge with an invalid merged name = %+v", res)
	}
}
