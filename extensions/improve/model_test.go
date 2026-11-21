package improve

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

// I6.
func TestBilling(t *testing.T) {
	t.Parallel()
	// usage is four zero counters with k set to v (deleted when v is nil).
	usage := func(k string, v any) map[string]any {
		u := map[string]any{
			"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 0,
		}
		if v == nil {
			delete(u, k)
		} else if k != "" {
			u[k] = v
		}
		return u
	}
	failure := func(cost any, u any) string {
		r := map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true}
		if cost != nil {
			r["total_cost_usd"] = cost
		}
		if u != nil {
			r["usage"] = u
		}
		return string(mustJSON(t, r))
	}
	zeroFailure := failure(0, usage("", 0))
	for _, tc := range []struct {
		name        string
		setup       func(f *fx)
		stdout      string
		outcome     string
		removed     bool
		costUnknown bool
	}{
		{
			name: "zero result then garbage", stdout: zeroFailure + "\n{broken",
			outcome: "failed:model-result", costUnknown: true,
		},
		{name: "zero no-change then garbage", stdout: claudeOut(map[string]any{
			"change": false, "evidence_ids": []string{}, "summary": "s", "watch": "w",
			"commit_message": "", "pr_title": "", "pr_body": "",
		}, 0, usage("", 0)) + "\n{broken", outcome: "failed:model-result", costUnknown: true},
		{
			name: "zero result then killed", outcome: "failed:model-result", costUnknown: true,
			setup: func(f *fx) {
				f.fake("claude", &fakeSpec{
					Print: []fakePrint{{Contains: "-p", Stdout: zeroFailure}}, Kill: true,
				}, proctest.Response{})
			},
		},
		{
			name: "zero result without type", stdout: strings.Replace(zeroFailure,
				`"type":"result",`, "", 1),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "start failure", setup: func(f *fx) { delete(f.tools, "claude") },
			outcome: "failed:model-start", removed: true,
		},
		{name: "worktree failure", setup: func(f *fx) {
			f.write(".local/state/stack/improve/work/"+week+"/keep", "x")
		}, outcome: "failed:worktree", removed: true},
		{
			name: "zero cost and zero counters", stdout: failure(0, usage("", 0)),
			outcome: "failed:model-result", removed: true,
		},
		{name: "zero cost no-change", stdout: claudeOut(map[string]any{
			"change": false, "evidence_ids": []string{}, "summary": "s", "watch": "w",
			"commit_message": "", "pr_title": "", "pr_body": "",
		}, 0, usage("", 0)), outcome: outNoChange, removed: true},
		{
			name: "empty usage", stdout: failure(0, map[string]any{}),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "missing counter", stdout: failure(0, usage("cache_read_input_tokens", nil)),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "string counter", stdout: failure(0, usage("input_tokens", "0")),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "negative counter", stdout: failure(0, usage("input_tokens", -1)),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "fractional counter", stdout: failure(0, usage("input_tokens", 0.5)),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "no cost", stdout: failure(nil, usage("", 0)),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "string cost", stdout: failure("0", usage("", 0)),
			outcome: "failed:model-result", costUnknown: true,
		},
		{
			name: "non-zero counter", stdout: failure(0, usage("output_tokens", 3)),
			outcome: "failed:model-result",
		},
		{name: "missing response", stdout: "", outcome: "failed:model-result", costUnknown: true},
		{name: "not json", stdout: "oops", outcome: "failed:model-result", costUnknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			if tc.stdout != "" || tc.setup == nil {
				f.fake("claude", nil, proctest.Response{Stdout: tc.stdout})
			}
			if tc.setup != nil {
				tc.setup(f)
			}
			var seen stamp
			f.opts.hook = func(point string) {
				if point == "cleanup" {
					seen = f.stamp()
				}
			}
			resp := f.run()
			if seen.Outcome != tc.outcome {
				t.Fatalf("outcome %q (%s), want %q\n%s", seen.Outcome, seen.Reason, tc.outcome,
					f.stderr)
			}
			if tc.removed {
				f.noStamp()
				if !strings.Contains(resp.Context, "stamp removed") {
					t.Fatalf("context %q", resp.Context)
				}
			} else {
				st := f.stamp()
				if st.Finished == "" {
					t.Fatalf("stamp not final: %+v", st)
				}
			}
			if hasFinding(resp, kindCostUnknown) != tc.costUnknown {
				t.Fatalf("findings %+v, cost unknown %v", resp.Findings, tc.costUnknown)
			}
			if tc.costUnknown && seen.CostUSD != unknownCost {
				t.Fatalf("cost %v", seen.CostUSD)
			}
			if tc.costUnknown && resp.Status != StatusWarn {
				t.Fatalf("status %q", resp.Status)
			}
		})
	}
}

// I7.
func TestModelInvocation(t *testing.T) {
	t.Parallel()
	t.Run("argv env stdin", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		envFile := filepath.Join(t.TempDir(), "env")
		f.opts.Env = append(f.opts.Env, "CLAUDE_CONFIG_DIR=/elsewhere",
			"CLAUDE_CODE_DISABLE_AUTO_MEMORY=0")
		f.fake("claude", &fakeSpec{
			EnvTo: envFile, Edits: []fakeEdit{{Path: "skills/a.md", Content: "alpha two\n"}},
		}, proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))})
		f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
		calls := f.calls("claude")
		if len(calls) != 1 {
			t.Fatalf("calls %d", len(calls))
		}
		want := []string{
			"-p",
			"--setting-sources",
			"user",
			"--strict-mcp-config",
			"--tools",
			"Read,Edit,Write,Grep,Glob",
			"--permission-mode",
			"acceptEdits",
			"--permission-prompts",
			"none",
			"--no-session-persistence",
			"--settings",
			`{"autoMemoryEnabled":false,"permissions":{"blockReadsOutsideWorkingDirectories":true,` +
				`"blockWritesOutsideWorkingDirectories":true}}`,
			"--output-format",
			"json",
			"--json-schema",
			outputSchema,
			"--model",
			"claude-opus-5-5",
			"--effort",
			"xhigh",
			"--max-budget-usd",
			"50",
		}
		if !slices.Equal(calls[0].Args, want) {
			t.Fatalf("argv = %q", calls[0].Args)
		}
		for _, banned := range []string{"--restricted", "--safe-mode", "--add-dir"} {
			if slices.Contains(calls[0].Args, banned) {
				t.Fatalf("argv has %s", banned)
			}
		}
		env, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(env), "\n")
		if !slices.Contains(lines, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") ||
			slices.Contains(lines, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=0") ||
			slices.ContainsFunc(
				lines,
				func(l string) bool { return strings.HasPrefix(l, "CLAUDE_CONFIG_DIR=") },
			) {
			t.Fatalf("env = %q", lines)
		}
		in := calls[0].Stdin
		for _, want := range []string{
			"id e1 |", "# Writing agent knowledge", "## Task", "- skills/**",
			"public repository o/r at commit ",
		} {
			if !strings.Contains(in, want) {
				t.Fatalf("stdin lacks %q:\n%s", want, in)
			}
		}
		if strings.Contains(in, "caps") || strings.Contains(in, "budget.json") {
			t.Fatalf("writing.md still has the caps rules:\n%s", in)
		}
		// P5, P11: the launcher logs what the first real run must show.
		for _, want := range []string{
			`claude argv: ["-p" "--setting-sources" "user"`,
			"claude env: CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 set, CLAUDE_CONFIG_DIR removed; timeout ",
			"claude wall time ", "codex timeout ", "codex wall time ",
		} {
			if !strings.Contains(f.stderr.String(), want) {
				t.Fatalf("stderr lacks %q:\n%s", want, f.stderr)
			}
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		// A frozen clock leaves the model exactly 10 s.
		f.opts.clock = (&fakeClock{}).now
		f.opts.limits = limits{
			afterModel: defaultBudget - defaultReserve - 10*time.Second,
			modelMin:   time.Second,
		}
		fake := f.fake("claude", nil,
			proctest.Response{Sleep: 2 * time.Minute, Grandchild: 2 * time.Minute, NoStdin: true})
		start := time.Now()
		resp := f.run()
		if took := time.Since(start); took > 40*time.Second {
			t.Fatalf("took %s", took)
		}
		st := f.wantOutcome(resp, StatusWarn, "failed:timeout")
		if st.CostUSD != unknownCost || !hasFinding(resp, kindCostUnknown) {
			t.Fatalf("stamp %+v findings %+v", st, resp.Findings)
		}
		calls := proctest.Calls(t, fake)
		if len(calls) != 1 || calls[0].GrandchildPID == 0 {
			t.Fatalf("calls %+v", calls)
		}
		requireGone(t, calls[0].GrandchildPID)
	})
}

// I8.
func TestProposal(t *testing.T) {
	t.Parallel()
	edit := func(e ...fakeEdit) []fakeEdit { return e }
	for _, tc := range []struct {
		name    string
		edits   []fakeEdit
		out     map[string]any
		outcome string
		reason  string
	}{
		{"add", edit(fakeEdit{Path: "skills/new.md", Content: "n\n"}), nil, "proposal:tree", "skills/new.md"},
		{"delete", edit(fakeEdit{Path: "skills/b.md", Remove: true}), nil, "proposal:tree", "skills/b.md"},
		{"rename", edit(fakeEdit{Path: "skills/b.md", Rename: "skills/c.md"}), nil, "proposal:tree", "skills/"},
		{
			"mode change", edit(fakeEdit{Path: "skills/a.md", Content: "alpha\n", Exec: true}), nil,
			"proposal:tree", "skills/a.md",
		},
		{"type change", edit(fakeEdit{Path: "skills/a.md", Symlink: "b.md"}), nil, "proposal:tree", "skills/a.md"},
		{
			"executable file", edit(fakeEdit{Path: "skills/run.sh", Content: "echo two\n", Exec: true}), nil,
			"proposal:tree", "skills/run.sh",
		},
		{"out of scope", edit(fakeEdit{Path: "other/c.md", Content: "g2\n"}), nil, "proposal:tree", "other/c.md"},
		{"** boundary", edit(fakeEdit{Path: "skills2/x.md", Content: "x2\n"}), nil, "proposal:tree", "skills2/x.md"},
		{"empty diff", nil, nil, "proposal:tree", "without a diff"},
		{
			"foreign id", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}), change("zz"),
			"proposal:evidence-ids", "zz",
		},
		{
			"private id", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}), change("p1"),
			"proposal:evidence-ids", "p1",
		},
		{
			"no id", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}), change(),
			"proposal:evidence-ids", "no evidence",
		},
		{
			"blank title", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}),
			with(change("e1"), "pr_title", "  "), "proposal:metadata", "title",
		},
		{
			"multi-line title", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}),
			with(change("e1"), "pr_title", "a\nb"), "proposal:metadata", "title",
		},
		{
			"blank body", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}),
			with(change("e1"), "pr_body", ""), "proposal:metadata", "body",
		},
		{
			"blank message", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}),
			with(change("e1"), "commit_message", "\n"), "proposal:metadata", "message",
		},
		{
			"schema mismatch", edit(fakeEdit{Path: "skills/a.md", Content: "a2\n"}),
			with(change("e1"), "extra", "x"), "failed:model-result", "schema",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			out := tc.out
			if out == nil {
				out = change("e1")
			}
			f.fake("claude", &fakeSpec{Edits: tc.edits},
				proctest.Response{Stdout: claudeOut(out, 1.0, zeroUsage(1))})
			head := f.repo.Git("rev-parse", "HEAD")
			refs := f.repo.Git("for-each-ref", "--exclude=refs/remotes/origin/main")
			resp := f.run()
			st := f.wantOutcome(resp, StatusWarn, tc.outcome)
			if !strings.Contains(st.Reason, tc.reason) {
				t.Fatalf("reason %q, want %q", st.Reason, tc.reason)
			}
			if len(tc.edits) > 0 && st.Patch == "" {
				t.Fatal("no patch in the stamp")
			}
			if st.OID != "" || f.repo.Git("rev-parse", "HEAD") != head ||
				f.repo.Git("for-each-ref", "--exclude=refs/remotes/origin/main") != refs {
				t.Fatalf("a commit or ref was made: oid %q", st.OID)
			}
			if len(f.calls("stack")) != 0 || len(f.calls("codex")) != 0 {
				t.Fatal("checks ran")
			}
			f.noPublish()
		})
	}
}

func with(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}

// I9.
func TestCommit(t *testing.T) {
	t.Parallel()
	t.Run("failing commit-msg hook", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		hooks := filepath.Join(f.repo.Root, "hooks")
		writeHook(t, hooks, "commit-msg", "exit 1")
		f.repo.Config("core.hooksPath", hooks)
		st := f.wantOutcome(f.run(), StatusWarn, "failed:commit")
		if !strings.Contains(st.Patch, "+alpha two") {
			t.Fatalf("patch %q", st.Patch)
		}
		f.noPublish()
	})
	t.Run("hooks run", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		hooks := filepath.Join(f.repo.Root, "hooks")
		marker := filepath.Join(t.TempDir(), "ran")
		writeHook(t, hooks, "pre-commit", "echo pre >> '"+marker+"'")
		writeHook(t, hooks, "commit-msg", "echo msg >> '"+marker+"'")
		f.repo.Config("core.hooksPath", hooks)
		f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
		if data, _ := os.ReadFile(marker); string(data) != "pre\nmsg\n" {
			t.Fatalf("hooks ran: %q", data)
		}
	})
}

// I12.
func TestCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		setup   func(f *fx)
		outcome string
	}{
		{"proposal", func(f *fx) {
			f.fake("claude", &fakeSpec{Edits: []fakeEdit{{Path: "other/c.md", Content: "x"}}},
				proctest.Response{Stdout: claudeOut(change("e1"), 1.0, zeroUsage(1))})
		}, "proposal:tree"},
		{"failure", func(f *fx) {
			hooks := filepath.Join(f.repo.Root, "hooks")
			writeHook(t, hooks, "pre-commit", "exit 1")
			f.repo.Config("core.hooksPath", hooks)
		}, "failed:commit"},
		{"success", func(*fx) {}, "pr https://github.com/o/r/pull/1"},
		// git registers the worktree, then reports the failing hook.
		{"add fails after registering", func(f *fx) {
			hooks := filepath.Join(f.repo.Root, "hooks")
			writeHook(t, hooks, "post-checkout", "exit 1")
			f.repo.Config("core.hooksPath", hooks)
		}, "failed:worktree"},
		{"unbilled", func(f *fx) {
			f.fake("claude", nil, proctest.Response{Exit: 1, Stdout: `{"type":"result","subtype":"error",` +
				`"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0,` +
				`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`})
		}, "failed:model-result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			tc.setup(f)
			var atCleanup struct {
				stamp    bool
				worktree bool
			}
			f.opts.hook = func(point string) {
				if point == "cleanup" {
					_, atCleanup.stamp = f.stampRaw()
					_, err := os.Stat(f.worktreeDir())
					atCleanup.worktree = err == nil
				}
			}
			resp := f.run()
			if !strings.Contains(resp.Context, tc.outcome) {
				t.Fatalf("context %q", resp.Context)
			}
			if !atCleanup.stamp || atCleanup.worktree {
				t.Fatalf(
					"at cleanup: %+v (want the stamp present and the worktree gone)",
					atCleanup,
				)
			}
			if _, err := os.Stat(f.worktreeDir()); !os.IsNotExist(err) {
				t.Fatalf("worktree left: %v", err)
			}
			if strings.Contains(f.repo.Git("worktree", "list", "--porcelain"), "improve/work") {
				t.Fatal("worktree metadata left")
			}
		})
	}
	t.Run("leftover of another week", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		keep := f.write(".local/state/stack/improve/work/2026-W40/keep", "mine")
		resp := f.run()
		if !strings.Contains(resp.Context, "pr https://github.com/o/r/pull/1") ||
			resp.Status != StatusWarn {
			t.Fatalf("resp %+v", resp)
		}
		left := slices.IndexFunc(resp.Findings, func(fd Finding) bool {
			return fd.Kind == kindLeftover && fd.Where == filepath.Dir(keep)
		})
		if left < 0 {
			t.Fatalf("findings %+v", resp.Findings)
		}
		if data, err := os.ReadFile(keep); err != nil || string(data) != "mine" {
			t.Fatalf("leftover touched: %q %v", data, err)
		}
	})
	t.Run("leftover at this week", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		keep := f.write(".local/state/stack/improve/work/"+week+"/keep", "mine")
		var seen stamp
		f.opts.hook = func(point string) {
			if point == "cleanup" {
				seen = f.stamp()
			}
		}
		resp := f.run()
		if seen.Outcome != "failed:worktree" || !hasFinding(resp, kindLeftover) {
			t.Fatalf("outcome %q findings %+v", seen.Outcome, resp.Findings)
		}
		if data, err := os.ReadFile(keep); err != nil || string(data) != "mine" {
			t.Fatalf("leftover touched: %q %v", data, err)
		}
		if strings.Contains(f.stderr.String(), "worktree remove") {
			t.Fatalf("worktree remove ran:\n%s", f.stderr)
		}
		f.noStamp()
		if len(f.calls("claude")) != 0 {
			t.Fatal("claude ran")
		}
	})
}
