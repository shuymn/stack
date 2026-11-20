package github

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestMain(m *testing.M) {
	proctest.Main()
	os.Exit(gittest.Main(m))
}

const oid = "0123456789abcdef0123456789abcdef01234567"

var repoArgs = []string{
	"repo",
	"view",
	"--json",
	"nameWithOwner,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed",
}

func client(t *testing.T, name string, resp ...proctest.Response) Client {
	t.Helper()
	exe := proctest.Install(t, name, proctest.Scenario{Responses: resp})
	c := Client{
		Runner:  proc.Exec{WaitDelay: 100 * time.Millisecond},
		Gh:      exe,
		Git:     exe,
		Timeout: 30 * time.Second,
	}
	return c
}

// TestGhFailures covers every way a gh read fails; each must be an ErrGh error, never a value.
func TestGhFailures(t *testing.T) {
	ok := `{"nameWithOwner":"o/r"}`
	tests := []struct {
		name string
		resp proctest.Response
		want string
	}{
		{
			"non-zero exit",
			proctest.Response{Stdout: ok, Stderr: "HTTP 401\nbad creds\n", Exit: 4},
			"exit 4: HTTP 401 bad creds",
		},
		{"timeout", proctest.Response{Stdout: ok, Sleep: 10 * time.Second}, "timed out"},
		{"pipe held open", proctest.Response{Stdout: ok, Grandchild: 5 * time.Second}, "outlived"},
		{"truncated", proctest.Response{Stdout: ok + strings.Repeat(" ", 1<<20)}, "size cap"},
		{"not JSON", proctest.Response{Stdout: "o/r\n"}, "decode"},
		{"trailing JSON", proctest.Response{Stdout: ok + ok}, "trailing output"},
		{
			"bad nameWithOwner",
			proctest.Response{Stdout: `{"nameWithOwner":"o/r/x"}`},
			"nameWithOwner",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.resp.Args = repoArgs
			c := client(t, "gh", tt.resp)
			if tt.resp.Sleep > 0 {
				c.Timeout = 300 * time.Millisecond
			}
			_, err := c.RepoView(t.Context())
			if !errors.Is(err, ErrGh) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want ErrGh with %q", err, tt.want)
			}
		})
	}
	t.Run("cannot start", func(t *testing.T) {
		c := Client{Runner: proc.Exec{}, Gh: filepath.Join(t.TempDir(), "gh")}
		if _, err := c.RepoView(t.Context()); !errors.Is(err, ErrGh) {
			t.Errorf("err = %v, want ErrGh", err)
		}
	})
	t.Run("ok", func(t *testing.T) {
		c := client(t, "gh", proctest.Response{Args: repoArgs, Stdout: `{"nameWithOwner":"o/r",` +
			`"squashMergeAllowed":true}`})
		r, err := c.RepoView(t.Context())
		if err != nil || r != (Repo{Owner: "o", Name: "r", SquashMergeAllowed: true}) {
			t.Errorf("RepoView = %+v, %v", r, err)
		}
	})
}

func TestToplevel(t *testing.T) {
	args := []string{"rev-parse", "--show-toplevel"}
	t.Run("outside a repository", func(t *testing.T) {
		c := client(
			t,
			"git",
			proctest.Response{Args: args, Stderr: "fatal: not a git repository", Exit: 128},
		)
		if top, err := c.Toplevel(t.Context()); top != "" || err != nil {
			t.Errorf("Toplevel = %q, %v; want empty", top, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		c := client(t, "git", proctest.Response{Args: args, Sleep: 10 * time.Second})
		c.Timeout = 300 * time.Millisecond
		if _, err := c.Toplevel(t.Context()); !errors.Is(err, ErrGit) {
			t.Errorf("err = %v, want ErrGit", err)
		}
	})
	t.Run("pipe held open", func(t *testing.T) {
		c := client(
			t,
			"git",
			proctest.Response{Args: args, Stdout: "/x\n", Grandchild: 5 * time.Second},
		)
		if _, err := c.Toplevel(t.Context()); !errors.Is(err, ErrGit) {
			t.Errorf("err = %v, want ErrGit", err)
		}
	})
	t.Run("cannot start", func(t *testing.T) {
		c := Client{Runner: proc.Exec{}, Git: filepath.Join(t.TempDir(), "git")}
		if _, err := c.Toplevel(t.Context()); !errors.Is(err, ErrGit) {
			t.Errorf("err = %v, want ErrGit", err)
		}
	})
	t.Run("real repository", func(t *testing.T) {
		r := gittest.New(t)
		c := Client{Runner: proc.Exec{}, Dir: r.Dir, Env: r.Env}
		top, err := c.Toplevel(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.EvalSymlinks(r.Dir)
		got, _ := filepath.EvalSymlinks(filepath.FromSlash(top))
		if got != want {
			t.Errorf("Toplevel = %q, want %q", top, r.Dir)
		}
	})
}

func TestMergeResult(t *testing.T) {
	argv := MergeArgv(7, "rebase", oid, MergeArgs{})
	t.Run("forwards output and reports the exit", func(t *testing.T) {
		c := client(
			t,
			"gh",
			proctest.Response{Args: argv, Stdout: "out\n", Stderr: "err\n", Exit: 2},
		)
		var log bytes.Buffer
		res := c.Merge(t.Context(), 7, "rebase", oid, MergeArgs{}, &log)
		if res.OK() || res.ExitCode != 2 || res.String() != "exited 2" {
			t.Errorf("result = %+v", res)
		}
		if !strings.Contains(log.String(), "out\n") || !strings.Contains(log.String(), "err\n") {
			t.Errorf("log = %q", log.String())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		c := client(t, "gh", proctest.Response{Args: argv, Sleep: 10 * time.Second})
		c.MergeTimeout = 200 * time.Millisecond
		res := c.Merge(t.Context(), 7, "rebase", oid, MergeArgs{}, &bytes.Buffer{})
		if res.OK() || !res.TimedOut || res.String() != "timed out" {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("cannot start", func(t *testing.T) {
		c := Client{Runner: proc.Exec{}, Gh: filepath.Join(t.TempDir(), "gh")}
		res := c.Merge(t.Context(), 7, "rebase", oid, MergeArgs{}, &bytes.Buffer{})
		if res.OK() || res.Err == nil {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestCommitChecksPaging(t *testing.T) {
	repo := Repo{Owner: "o", Name: "r"}
	page := func(total int, next bool, cursor, node string) string {
		n := "false"
		if next {
			n = "true"
		}
		return `{"data":{"repository":{"object":{"oid":"` + oid + `","statusCheckRollup":{"contexts":{` +
			`"totalCount":` + strconv.Itoa(
			total,
		) + `,"pageInfo":{"hasNextPage":` + n +
			`,"endCursor":"` + cursor + `"},"nodes":[` + node + `]}}}}}}`
	}
	run := `{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"SUCCESS"}`
	tests := []struct {
		name  string
		pages []proctest.Response
		want  string
	}{
		{"cursor does not advance", []proctest.Response{
			{Args: ChecksArgs(repo, oid, ""), Stdout: page(2, true, "c1", run)},
			{Args: ChecksArgs(repo, oid, "c1"), Stdout: page(2, true, "c1", run)},
		}, "did not advance"},
		{"total changes", []proctest.Response{
			{Args: ChecksArgs(repo, oid, ""), Stdout: page(2, true, "c1", run)},
			{Args: ChecksArgs(repo, oid, "c1"), Stdout: page(3, false, "c2", run)},
		}, "total changed"},
		{"unknown node", []proctest.Response{
			{Args: ChecksArgs(repo, oid, ""), Stdout: page(1, false, "", `{"__typename":"Other"}`)},
		}, "unknown node"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := client(t, "gh", tt.pages...)
			if _, err := c.CommitChecks(t.Context(), repo, oid); !errors.Is(err, ErrGh) ||
				!strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
