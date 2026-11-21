package improve

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		if len(f.calls("stack-public")) != 0 || len(f.calls("codex")) != 0 {
			t.Fatal("checks ran")
		}
		f.noPublish()
	})
	t.Run("replace ref hides the extra file", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		hooks := filepath.Join(f.repo.Root, "hooks")
		writeHook(t, hooks, "pre-commit", "echo more >> other/c.md && git add other/c.md")
		// A look-alike commit with only the allowed change replaces the real one.
		writeHook(t, hooks, "post-commit", `idx="$(git rev-parse --git-dir)/safe-index"
GIT_INDEX_FILE="$idx" git read-tree HEAD^ &&
GIT_INDEX_FILE="$idx" git update-index --cacheinfo "100644,$(git rev-parse HEAD:skills/a.md),skills/a.md" &&
tree=$(GIT_INDEX_FILE="$idx" git write-tree) &&
safe=$(git commit-tree "$tree" -p HEAD^ -m safe) &&
git replace HEAD "$safe"`)
		f.repo.Config("core.hooksPath", hooks)
		st := f.wantOutcome(f.run(), StatusWarn, "proposal:commit-scope")
		if !strings.Contains(st.Reason, "other/c.md") {
			t.Fatalf("reason %q", st.Reason)
		}
		if f.repo.Git("for-each-ref", "refs/replace/") == "" {
			t.Fatal("the hook made no replace ref")
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
		calls := f.calls("stack-public")
		if len(calls) != 1 || calls[0].Args[1] != f.stamp().BaseOID+".."+st.OID {
			t.Fatalf("stack calls %+v", calls)
		}
	})
}

// I10 with the real stack-public check (C21 of docs/design/core-stages.md): hits stop the PR, and
// a stack-public that exits 2 or is not on PATH is a failed check with no Codex, push or PR.
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
		{name: "broken safety.toml", edit: "alpha two\n", terms: "safety", outcome: "failed:check"},
		{name: "not on PATH", edit: "alpha two\n", terms: "missing", outcome: "failed:check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			f.tools["stack-public"] = built.public
			switch tc.terms {
			case "dir":
				f.write(".config/stack/private-terms/x", "")
			default:
				f.write(".config/stack/private-terms", term+"\n")
			}
			switch tc.terms {
			case "safety":
				f.write(".config/stack/safety.toml", "private_terms = [\n")
			case "missing":
				delete(f.tools, "stack-public")
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
				if len(f.calls("codex")) != 0 {
					t.Fatal("codex ran")
				}
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
		reason  string
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
			// The value straddles a 64 KiB read and lies past the old 8 MiB cap.
			name: "large new file", outcome: "proposal:credential", setup: func(f *fx) {
				big := strings.Repeat("x", 8<<20-3) + settingsToken
				f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "notes.txt", Content: big}}},
					proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))})
			},
		},
		{
			name: "unreadable new file", outcome: "proposal:credential", reason: "cannot scan",
			setup: func(f *fx) {
				if os.Geteuid() == 0 {
					f.t.Skip("root reads a write-only file")
				}
				f.fake("claude", &fakeSpec{Edits: []fakeEdit{
					{Path: "skills/a.md", Content: "a2\n"},
					{Path: "notes.txt", Content: "n", NoRead: true},
				}}, proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))})
			},
		},
		{
			name: "settings value on gh's stderr", outcome: "pr https://github.com/o/r/pull/1",
			setup: func(f *fx) {
				f.fake("gh", f.ghSpec(),
					proctest.Response{
						Args: argsRepoView, Stdout: repoView, Stderr: "using " + settingsToken + "\n",
					},
					proctest.Response{Args: argsOpenPRs, Stdout: "[[]]"},
					proctest.Response{Args: argsReadPRs},
					proctest.Response{Stdout: "https://github.com/o/r/pull/1\n"})
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
			if !strings.Contains(st.Reason, tc.reason) {
				t.Fatalf("reason %q, want %q", st.Reason, tc.reason)
			}
			if strings.HasPrefix(tc.outcome, "pr ") {
				if !strings.Contains(f.stderr.String(), redactMark) {
					t.Fatalf("stderr not passed through redacted:\n%s", f.stderr)
				}
			} else {
				if len(f.calls("stack-public")) != 0 || len(f.calls("codex")) != 0 {
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
	// tick, once set, moves the clock on by itself at every reading.
	tick atomic.Int64
}

var clockBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (c *fakeClock) now() time.Time {
	return clockBase.Add(time.Duration(c.ahead.Add(c.tick.Load())))
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
		// One minute left: the worktree removal still runs inside the reserve.
		{"at the reserve", "codex", 28 * time.Minute, "failed:deadline", false, 1, 1},
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
			if _, err := os.Stat(f.worktreeDir()); !os.IsNotExist(err) {
				t.Fatalf("worktree left: %v\n%s", err, f.stderr)
			}
		})
	}
	t.Run("slow untracked scan", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		c := &fakeClock{}
		f.opts.clock = c.now
		// 64 MiB, sparse: read whole it holds no credential and the outcome is proposal:tree.
		f.fake("claude", &fakeSpec{Edits: []fakeEdit{
			{Path: "skills/a.md", Content: "a2\n"},
			{Path: "notes.txt", Content: "n", Size: 64 << 20},
		}}, proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))})
		f.opts.hook = func(point string) {
			if point == "model" {
				// 10 s past the reserve, then 100 ms per clock reading: the scan reaches the
				// reserve after about 100 chunks, long before the end of the file.
				c.advance(defaultBudget - defaultReserve - 10*time.Second)
				c.tick.Store(int64(100 * time.Millisecond))
			}
		}
		start := time.Now()
		st := f.wantOutcome(f.run(), StatusWarn, "proposal:credential")
		took := time.Since(start)
		if took > time.Minute || !strings.Contains(st.Reason, "cannot scan") {
			t.Fatalf("took %s, reason %q", took, st.Reason)
		}
		if _, err := os.Stat(f.worktreeDir()); !os.IsNotExist(err) {
			t.Fatalf("worktree left: %v\n%s", err, f.stderr)
		}
	})
	t.Run("slow Codex input", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		c := &fakeClock{}
		f.opts.clock = c.now
		f.fake("codex", nil, proctest.Response{Sleep: 2 * time.Minute, NoStdin: true})
		f.opts.hook = func(point string) {
			switch point {
			case "check":
				// 5 min 30 s for Codex when the check ends ...
				c.advance(29*time.Minute - 3*time.Minute - 5*time.Minute - 30*time.Second)
			case "codex-input":
				// ... and 1 s once its input is ready.
				c.advance(5*time.Minute + 29*time.Second)
			}
		}
		start := time.Now()
		f.wantOutcome(f.run(), StatusWarn, "proposal:codex")
		if took := time.Since(start); took > 30*time.Second {
			t.Fatalf("took %s: the Codex timeout was not cut after its input", took)
		}
		f.noPublish()
	})
	t.Run("create and read share a step", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		// Default limits, so every other step keeps a full minute under load; only the PR step is
		// cut short, and gh pr create hangs in it.
		const prStep = 20 * time.Second
		c := &realClock{}
		f.opts.clock = c.now
		f.fake("gh", f.ghSpec(),
			proctest.Response{Args: argsRepoView, Stdout: repoView},
			proctest.Response{Args: argsOpenPRs, Stdout: "[[]]"},
			proctest.Response{Args: argsReadPRs},
			proctest.Response{Sleep: time.Minute})
		f.opts.hook = func(point string) {
			switch point {
			case "codex":
				c.leave(defaultBudget, defaultReserve+defaultPushPR+time.Second)
			case "pushed":
				// A slow push leaves 20 s: create gets half, the read the rest. With separate
				// timeouts create would use all 20 s and the read none.
				c.leave(defaultBudget, defaultReserve+prStep)
			}
		}
		f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
		if n := f.ghCalls(argsReadPRs...); n != 1 {
			t.Fatalf("PR reads = %d, want 1", n)
		}
	})
}

// realClock runs at real speed from the first reading, shifted by what the test moves it.
type realClock struct {
	mu    sync.Mutex
	first time.Time
	start time.Time
	ahead time.Duration
}

func (c *realClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start.IsZero() {
		c.start, c.first = time.Now(), clockBase
	}
	return c.first.Add(time.Since(c.start) + c.ahead)
}

// leave moves the clock so that left remains before the deadline the first reading set with
// budget.
func (c *realClock) leave(budget, left time.Duration) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ahead += c.first.Add(budget).Sub(now) - left
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
						// Leave the reserve, push and PR, and 20 s for Codex: enough for the fake
						// to start and log its call on a loaded machine.
						c.advance(29*time.Minute - 3*time.Minute - 20*time.Second)
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
