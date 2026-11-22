package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shuymn/stack/extensions/internal/safety"
)

// TestBinary is C16: the built stack-guard decides the four moved rules as the core hook does,
// with its own reason prefix.
func TestBinary(t *testing.T) {
	needBuilt(t)
	lp := localPath()

	t.Run("argv", func(t *testing.T) {
		home := t.TempDir()
		stdin := bashPayload(t, t.TempDir(), "cat .dev.vars")
		assertDeny(t, "hook claude pre-tool-use", guardBin(t, home, stdin), RuleSecrets)
		for _, args := range [][]string{
			nil,
			{"hook", "claude"},
			{"hook", "claude", "session-start"},
			{"hook", "claude", "pre-tool-use", "x"},
		} {
			r := runBinary(t, built.guard, home, stdin, args...)
			if r.code != 2 || r.stdout != "" ||
				!strings.HasPrefix(r.stderr, "usage: stack-guard ") {
				t.Errorf("argv %q = %+v, want usage on stderr and exit 2", args, r)
			}
		}
	})

	t.Run("safety.toml from HOME", func(t *testing.T) {
		home := t.TempDir()
		writeSafety(t, home, "secrets = [\"custom.key\"]\ndeny_pencil_generate = false\n")
		cwd := t.TempDir()
		assertDeny(t, "custom secret", guardBin(t, home,
			payload(t, cwd, "Read", map[string]any{"file_path": "custom.key"})), RuleSecrets)
		assertAllow(t, "default secret replaced by the file", guardBin(t, home,
			payload(t, cwd, "Read", map[string]any{"file_path": ".dev.vars"})))
		assertAllow(t, "deny_pencil_generate = false", guardBin(t, home,
			payload(t, cwd, "mcp__pencil__execute", map[string]any{"input": `Generate({})`})))
	})

	t.Run("malformed payload fails open", func(t *testing.T) {
		for _, stdin := range []string{
			"",
			"x",
			"[]",
			`{"tool_name":`,
			`{"tool_name":"Bash","tool_input":"rm -rf /"}`,
			`{"tool_name":"Read","tool_input":{"file_path":7}}`,
			"\x00\xff",
		} {
			r := guardBin(t, t.TempDir(), stdin)
			if r.code != 0 || r.stdout != "" || r.stderr != "" {
				t.Errorf("stdin %q = %+v, want exit 0 and no output", stdin, r)
			}
		}
	})

	t.Run("bash", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		write := func(rel, content string) {
			p := filepath.Join(cwd, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		write("body.md", "see "+lp+"\n")
		write("clean.md", "clean\n")
		if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		write("wt/.git", "gitdir: ../.git/worktrees/wt\n")

		cases := []struct{ cmd, rule string }{
			{`K="$(cat .dev.vars)" cmd`, ""},
			{`cmd --token "$(cat .dev.vars)"`, ""},
			{"cat .dev.vars", RuleSecrets},
			{"gh pr create -t t --body-file body.md", RuleGhPublish},
			{"gh pr create -t t --body-file clean.md", ""},
			{"gh pr create -t t --body-file - <<'EOF'\nsee " + lp + "\nEOF", RuleGhPublish},
			{"gh pr create -t t --body '" + lp + "'", RuleGhPublish},
			{"gh release create v1 " + lp + "/dist/a.tgz --notes clean", ""},
			{"gh api repos/o/r/pulls --jq '.[] | \"" + lp + "\"'", ""},
			{"rm -rf ~", RuleRmRoot},
			{"rm -rf .git", RuleRmRoot},
			{"rm -rf wt/.git", ""},
			{"git status", ""},
		}
		if isWindows {
			cases = append(cases, struct{ cmd, rule string }{"rm -rf C:/", RuleRmRoot})
		} else {
			cases = append(cases, struct{ cmd, rule string }{"rm -rf /", RuleRmRoot})
		}
		for _, c := range cases {
			r := guardBin(t, home, bashPayload(t, cwd, c.cmd))
			if c.rule == "" {
				assertAllow(t, c.cmd, r)
			} else {
				assertDeny(t, c.cmd, r, c.rule)
			}
		}
	})

	t.Run("pencil", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		pencil := func(code string) string {
			return payload(t, cwd, "mcp__pencil__execute", map[string]any{"input": code})
		}
		assertDeny(t, "real call", guardBin(t, home, pencil(`await Generate({prompt: "icon"})`)),
			RulePencil)
		assertAllow(t, "string", guardBin(t, home, pencil(`Print("Generate()")`)))
	})

	// With no home, `~` entries match by their home-relative components and `~` operands are not
	// resolved; the core hook, which keeps only the raw-merge rule, denies none of them.
	t.Run("HOME unset", func(t *testing.T) {
		cwd := t.TempDir()
		for _, c := range []struct {
			name, stdin, rule string
		}{
			{"rm -rf /", bashPayload(t, cwd, "rm -rf /"), RuleRmRoot},
			{"rm -rf ~", bashPayload(t, cwd, "rm -rf ~"), ""},
			{"Read T3 components", payload(t, cwd, "Read",
				map[string]any{"file_path": t3Rel + "/token"}), RuleSecrets},
			{"cat T3 components", bashPayload(t, cwd, "cat "+t3Rel+"/token"), RuleSecrets},
			{"Read .dev.vars", payload(t, cwd, "Read",
				map[string]any{"file_path": ".dev.vars"}), RuleSecrets},
		} {
			g, k := guardBin(t, "", c.stdin), coreBin(t, "", c.stdin)
			if g.code != 0 || k.code != 0 {
				t.Errorf("%s: exit stack-guard %d, core %d, want 0", c.name, g.code, k.code)
			}
			if got := deniedBy(g, "stack-guard"); got != c.rule {
				t.Errorf("%s: stack-guard rule %q (%+v), want %q", c.name, got, g, c.rule)
			}
			if k.stdout != "" {
				t.Errorf("%s: core output %q, want none", c.name, k.stdout)
			}
			if c.rule == "" && g.stdout != "" {
				t.Errorf("%s: want no output, stack-guard %q", c.name, g.stdout)
			}
		}
	})

	// Both hooks run on a matching call (§4.5): an input matching raw-merge and secrets is denied
	// by each, stack-guard for secrets and core for raw-merge (§4.9); a raw merge alone is denied by
	// core only.
	t.Run("two hooks", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		both := bashPayload(t, cwd, "gh pr merge 1 && cat .dev.vars")
		assertDeny(t, "stack-guard on raw-merge and secrets", guardBin(t, home, both), RuleSecrets)
		if rule := deniedBy(coreBin(t, home, both), "stack guard"); rule != "raw-merge" {
			t.Errorf(
				"core on raw-merge and secrets: rule %q, want a stack guard (raw-merge) deny",
				rule,
			)
		}
		merge := bashPayload(t, cwd, "gh pr merge 1")
		assertAllow(t, "stack-guard on a raw merge", guardBin(t, home, merge))
		if rule := deniedBy(coreBin(t, home, merge), "stack guard"); rule != "raw-merge" {
			t.Errorf("core on a raw merge: rule %q, want a stack guard (raw-merge) deny", rule)
		}
	})
}

// TestSafetyConfig is C17: while safety.toml is absent or invalid, the v10 defaults apply.
func TestSafetyConfig(t *testing.T) {
	read := func(cwd, p string) string {
		return payload(t, cwd, "Read", map[string]any{"file_path": p})
	}
	pencil := func(cwd string) string {
		return payload(t, cwd, "mcp__pencil__execute",
			map[string]any{"input": `await Generate({prompt: "icon"})`})
	}
	for name, file := range map[string]string{
		"no file":      "",
		"syntax error": "secrets = [\"custom.key\"\ndeny_pencil_generate = false\n",
		"type error":   "secrets = [\"custom.key\"]\ndeny_pencil_generate = \"no\"\n",
		"string list":  "secrets = \"custom.key\"\ndeny_pencil_generate = false\n",
	} {
		home, cwd := t.TempDir(), t.TempDir()
		if file != "" {
			writeSafety(t, home, file)
		}
		assertDeny(
			t,
			name+": Read .dev.vars",
			runHook(t, home, read(cwd, ".dev.vars")),
			RuleSecrets,
		)
		assertDeny(t, name+": Read T3 secret",
			runHook(t, home, read(cwd, t3Secrets+"/token")), RuleSecrets)
		assertDeny(t, name+": Pencil Generate", runHook(t, home, pencil(cwd)), RulePencil)
		assertAllow(t, name+": custom secret not kept", runHook(t, home, read(cwd, "custom.key")))
	}

	home, cwd := t.TempDir(), t.TempDir()
	writeSafety(t, home, "secrets = [\"custom.key\"]\n")
	assertDeny(
		t,
		"valid file: custom secret",
		runHook(t, home, read(cwd, "custom.key")),
		RuleSecrets,
	)
	assertDeny(t, "valid file: Pencil default kept", runHook(t, home, pencil(cwd)), RulePencil)
}

// TestJevKeySecret: the Jev key file is a secret by default and under a custom secrets list
// that does not name it.
func TestJevKeySecret(t *testing.T) {
	for name, file := range map[string]string{
		"defaults":    "",
		"custom list": "secrets = [\"custom.key\"]\n",
	} {
		home, cwd := t.TempDir(), t.TempDir()
		if file != "" {
			writeSafety(t, home, file)
		}
		key := filepath.Join(home, ".config", "stack", "jev", "key")
		r := runHook(t, home, payload(t, cwd, "Read", map[string]any{"file_path": key}))
		assertDeny(t, name+": Read the key file", r, RuleSecrets)
		var out denyOutput
		if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		reason := out.HookSpecificOutput.PermissionDecisionReason
		want := "stack-guard (secrets): Read targets the secret " +
			strconv.Quote(safety.JevKey) + ". "
		if !strings.HasPrefix(reason, want) {
			t.Errorf("%s: reason %q, want the prefix %q", name, reason, want)
		}
		assertDeny(t, name+": cat the key file",
			runHook(t, home, bashPayload(t, cwd, "cat "+safety.JevKey)), RuleSecrets)
		if file != "" {
			assertDeny(t, name+": custom secret",
				runHook(t, home, payload(t, cwd, "Read", map[string]any{"file_path": "custom.key"})),
				RuleSecrets)
		}
	}
}
