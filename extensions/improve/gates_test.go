package improve

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/proc/proctest"
)

// recordLookups makes f record every tool improve looks up.
func (f *fx) recordLookups() *[]string {
	var asked []string
	inner := f.opts.LookPath
	f.opts.LookPath = func(name string) (string, error) {
		asked = append(asked, name)
		return inner(name)
	}
	return &asked
}

// I1.
func TestNotConfigured(t *testing.T) {
	t.Parallel()
	t.Run("disabled calls nothing", func(t *testing.T) {
		t.Parallel()
		for _, cfg := range []string{"", "enabled = false\n", "enabled = \"true\"\n", "enabled = 1\n"} {
			home := t.TempDir()
			if cfg != "" {
				p := filepath.Join(home, ".config", "stack", "improve.toml")
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var asked []string
			resp := Run(context.Background(), Options{
				Home: home, Env: []string{},
				LookPath: func(name string) (string, error) {
					asked = append(asked, name)
					return "", errNotFake
				},
			}, Request{Stage: "cycle", Repo: home})
			if resp.Status != StatusPass || resp.Context != "improve disabled" || len(asked) != 0 {
				t.Fatalf("config %q: resp %+v, lookups %q", cfg, resp, asked)
			}
			if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
				t.Fatalf("config %q: state written", cfg)
			}
		}
	})
	t.Run("invalid toml", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.write(".config/stack/improve.toml", "enabled = = true\n")
		asked := f.recordLookups()
		resp := f.run()
		if resp.Status != StatusWarn || len(*asked) != 0 {
			t.Fatalf("resp %+v lookups %q", resp, *asked)
		}
	})
	t.Run("windows", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.opts.goos = "windows"
		asked := f.recordLookups()
		resp := f.run()
		if resp.Status != StatusPass || resp.Context != "improve runs on macOS and Linux" ||
			len(*asked) != 0 {
			t.Fatalf("resp %+v lookups %q", resp, *asked)
		}
	})
	t.Run("empty repo", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.req.Repo = ""
		asked := f.recordLookups()
		resp := f.run()
		if resp.Status != StatusPass || resp.Context != "not in a repository" || len(*asked) != 0 {
			t.Fatalf("resp %+v lookups %q", resp, *asked)
		}
	})
	t.Run("no repository config", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.repo.Git("rm", "-q", ".stack/improve.toml")
		f.repo.Git("commit", "-q", "-m", "drop config")
		f.repo.Git("push", "-q", "origin", "main")
		resp := f.run()
		if resp.Status != StatusPass || resp.Context != "o/r has no .stack/improve.toml" {
			t.Fatalf("resp %+v", resp)
		}
		calls := f.calls("gh")
		if len(calls) != 1 || !slices.Equal(calls[0].Args, argsRepoView) {
			t.Fatalf("gh calls = %+v, want only repo view", calls)
		}
		if len(f.calls("claude")) != 0 {
			t.Fatal("claude ran")
		}
		f.noStamp()
	})
	t.Run("invalid repository config", func(t *testing.T) {
		t.Parallel()
		for _, cfg := range []string{
			"paths = []\n", "paths = \"skills\"\n", "x = 1\n",
			"paths = [\"skills/[\"]\n",
		} {
			f := newFx(t)
			f.repo.Write(".stack/improve.toml", cfg)
			f.repo.Git("commit", "-q", "-am", "config")
			f.repo.Git("push", "-q", "origin", "main")
			if resp := f.run(); resp.Status != StatusWarn || len(f.calls("claude")) != 0 {
				t.Fatalf("config %q: resp %+v", cfg, resp)
			}
		}
	})
	t.Run("uncommitted config does not count", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.repo.Write(".stack/improve.toml", "paths = [\"other/**\"]\n")
		resp := f.run()
		st := f.wantOutcome(resp, StatusPass, "pr https://github.com/o/r/pull/1")
		if st.OID == "" {
			t.Fatal("no oid")
		}
	})
}

// I15.
func TestTarget(t *testing.T) {
	t.Parallel()
	view := func(name, visibility string) string {
		return `{"nameWithOwner":"` + name + `","visibility":"` + visibility +
			`","defaultBranchRef":{"name":"main"}}`
	}
	for _, tc := range []struct {
		name     string
		view     proctest.Response
		settings string
		origin   string
		setup    func(f *fx)
		status   string
		context  string
	}{
		{
			name: "private", view: proctest.Response{Stdout: view("o/r", "PRIVATE")},
			status: StatusPass, context: "not a public GitHub repository",
		},
		{
			name: "internal", view: proctest.Response{Stdout: view("o/r", "INTERNAL")},
			status: StatusPass, context: "not a public GitHub repository",
		},
		{
			name: "name mismatch", view: proctest.Response{Stdout: view("o/other", "PUBLIC")},
			status: StatusPass, context: "not a public GitHub repository",
		},
		{
			name: "case-insensitive name", view: proctest.Response{Stdout: view("O/R", "PUBLIC")},
			status: StatusPass, context: "pr https://github.com/o/r/pull/1",
		},
		{
			name: "gh error", view: proctest.Response{Exit: 1, Stderr: "boom"},
			status: StatusWarn, context: "gh repo view failed",
		},
		{
			name: "not github", origin: "https://example.com/o/r.git",
			status: StatusPass, context: "origin is not exactly one GitHub URL",
		},
		{
			// The look-alike host reaches the bare remote, so only the parse stops the publish.
			name: "look-alike host", origin: "https://notgithub.com/o/r.git",
			setup: func(f *fx) {
				f.repo.Config("url."+filepath.ToSlash(f.bare)+".insteadOf",
					"https://notgithub.com/o/r.git")
			},
			status: StatusPass, context: "origin is not exactly one GitHub URL",
		},
		{
			name: "two origin URLs",
			setup: func(f *fx) {
				f.repo.Git("config", "--add", "remote.origin.url", "https://github.com/x/y.git")
			},
			status: StatusPass, context: "origin is not exactly one GitHub URL",
		},
		{
			name: "ssh origin", origin: "git@github.com:o/r.git",
			setup: func(f *fx) {
				f.repo.Config("url."+filepath.ToSlash(f.bare)+".insteadOf", "git@github.com:o/r.git")
			},
			status: StatusPass, context: "pr https://github.com/o/r/pull/1",
		},
		{
			name: "additional directories", status: StatusWarn, context: "additionalDirectories",
			settings: `{"permissions":{"additionalDirectories":["/x"]}}`,
		},
		{name: "unreadable settings", settings: "{", status: StatusWarn, context: "settings.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			tc.view.Args = argsRepoView
			if tc.view.Stdout == "" && tc.view.Exit == 0 {
				tc.view.Stdout = repoView
			}
			f.fake("gh", f.ghSpec(), tc.view,
				proctest.Response{Args: argsOpenPRs, Stdout: "[[]]"},
				proctest.Response{Args: argsReadPRs},
				proctest.Response{Stdout: "https://github.com/o/r/pull/1\n"})
			if tc.settings != "" {
				f.write(".claude/settings.json", tc.settings)
			}
			if tc.origin != "" {
				f.repo.Git("remote", "set-url", "origin", tc.origin)
			}
			if tc.setup != nil {
				tc.setup(f)
			}
			resp := f.run()
			if resp.Status != tc.status || !strings.Contains(resp.Context, tc.context) {
				t.Fatalf("resp %+v, want %s %q", resp, tc.status, tc.context)
			}
			if tc.context != "pr https://github.com/o/r/pull/1" {
				if len(f.calls("claude")) != 0 {
					t.Fatal("claude ran")
				}
				f.noStamp()
				f.noPublish()
			}
		})
	}
}

// I4.
func TestOpenPR(t *testing.T) {
	t.Parallel()
	open := func(ref, headRepo string, age time.Duration) string {
		return `{"number":5,"html_url":"https://github.com/o/r/pull/5","created_at":"` +
			iso(now.Add(-age)) + `","head":{"ref":"` + ref + `","sha":"x","repo":{"full_name":"` +
			headRepo + `"}},"base":{"ref":"main"}}`
	}
	other := `{"number":4,"head":{"ref":"feat/x","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}`
	day := 24 * time.Hour
	for _, tc := range []struct {
		name    string
		resp    proctest.Response
		status  string
		context string
		attempt bool
	}{
		{
			name: "match on page 2", status: StatusPass, context: "open improve PR #5",
			resp: proctest.Response{Stdout: "[[" + other + "],[" + open("improve/2026-W46", "o/r", day) + "]]"},
		},
		{
			name: "kaizen prefix", status: StatusPass, context: "open improve PR #5",
			resp: proctest.Response{Stdout: "[[" + open("kaizen/improve-1", "O/R", day) + "]]"},
		},
		{
			name: "13 days", status: StatusPass, context: "open improve PR #5",
			resp: proctest.Response{Stdout: "[[" + open("improve/x", "o/r", 13*day) + "]]"},
		},
		{
			name: "15 days", status: StatusWarn, context: "open for 15 days",
			resp: proctest.Response{Stdout: "[[" + open("improve/x", "o/r", 15*day) + "]]"},
		},
		{
			name: "fork head ignored", attempt: true, status: StatusPass, context: "pr https://",
			resp: proctest.Response{Stdout: "[[" + open("improve/x", "fork/r", day) + "]]"},
		},
		{
			name: "deleted head repo ignored", attempt: true, status: StatusPass, context: "pr https://",
			resp: proctest.Response{Stdout: `[[{"number":6,"head":{"ref":"improve/x","repo":null}}]]`},
		},
		{
			name: "gh error", status: StatusWarn, context: "open PR check failed",
			resp: proctest.Response{Exit: 1},
		},
		{
			name: "malformed", status: StatusWarn, context: "open PR check failed",
			resp: proctest.Response{Stdout: `[{"number":1}]`},
		},
		{
			name: "null", status: StatusWarn, context: "open PR check failed",
			resp: proctest.Response{Stdout: `null`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			tc.resp.Args = argsOpenPRs
			f.fake("gh", f.ghSpec(),
				proctest.Response{Args: argsRepoView, Stdout: repoView}, tc.resp,
				proctest.Response{Args: argsReadPRs},
				proctest.Response{Stdout: "https://github.com/o/r/pull/1\n"})
			resp := f.run()
			if resp.Status != tc.status || !strings.Contains(resp.Context, tc.context) {
				t.Fatalf("resp %+v", resp)
			}
			if !tc.attempt {
				f.noStamp()
				if len(f.calls("claude")) != 0 {
					t.Fatal("claude ran")
				}
			}
		})
	}
}

// I2.
func TestStampExists(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stamp, status, context string
	}{
		{
			"no-change", `{"week":"2026-W47","outcome":"no-change","cost_usd":2}`, StatusPass,
			"already attempted: no-change",
		},
		{
			"pr", `{"outcome":"pr https://github.com/o/r/pull/9","cost_usd":2}`, StatusPass,
			"already attempted: pr https://github.com/o/r/pull/9",
		},
		{"cost unknown", `{"outcome":"no-change","cost_usd":"unknown"}`, StatusWarn, "no-change"},
		{"proposal", `{"outcome":"proposal:tree","cost_usd":1}`, StatusWarn, "proposal:tree"},
		{"failed", `{"outcome":"failed:commit","cost_usd":1}`, StatusWarn, "failed:commit"},
		{"pushed-no-pr", `{"outcome":"pushed-no-pr","cost_usd":1}`, StatusWarn, "pushed-no-pr"},
		{"publish-unknown", `{"outcome":"publish-unknown"}`, StatusWarn, "publish-unknown"},
		{"empty", "", StatusWarn, "attempt outcome unknown"},
		{"bad json", "{", StatusWarn, "attempt outcome unknown"},
		{
			"started bare", `{"outcome":"started"}`, StatusWarn,
			"attempt outcome unknown for 2026-W47 (no oid, branch, title or body recorded)",
		},
		{
			"started recorded", `{"outcome":"started","oid":"abc","branch":"improve/2026-W47",` +
				`"title":"T","body":"Body"}`, StatusWarn,
			"oid abc; branch improve/2026-W47; title T; body 4 bytes",
		},
		{
			"wrong types", `{"outcome":"started","oid":"abc","cost_usd":[1],"codex_runs":"x"}`,
			StatusWarn, "oid abc",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			p := f.write(".local/state/stack/improve/"+week, tc.stamp)
			resp := f.run()
			if resp.Status != tc.status || !strings.Contains(resp.Context, tc.context) {
				t.Fatalf("resp %+v, want %s %q", resp, tc.status, tc.context)
			}
			if len(f.calls("claude")) != 0 {
				t.Fatal("claude ran")
			}
			if data, _ := os.ReadFile(p); string(data) != tc.stamp {
				t.Fatalf("stamp changed: %q", data)
			}
		})
	}
}

// I3.
func TestEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		setup   func(f *fx)
		status  string
		context string
	}{
		{"missing", func(f *fx) {
			_ = os.Remove(latestPath(f.home))
		}, StatusPass, "no observation"},
		{
			"stale", func(f *fx) { f.latest(now.Add(-25*time.Hour), publicItems()) },
			StatusPass, "stale observation",
		},
		{
			"unparsable", func(f *fx) { f.write(".local/state/stack/observe/latest.json", "{") },
			StatusWarn, "not valid JSON",
		},
		{"unknown version", func(f *fx) {
			f.write(".local/state/stack/observe/latest.json",
				`{"version":2,"window":{"until":"`+iso(now)+`"},"items":[]}`)
		}, StatusWarn, "unknown version"},
		{"no version", func(f *fx) {
			f.write(".local/state/stack/observe/latest.json", `{"window":{"until":"`+iso(now)+`"}}`)
		}, StatusWarn, "unknown version"},
		{
			"future", func(f *fx) { f.latest(now.Add(10*time.Minute), publicItems()) },
			StatusWarn, "in the future",
		},
		{
			"no public items", func(f *fx) { f.latest(now, publicItems()[1:]) },
			StatusPass, "no public evidence",
		},
		{"no public items, gh failed", func(f *fx) {
			f.write(".local/state/stack/observe/latest.json", `{"version":1,"window":{"until":"`+
				iso(now)+`"},"sources":{"gh":{"status":"fail"}},"items":[]}`)
		}, StatusPass, "no public evidence (gh source fail)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			tc.setup(f)
			resp := f.run()
			if resp.Status != tc.status || !strings.Contains(resp.Context, tc.context) {
				t.Fatalf("resp %+v, want %s %q", resp, tc.status, tc.context)
			}
			f.noStamp()
			if len(f.calls("claude")) != 0 {
				t.Fatal("claude ran")
			}
		})
	}
	t.Run("gh failed with public items", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		items := publicItems()
		doc := `{"version":1,"window":{"until":"` + iso(
			now,
		) + `"},"sources":{"gh":{"status":"fail"}},"items":` +
			string(
				mustJSON(t, items),
			) + `}`
		f.write(".local/state/stack/observe/latest.json", doc)
		f.wantOutcome(f.run(), StatusPass, "pr https://github.com/o/r/pull/1")
		calls := f.calls("claude")
		if len(calls) != 1 {
			t.Fatalf("claude calls = %d", len(calls))
		}
		in := calls[0].Stdin
		if !strings.Contains(
			in,
			"- id e1 | revert | https://github.com/o/r/pull/7 | Revert a rule",
		) {
			t.Fatalf("brief missing the public item:\n%s", in)
		}
		for _, private := range []string{
			"p1", "private note", "/w/private.jsonl", "u1",
			"example.com",
		} {
			if strings.Contains(in, "id "+private) || strings.Contains(in, private+" |") ||
				strings.Contains(in, "| "+private) {
				t.Fatalf("brief holds %q:\n%s", private, in)
			}
		}
	})
}
