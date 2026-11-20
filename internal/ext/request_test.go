package ext

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc"
)

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}

func TestBuildRequest(t *testing.T) {
	t.Parallel()
	r := gittest.New(t)
	r.Commit("base")
	r.Git("branch", "base")
	r.Commit("work")
	g := Git{Runner: proc.Exec{}, Dir: r.Dir, Env: r.Env}
	ctx := context.Background()

	// No upstream yet: outbound range stays empty.
	req, err := BuildRequest(ctx, g, StageOutbound, "", "P")
	if err != nil || !sameDir(t, req.Repo, r.Dir) || req.Range != "" || req.Prompt != "P" ||
		req.Stage != StageOutbound {
		t.Fatalf("no upstream: %+v, %v", req, err)
	}

	r.Config("branch.main.remote", ".")
	r.Config("branch.main.merge", "refs/heads/base")
	req, err = BuildRequest(ctx, g, StageOutbound, "", "")
	if err != nil || req.Range != "@{upstream}..HEAD" {
		t.Fatalf("upstream: %+v, %v", req, err)
	}
	for _, stage := range []string{StageEntry, StageCycle} {
		if req, err := BuildRequest(ctx, g, stage, "", ""); err != nil || req.Range != "" {
			t.Fatalf("%s with upstream: %+v, %v", stage, req, err)
		}
	}

	// An explicit range is kept verbatim when git resolves it.
	req, err = BuildRequest(ctx, g, StageEntry, "base..HEAD", "")
	if err != nil || req.Range != "base..HEAD" {
		t.Fatalf("explicit range: %+v, %v", req, err)
	}
	for _, bad := range []string{"nope..HEAD", "--all", "base..nope"} {
		if _, err := BuildRequest(ctx, g, StageOutbound, bad, ""); !errors.Is(err, ErrRange) {
			t.Errorf("range %q: err = %v, want ErrRange", bad, err)
		}
	}

	// Outside a repository: repo and range are empty.
	g.Dir = filepath.Join(r.Root, "home")
	req, err = BuildRequest(ctx, g, StageOutbound, "", "")
	if err != nil || req.Repo != "" || req.Range != "" {
		t.Fatalf("outside a repo: %+v, %v", req, err)
	}
}

func TestParseReplay(t *testing.T) {
	t.Parallel()
	r, err := ParseReplay([]byte(`{"request":{"stage":"entry","x":1},"expect":{"status":"block"}}`))
	if err != nil || r.Stage != "entry" || r.Expect != StatusBlock ||
		string(r.Request) != `{"stage":"entry","x":1}` {
		t.Fatalf("replay = %+v, %v", r, err)
	}
	r, err = ParseReplay([]byte(`{"request":{"stage":"cycle"}}` + "\n"))
	if err != nil || r.Expect != "" {
		t.Fatalf("no expect = %+v, %v", r, err)
	}
	for _, bad := range []string{
		``,
		`{}`,
		`{"request":[]}`,
		`{"request":{}}`,
		`{"request":{"stage":1}}`,
		`{"request":{"stage":"entry"}} x`,
		`{"request":{"stage":"entry"},"expect":{"status":"ok"}}`,
		`{"request":{"stage":"entry"},"expect":{}}`,
	} {
		if _, err := ParseReplay([]byte(bad)); !errors.Is(err, ErrReplay) {
			t.Errorf("ParseReplay(%q) = %v, want ErrReplay", bad, err)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("empty error for %q", bad)
		}
	}
}
