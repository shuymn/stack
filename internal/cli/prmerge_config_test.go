package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/ext"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// TestPRMergeConfig is C19 through the CLI, with the real stack-public and a fake gh: pr merge
// loads and validates config.toml before any gh call in every case, and runs
// [merge].message_checks, never [stages].outbound, on --subject/--body before the first gh call.
// PATH holds only the test's fakes and, where a case says so, stack-public, so no host `stack-*`
// binary can start. Not parallel: it changes PATH, HOME and the working directory.
func TestPRMergeConfig(t *testing.T) {
	needPublic(t)
	local := "see " + strings.Join([]string{"", "home", "bob", "x"}, "/") // built at run time
	const clean = "Merged after review."

	type merge struct {
		name   string
		config string // config.toml; "" means none
		terms  []byte // the private-terms file; nil means none
		public bool   // stack-public on PATH
		fakes  map[string]string
		args   []string // message flags
		// code is the exit code; merged whether gh pr merge ran; stderr a substring of stderr.
		code   int
		merged bool
		stderr string
	}
	body := func(text string) []string { return []string{"--body", text} }
	listed := "[merge]\nmessage_checks = [\"public\"]\n"
	for _, tt := range []merge{
		{
			name: "no config.toml and no stack-* on PATH", args: body(local),
			code: exitOK, merged: true,
		},
		{
			name: "explicit empty message_checks", config: "[merge]\nmessage_checks = []\n",
			public: true, args: body(local), code: exitOK, merged: true,
		},
		{
			name:   "[stages].outbound names a missing extension",
			config: "[stages]\noutbound = [\"absent\"]\n[merge]\nmessage_checks = []\n",
			args:   body(local), code: exitOK, merged: true,
		},
		{
			name: "public listed but not on PATH", config: listed, args: body(clean),
			code: exitRefused,
			stderr: "stack pr merge: outbound could not check the message: public: " +
				"not installed",
		},
		{
			name: "public hit in --body", config: listed, public: true, args: body(local),
			code: exitRefused, stderr: "stack pr merge: outbound blocked: public: check public: ",
		},
		{
			name: "public hit in --subject", config: listed, public: true,
			args: []string{"--subject", local}, code: exitRefused,
			stderr: "stack pr merge: outbound blocked: public: ",
		},
		{
			name: "private term in --body", config: listed, public: true, terms: []byte("Zebra\n"),
			args: body("about zebra"), code: exitRefused,
			stderr: "stack pr merge: outbound blocked: public: ",
		},
		{
			name: "clean message", config: listed, public: true, args: body(clean),
			code: exitOK, merged: true,
		},
		{
			name: "empty --body is checked", config: "[merge]\nmessage_checks = [\"gate\"]\n",
			fakes: map[string]string{"gate": `{"status":"block","context":"STOP"}`},
			args:  body(""), code: exitRefused,
			stderr: "stack pr merge: outbound blocked: gate: STOP\n",
		},
		{
			name: "valid warn continues", config: "[merge]\nmessage_checks = [\"note\"]\n",
			fakes: map[string]string{"note": `{"status":"warn"}`}, args: body(clean),
			code: exitOK, merged: true, stderr: "[extension note: warn: warn]\n",
		},
		{
			name: "no message flag runs no check", config: "[merge]\nmessage_checks = [\"gate\"]\n",
			fakes: map[string]string{"gate": `{"status":"block","context":"STOP"}`},
			code:  exitOK, merged: true,
		},
		{
			name: "broken config.toml without a message flag", config: "[merge\n", public: true,
			code: exitUsage, stderr: "invalid config",
		},
		{
			name: "broken config.toml with a message flag", config: "[merge\n", public: true,
			args: body(clean), code: exitUsage, stderr: "invalid config",
		},
		{
			name: "broken private-terms file without a message flag", config: listed,
			public: true, terms: []byte("\xff\n"), code: exitOK, merged: true,
		},
		{
			name: "broken private-terms file with a message flag", config: listed,
			public: true, terms: []byte("\xff\n"), args: body(clean), code: exitRefused,
			stderr: "stack pr merge: outbound blocked: public: ",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ghArgs []string
			if len(tt.args) == 2 {
				ghArgs = []string{tt.args[0] + "=" + tt.args[1]}
			}
			gh, home := mergeFixture(t, tt.public, tt.fakes, ghArgs...)
			if tt.config != "" {
				writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
					[]byte(tt.config))
			}
			if tt.terms != nil {
				writeFile(t, filepath.Join(home, ".config", "stack", "private-terms"), tt.terms)
			}
			res := runIn(t, home, nil,
				append([]string{"pr", "merge", "7", prHead[:9], "--merge"}, tt.args...)...)
			calls := proctest.Calls(t, gh)
			if res.code != tt.code || !strings.Contains(res.stderr, tt.stderr) {
				t.Errorf("pr merge = %+v, want exit %d with %q", res, tt.code, tt.stderr)
			}
			if got := ghMerged(t, gh); got != tt.merged {
				t.Errorf("gh pr merge ran = %v, want %v", got, tt.merged)
			}
			if !tt.merged && len(calls) != 0 {
				t.Errorf("gh calls = %+v, want none", calls)
			}
		})
	}

	t.Run("an extension that hangs past the deadline", func(t *testing.T) {
		saved := ext.Timeouts[ext.StageOutbound]
		ext.Timeouts[ext.StageOutbound] = 500 * time.Millisecond
		t.Cleanup(func() { ext.Timeouts[ext.StageOutbound] = saved })
		gh, home := mergeFixture(t, true, nil, "--body="+clean)
		bin := t.TempDir()
		proctest.InstallAt(t, bin, "stack-hang", proctest.Scenario{Responses: []proctest.Response{
			{Stdout: `{"status":"pass"}`, Sleep: time.Minute},
		}})
		t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+bin)
		writeFile(t, filepath.Join(home, ".config", "stack", "config.toml"),
			[]byte("[merge]\nmessage_checks = [\"hang\", \"public\"]\n"))
		res := runIn(t, home, nil, "pr", "merge", "7", prHead[:9], "--merge", "--body", clean)
		want := "stack pr merge: outbound could not check the message: hang: timed out"
		if res.code != exitRefused || !strings.Contains(res.stderr, want) ||
			!strings.Contains(res.stderr, "public: not started") {
			t.Errorf("pr merge = %+v, want exit 1 with %q", res, want)
		}
		if calls := proctest.Calls(t, gh); len(calls) != 0 {
			t.Errorf("gh calls = %+v, want none", calls)
		}
	})
}

// mergeFixture is fakeGh with a PATH of the fake gh, a fake git that finds the fixture's top level,
// stack-public when public is set, and a fake `stack-<name>` answering each of fakes. It returns
// the fake gh and a home that is also HOME, so the extensions read no real file.
func mergeFixture(
	t *testing.T, public bool, fakes map[string]string, mergeArgs ...string,
) (string, string) {
	t.Helper()
	gh := fakeGh(t, prHead, mergeArgs...)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	proctest.InstallAt(t, bin, "git", proctest.Scenario{Responses: []proctest.Response{{
		Args: []string{"rev-parse", "--show-toplevel"}, Stdout: filepath.ToSlash(cwd) + "\n",
	}}})
	for name, stdout := range fakes {
		proctest.InstallAt(t, bin, "stack-"+name,
			proctest.Scenario{Responses: []proctest.Response{{Stdout: stdout}}})
	}
	path := []string{filepath.Dir(gh), bin}
	if public {
		path = append(path, filepath.Dir(builtPublic))
	}
	t.Setenv("PATH", strings.Join(path, string(os.PathListSeparator)))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return gh, home
}
