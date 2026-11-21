package improve

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shuymn/stack/internal/proc/proctest"
)

// TestPublishSuccess runs a whole attempt to a PR: the pushed OID is the pinned one, gh pr
// create gets the checked title and body, and the worktree is gone.
func TestPublishSuccess(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	// An annotated tag reachable from the OID, with followTags on, reaches the remote unless the
	// push turns it off.
	f.repo.Git("tag", "-a", "v1", "-m", "v1")
	f.repo.Config("push.followTags", "true")
	resp := f.run()
	st := f.wantOutcome(resp, StatusPass, "pr https://github.com/o/r/pull/1")
	if st.PR != "https://github.com/o/r/pull/1" || st.OID == "" || st.Branch != branch {
		t.Fatalf("stamp = %+v", st)
	}
	if !strings.Contains(f.remote(), st.OID+"\trefs/heads/"+branch) {
		t.Fatalf("remote = %s, want %s at %s", f.remote(), branch, st.OID)
	}
	if strings.Contains(f.remote(), "refs/tags/") {
		t.Fatalf("a tag reached the remote: %s", f.remote())
	}
	wantBody := "Tighten a rule.\n\nEvidence:\n- https://github.com/o/r/pull/7\n"
	if st.Body != wantBody || st.Title != "docs: tighten a rule" {
		t.Fatalf("title %q body %q", st.Title, st.Body)
	}
	if body := f.createBody(); body != wantBody {
		t.Fatalf("gh pr create body = %q", body)
	}
	var create []string
	for _, c := range f.calls("gh") {
		if slices.Equal(c.Args[:2], []string{"pr", "create"}) {
			create = c.Args
		}
	}
	wantPrefix := []string{
		"pr", "create", "--repo", slug, "--base", "main", "--head", branch,
		"--title", "docs: tighten a rule", "--body-file", "-",
	}
	if !slices.Equal(create, wantPrefix) {
		t.Fatalf("gh pr create args = %q", create)
	}
	if n := f.ghCalls(argsReadPRs...); n != 1 {
		t.Fatalf("PR reads = %d, want 1", n)
	}
	if _, err := os.Stat(f.worktreeDir()); !os.IsNotExist(err) {
		t.Fatalf("worktree left: %v", err)
	}
	if strings.Contains(f.repo.Git("worktree", "list", "--porcelain"), "improve/work") {
		t.Fatalf("worktree metadata left: %s", f.repo.Git("worktree", "list"))
	}
	if st.CostUSD != 1.5 || !strings.Contains(resp.Context, "$1.50") ||
		!strings.Contains(resp.Context, "PR: https://github.com/o/r/pull/1") {
		t.Fatalf("context %q cost %v", resp.Context, st.CostUSD)
	}
	if len(resp.Findings) != 0 {
		t.Fatalf("findings = %+v", resp.Findings)
	}
}

// TestPublish covers step 12.8-12.10 (I11): a new branch only, one create with the checked bytes,
// a PR recorded only from a matching read, and read-only reporting of an ambiguous result.
func TestPublish(t *testing.T) {
	t.Parallel()
	fork := "[" + prJSON(2, "fork/r", "main") + "]"
	otherBase := "[" + prJSON(2, slug, "dev") + "]"
	two := "[" + prJSON(1, slug, "main") + "," +
		prJSON(3, slug, "main") + "]"
	mixed := "[" + prJSON(2, "fork/r", "main") + "," +
		prJSON(1, slug, "main") + "]"
	for _, tc := range []struct {
		name    string
		setup   func(f *fx)
		read    string
		readErr bool
		create  int
		status  string
		outcome string
		reads   int
		creates int
		reason  string
	}{
		{
			name: "branch exists at an ancestor",
			setup: func(f *fx) {
				f.repo.Git("push", "-q", f.bare, "main:refs/heads/"+branch)
			},
			status: StatusWarn, outcome: "failed:push", reason: "refs/heads/" + branch,
		},
		{
			name: "branch already at the oid",
			setup: func(f *fx) {
				f.opts.hook = func(point string) {
					if point == "codex" {
						f.repo.Git("-C", f.worktreeDir(), "push", "-q", f.bare,
							"HEAD:refs/heads/"+branch)
					}
				}
			},
			status: StatusWarn, outcome: "failed:push", reason: "[up to date]",
		},
		{
			name: "ambiguous push",
			setup: func(f *fx) {
				hooks := f.repo.Root + "/hooks"
				writeHook(t, hooks, "pre-push", "exit 1")
				f.repo.Config("core.hooksPath", hooks)
			},
			status: StatusWarn, outcome: outPublishUnknown, reads: 1, reason: "branch absent",
		},
		{
			name: "fork PR at the oid", read: fork, status: StatusWarn, outcome: outPublishUnknown,
			reads: 1, creates: 1, reason: "fork/r",
		},
		{
			name: "another base", read: otherBase, status: StatusWarn, outcome: outPublishUnknown,
			reads: 1, creates: 1, reason: "base dev",
		},
		{
			name: "create fails, read matches", create: 1, status: StatusPass,
			outcome: "pr https://github.com/o/r/pull/1", reads: 1, creates: 1,
		},
		{
			name: "fork PR beside the match", read: mixed, status: StatusPass,
			outcome: "pr https://github.com/o/r/pull/1", reads: 1, creates: 1,
		},
		{
			name: "two matching PRs", read: two, status: StatusWarn, outcome: outPublishUnknown,
			reads: 1, creates: 1, reason: "2 PR(s)",
		},
		{
			name: "branch only", read: "[]", create: 1, status: StatusWarn, outcome: outPushedNoPR,
			reads: 1, creates: 1, reason: "gh pr create --repo o/r",
		},
		{
			name: "read fails", readErr: true, status: StatusWarn, outcome: outPublishUnknown,
			reads: 1, creates: 1, reason: "PR read failed",
		},
		{
			name: "match without a URL", read: strings.Replace("["+prJSON(1, slug, "main")+"]",
				"https://github.com/o/r/pull/1", "", 1),
			status: StatusWarn, outcome: outPublishUnknown, reads: 1, creates: 1, reason: "1 PR(s)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			spec := f.ghSpec()
			if tc.read != "" {
				spec = f.ghSpecWith(tc.read)
			}
			readResp := proctest.Response{Args: argsReadPRs}
			if tc.readErr {
				spec.Print = nil
				readResp.Exit = 1
			}
			f.fake("gh", spec,
				proctest.Response{Args: argsRepoView, Stdout: repoView},
				proctest.Response{Args: argsOpenPRs, Stdout: "[[]]"},
				readResp,
				proctest.Response{Stdout: "https://github.com/o/r/pull/1\n", Exit: tc.create},
			)
			if tc.setup != nil {
				tc.setup(f)
			}
			resp := f.run()
			st := f.wantOutcome(resp, tc.status, tc.outcome)
			if !strings.Contains(st.Reason, tc.reason) {
				t.Fatalf("reason %q, want %q", st.Reason, tc.reason)
			}
			if n := f.ghCalls(argsReadPRs...); n != tc.reads {
				t.Fatalf("PR reads = %d, want %d", n, tc.reads)
			}
			if n := f.ghCalls("pr", "create"); n != tc.creates {
				t.Fatalf("gh pr create = %d, want %d", n, tc.creates)
			}
			if st.OID == "" || st.Branch != branch || st.Title == "" || st.Body == "" {
				t.Fatalf("candidate not recorded: %+v", st)
			}
		})
	}
}

// I10, I11: a pre-push hook that rewrites the body file after the checks does not change what
// gh pr create gets: the checked bytes go on its stdin.
func TestPublishBodyEdited(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	hooks := filepath.Join(f.repo.Root, "hooks")
	writeHook(t, hooks, "pre-push", `for b in "$HOME"/.local/state/stack/improve/.run-*/body; do
printf 'leak %s' '`+token+`' >> "$b"
done`)
	f.repo.Config("core.hooksPath", hooks)
	st := f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
	if body := f.createBody(); body != st.Body || strings.Contains(body, "leak") {
		t.Fatalf("gh pr create body = %q, stamp body %q", body, st.Body)
	}
}

// ownerState is the P8 snapshot of the owner's checkout.
func (f *fx) ownerState() string {
	f.t.Helper()
	return strings.Join([]string{
		f.repo.Git("status", "--porcelain=v2", "-z", "--untracked-files=all"),
		f.repo.Git("symbolic-ref", "-q", "HEAD"),
		f.repo.Git("rev-parse", "HEAD"),
		f.repo.Git("ls-files", "-s"),
		f.repo.Git("for-each-ref", "--exclude=refs/remotes/origin/main",
			"--exclude=refs/remotes/origin/"+branch),
		f.repo.Git("stash", "list"),
	}, "\n--\n")
}

// P8: the owner's checkout keeps its refs, tags, index and files, whatever its fetch
// configuration and hooks do.
func TestOwnerCheckout(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.repo.Git("config", "--add", "remote.origin.fetch",
		"+refs/heads/main:refs/heads/review-local-extra")
	f.repo.Config("fetch.prune", "true")
	f.repo.Config("fetch.pruneTags", "true")
	f.repo.Tag("local-only")
	hooks := filepath.Join(f.repo.Root, "hooks")
	writeHook(t, hooks, "pre-push", "printf ran > .improve-hook-marker")
	f.repo.Config("core.hooksPath", hooks)
	before := f.ownerState()
	f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
	if after := f.ownerState(); after != before {
		t.Fatalf("owner checkout changed:\nbefore %s\nafter  %s", before, after)
	}
	if strings.Contains(f.repo.Git("worktree", "list", "--porcelain"), "improve/work") {
		t.Fatal("worktree metadata left")
	}
}

// Protocol §3: a huge model summary stays whole in the stamp and is cut in the response.
func TestResponseSize(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	huge := strings.Repeat("x", 2<<20)
	out := claudeOut(with(change("e1"), "summary", huge), 1.0, zeroUsage(1))
	f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "skills/a.md", Content: "alpha two\n"}}},
		proctest.Response{Stdout: out})
	resp := f.run()
	st := f.wantOutcome(resp, StatusPass, "pr https://github.com/o/r/pull/1")
	if st.Summary != huge {
		t.Fatalf("stamp summary has %d bytes", len(st.Summary))
	}
	if n := len(mustJSON(t, resp)); n > 64<<10 {
		t.Fatalf("response has %d bytes", n)
	}
	if !strings.Contains(resp.Context, "PR: https://github.com/o/r/pull/1") {
		t.Fatalf("context lost the PR: %.200s", resp.Context)
	}
}
