package improve

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

// I10: the pinned OID is verified, checked and pushed, whatever HEAD does afterwards.
func TestCommitScope(t *testing.T) {
	t.Parallel()
	t.Run("hook stages an extra file", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		hooks := filepath.Join(f.repo.Root, "hooks")
		writeHook(t, hooks, "pre-commit", "echo more >> other/c.md && git add other/c.md")
		f.repo.Config("core.hooksPath", hooks)
		st := f.wantOutcome(f.run(), StatusWarn, "proposal:commit-scope")
		if !strings.Contains(st.Reason, "other/c.md") {
			t.Fatalf("reason %q", st.Reason)
		}
		if len(f.calls("stack")) != 0 || len(f.calls("codex")) != 0 {
			t.Fatal("checks ran")
		}
		f.noPublish()
	})
	t.Run("HEAD moved after the check", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		var moved string
		f.opts.hook = func(point string) {
			if point == "check" {
				wt := f.worktreeDir()
				f.repo.Git("-C", wt, "commit", "-q", "--allow-empty", "-m", "moved")
				moved = f.repo.Git("-C", wt, "rev-parse", "HEAD")
			}
		}
		st := f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
		if moved == "" || moved == st.OID {
			t.Fatalf("HEAD did not move: %q", moved)
		}
		if !strings.Contains(f.remote(), st.OID+"\trefs/heads/"+branch) {
			t.Fatalf("remote %s, want the pinned %s", f.remote(), st.OID)
		}
		calls := f.calls("stack")
		if len(calls) != 1 || calls[0].Args[2] != f.stamp().BaseOID+".."+st.OID {
			t.Fatalf("stack calls %+v", calls)
		}
	})
}

// I10 with the real stack check public.
func TestPublicCheck(t *testing.T) {
	t.Parallel()
	needBuilt(t)
	const term = "Zebrafinch"
	for _, tc := range []struct {
		name    string
		edit    string
		out     map[string]any
		terms   string
		outcome string
	}{
		{name: "clean", edit: "alpha two\n", outcome: "pr https://github.com/o/r/pull/1"},
		{name: "term in the diff", edit: "alpha " + term + "\n", outcome: "proposal:public-check"},
		{
			name: "term in the message", edit: "alpha two\n",
			out: with(change("e1"), "commit_message", "docs: "+term), outcome: "proposal:public-check",
		},
		{
			name: "term in the title", edit: "alpha two\n",
			out: with(change("e1"), "pr_title", "docs: "+term), outcome: "proposal:public-check",
		},
		{
			name: "term in the body", edit: "alpha two\n",
			out: with(change("e1"), "pr_body", "About "+term+"."), outcome: "proposal:public-check",
		},
		{name: "exit 2", edit: "alpha two\n", terms: "dir", outcome: "failed:check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			f.tools["stack"] = built.stack
			if tc.terms == "dir" {
				f.write(".config/stack/private-terms/x", "")
			} else {
				f.write(".config/stack/private-terms", term+"\n")
			}
			out := tc.out
			if out == nil {
				out = change("e1")
			}
			f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "skills/a.md", Content: tc.edit}}},
				proctest.Response{Stdout: claudeOut(out, 1.0, zeroUsage(1))})
			resp := f.run()
			wantStatus := StatusWarn
			if strings.HasPrefix(tc.outcome, "pr ") {
				wantStatus = StatusPass
			}
			st := f.wantOutcome(resp, wantStatus, tc.outcome)
			if tc.outcome == "proposal:public-check" {
				if !strings.Contains(st.CheckHits, term) || st.Patch == "" {
					t.Fatalf("stamp %+v", st)
				}
				if len(f.calls("codex")) != 0 {
					t.Fatal("codex ran")
				}
			}
			if !strings.HasPrefix(tc.outcome, "pr ") {
				f.noPublish()
			}
		})
	}
}

// I13.
func TestCredential(t *testing.T) {
	t.Parallel()
	const settingsToken = "settings-secret-98765"
	for _, tc := range []struct {
		name    string
		setup   func(f *fx)
		outcome string
		// committed: the hook added the value after the commit was made.
		committed bool
	}{
		{name: "model writes it into a file", outcome: "proposal:credential", setup: func(f *fx) {
			f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "skills/a.md", Content: token}}},
				proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))})
		}},
		{name: "model writes it into a new file", outcome: "proposal:credential", setup: func(f *fx) {
			f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "notes.txt", Content: settingsToken}}},
				proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))})
		}},
		{name: "model writes it into the title", outcome: "proposal:credential", setup: func(f *fx) {
			f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "skills/a.md", Content: "a2\n"}}},
				proctest.Response{Stdout: claudeOut(with(change("e1"), "pr_title", "t "+settingsToken),
					1.0, zeroUsage(1))})
		}},
		{
			name: "commit-msg hook appends it", outcome: "proposal:credential", committed: true,
			setup: func(f *fx) {
				hooks := filepath.Join(f.repo.Root, "hooks")
				writeHook(t, hooks, "commit-msg", "echo '"+token+"' >> \"$1\"")
				f.repo.Config("core.hooksPath", hooks)
			},
		},
		{
			name: "pre-commit hook writes it", outcome: "proposal:credential", committed: true,
			setup: func(f *fx) {
				hooks := filepath.Join(f.repo.Root, "hooks")
				writeHook(t, hooks, "pre-commit", "echo '"+settingsToken+"' >> skills/a.md && git add skills/a.md")
				f.repo.Config("core.hooksPath", hooks)
			},
		},
		{
			name: "external diff hides it", outcome: "proposal:credential", committed: true,
			setup: func(f *fx) {
				hooks := filepath.Join(f.repo.Root, "hooks")
				writeHook(t, hooks, "pre-commit", "echo '"+token+"' >> skills/a.md && git add skills/a.md")
				writeHook(t, hooks, "nodiff", "exit 0")
				f.repo.Config("core.hooksPath", hooks)
				f.repo.Config("diff.external", filepath.ToSlash(filepath.Join(hooks, "nodiff")))
			},
		},
		{
			name: "only on claude's stderr", outcome: "pr https://github.com/o/r/pull/1",
			setup: func(f *fx) {
				f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "skills/a.md", Content: "a2\n"}}},
					proctest.Response{
						Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1)),
						Stderr: "using token " + token + " and " + settingsToken + "\n",
					})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			f.secrets = append(f.secrets, settingsToken)
			f.write(".claude/settings.json", `{"env":{"ANTHROPIC_AUTH_TOKEN":"`+settingsToken+`"}}`)
			tc.setup(f)
			resp := f.run()
			status := StatusWarn
			if strings.HasPrefix(tc.outcome, "pr ") {
				status = StatusPass
			}
			st := f.wantOutcome(resp, status, tc.outcome)
			if strings.HasPrefix(tc.outcome, "pr ") {
				if !strings.Contains(f.stderr.String(), redactMark) {
					t.Fatalf("stderr not passed through redacted:\n%s", f.stderr)
				}
			} else {
				if len(f.calls("stack")) != 0 || len(f.calls("codex")) != 0 {
					t.Fatal("checks ran after a credential hit")
				}
				f.noPublish()
			}
			if tc.committed {
				if st.OID == "" {
					t.Fatal("no local commit recorded")
				}
				if refs := f.repo.Git("for-each-ref", "--contains", st.OID); refs != "" {
					t.Fatalf("refs point at the commit: %s", refs)
				}
			} else if tc.outcome == "proposal:credential" && st.OID != "" {
				t.Fatalf("committed: %q", st.OID)
			}
			log := f.repo.Git("--git-dir", f.bare, "log", "--all", "-p")
			for _, secret := range f.secrets {
				if strings.Contains(log, secret) {
					t.Fatal("the remote holds a credential")
				}
			}
		})
	}
}

// fakeClock is a monotonic clock that stands still until the test moves it, so the time left at
// each step does not depend on how fast the machine runs the steps.
type fakeClock struct {
	ahead atomic.Int64
}

var clockBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (c *fakeClock) now() time.Time {
	return clockBase.Add(time.Duration(c.ahead.Load()))
}

func (c *fakeClock) advance(d time.Duration) { c.ahead.Add(int64(d)) }

// I14.
func TestDeadline(t *testing.T) {
	t.Parallel()
	t.Run("before the stamp", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.opts.limits.budget = 10 * time.Minute
		resp := f.run()
		if resp.Status != StatusPass || !strings.Contains(resp.Context, "not enough time") {
			t.Fatalf("resp %+v", resp)
		}
		f.noStamp()
		if len(f.calls("claude")) != 0 {
			t.Fatal("claude ran")
		}
	})
	for _, tc := range []struct {
		name, point string
		jump        time.Duration
		outcome     string
		removed     bool
		claude      int
		codex       int
	}{
		{"slow worktree", "worktree", 20 * time.Minute, "failed:deadline", true, 0, 0},
		{"before codex", "check", 25 * time.Minute, "failed:deadline", false, 1, 0},
		{"before push", "codex", 26*time.Minute + 30*time.Second, "failed:deadline", false, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			c := &fakeClock{}
			f.opts.clock = c.now
			var seen stamp
			f.opts.hook = func(point string) {
				switch point {
				case tc.point:
					c.advance(tc.jump)
				case "cleanup":
					seen = f.stamp()
				}
			}
			resp := f.run()
			if seen.Outcome != tc.outcome || resp.Status != StatusWarn {
				t.Fatalf("outcome %q (%s), status %s", seen.Outcome, seen.Reason, resp.Status)
			}
			if tc.removed {
				f.noStamp()
			} else {
				f.stamp()
			}
			if n := len(f.calls("claude")); n != tc.claude {
				t.Fatalf("claude calls %d", n)
			}
			if n := len(f.calls("codex")); n != tc.codex {
				t.Fatalf("codex calls %d", n)
			}
			f.noPublish()
		})
	}
}

// I16.
func TestCodexGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		spec    *fakeSpec
		resp    proctest.Response
		short   bool
		outcome string
	}{
		{
			"high", &fakeSpec{WriteAfter: "-o", WriteContent: `{"high":["loses judgment"]}`},
			proctest.Response{},
			false, "proposal:codex-high",
		},
		{
			"non-zero exit", &fakeSpec{WriteAfter: "-o", WriteContent: `{"high":[]}`},
			proctest.Response{Exit: 1},
			false, "proposal:codex",
		},
		{
			"timeout", nil,
			proctest.Response{Sleep: 2 * time.Minute, NoStdin: true},
			true,
			"proposal:codex",
		},
		{"missing verdict", nil, proctest.Response{}, false, "proposal:codex"},
		{
			"invalid verdict", &fakeSpec{WriteAfter: "-o", WriteContent: `{"high":"x"}`},
			proctest.Response{},
			false, "proposal:codex",
		},
		{
			"extra key", &fakeSpec{WriteAfter: "-o", WriteContent: `{"high":[],"low":[]}`},
			proctest.Response{},
			false, "proposal:codex",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			f.fake("codex", tc.spec, tc.resp)
			if tc.short {
				c := &fakeClock{}
				f.opts.clock = c.now
				f.opts.limits.codexMin = time.Second
				f.opts.hook = func(point string) {
					if point == "check" {
						// Leave the reserve, push and PR, and 5 s for Codex.
						c.advance(29*time.Minute - 3*time.Minute - 5*time.Second)
					}
				}
			}
			start := time.Now()
			st := f.wantOutcome(f.run(), StatusWarn, tc.outcome)
			if took := time.Since(start); took > time.Minute {
				t.Fatalf("took %s", took)
			}
			if st.Patch == "" || st.OID == "" || st.CodexRuns != 1 {
				t.Fatalf("stamp %+v", st)
			}
			if tc.outcome == "proposal:codex-high" && (len(st.CodexHigh) != 1 ||
				st.CodexHigh[0] != "loses judgment") {
				t.Fatalf("codex_high %q", st.CodexHigh)
			}
			calls := f.calls("codex")
			if len(calls) != 1 {
				t.Fatalf("codex calls %d", len(calls))
			}
			args := calls[0].Args
			if len(args) != 14 || strings.Join(args[:7], " ") !=
				"exec --ephemeral --skip-git-repo-check -s read-only -c approval_policy=never" ||
				args[7] != "-C" || args[9] != "--output-schema" || args[11] != "-o" || args[13] != "-" {
				t.Fatalf("codex argv %q", args)
			}
			if !tc.short && (!strings.Contains(calls[0].Stdin, "+alpha two") ||
				!strings.Contains(calls[0].Stdin, "docs: tighten a rule") ||
				!strings.Contains(calls[0].Stdin, "id e1")) {
				t.Fatalf("codex stdin:\n%s", calls[0].Stdin)
			}
			f.noPublish()
		})
	}
}
