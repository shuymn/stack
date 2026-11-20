package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc/proctest"
)

const marker = "Follow the body of ~/.claude/agents/opus-worker.md"

// stateTag is the opening tag of a kit state block for path.
func stateTag(path string) string {
	return "<orchestration-state path=" + strconv.Quote(path) + ">\n"
}

// C18: every source exports the id when CLAUDE_ENV_FILE is provided; only compact prints.
func TestExport(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"startup", "resume", "clear", "compact", "fork", "future"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			envFile := filepath.Join(t.TempDir(), "env")
			f.env[EnvFile] = envFile
			in := hookio.Input{SessionID: "Sess-1", Source: source}
			for range 2 {
				if ctx, errs := f.start(in); ctx != "" || errs != nil {
					t.Fatalf("start = %q, %v; want no output", ctx, errs)
				}
			}
			data, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			line := "export STACK_SESSION_ID='Sess-1'\n"
			if string(data) != line+line {
				t.Fatalf("env file = %q", data)
			}
			if got := sourceEnv(t, envFile); got != "Sess-1" && got != "<no bash>" {
				t.Fatalf("sourced STACK_SESSION_ID = %q", got)
			}
		})
	}

	t.Run("invalid id writes nothing", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		envFile := filepath.Join(t.TempDir(), "env")
		f.env[EnvFile] = envFile
		for _, id := range []string{"", "a b", "x';rm -rf ~;'"} {
			f.start(hookio.Input{SessionID: id, Source: "startup"})
		}
		if _, err := os.Stat(envFile); !os.IsNotExist(err) {
			t.Fatalf("env file written: %v", err)
		}
	})

	t.Run("no env file still restores a worker on compact", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules("RULES")
		in := compactIn("s1")
		in.AgentType = "opus-worker"
		ctx, _ := f.start(in)
		mustContain(t, ctx, "<worker-rules", "RULES")
	})
}

// sourceEnv sources file in bash and returns STACK_SESSION_ID, or "<no bash>". Windows is
// skipped: the first bash on PATH there may be WSL's, which cannot read the file's path.
func sourceEnv(t *testing.T, file string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		return "<no bash>"
	}
	out, err := exec.Command(bash, "-c", `. "$1" && printf %s "$STACK_SESSION_ID"`, "_",
		filepath.ToSlash(file)).Output()
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	return string(out)
}

// C19: compact renders the kits of that id only, without extensions. Not parallel: one case
// changes PATH.
func TestCompact(t *testing.T) {
	t.Run("resume then compact restores; fork and close do not", func(t *testing.T) {
		f := newFixture(t)
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		statePath := filepath.Join(f.kitDir("demo"), "state.md")

		if ctx, _ := f.start(hookio.Input{SessionID: "s1", Source: "resume"}); ctx != "" {
			t.Fatalf("resume printed %q", ctx)
		}
		ctx, errs := f.start(compactIn("s1"))
		if errs != nil {
			t.Fatalf("errs = %v", errs)
		}
		if want := stateTag(statePath) + "DEMO STATE\n</orchestration-state>\n"; ctx != want {
			t.Fatalf("compact = %q, want %q", ctx, want)
		}
		mustLack(t, ctx, "worker-rules")

		// A fork starts a new id, which has no kits.
		if ctx, _ := f.start(hookio.Input{SessionID: "s2", Source: "fork"}); ctx != "" {
			t.Fatalf("fork printed %q", ctx)
		}
		if ctx, _ := f.start(compactIn("s2")); ctx != "" {
			t.Fatalf("compact of forked id = %q, want nothing", ctx)
		}

		f.close("demo")
		if ctx, _ := f.start(compactIn("s1")); ctx != "" {
			t.Fatalf("compact after close = %q, want nothing", ctx)
		}
	})

	t.Run("lock held omits kits and keeps the worker part", func(t *testing.T) {
		f := newFixture(t)
		f.rules("RULES")
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		lock := flock.New(paths.SessionLockFile(f.home, "s1"))
		if err := lock.Lock(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Close() }()

		in := compactIn("s1")
		in.TranscriptPath = f.transcript(userLine(marker + "\nBRIEF"))
		ctx, _ := f.start(in)
		mustContain(t, ctx, "RULES", "<original-brief>\n"+marker+"\nBRIEF\n</original-brief>",
			"[kits omitted: .kits locked]")
		mustLack(t, ctx, "DEMO STATE")
	})

	t.Run("no extension runs; board, handover once", func(t *testing.T) {
		f := newFixture(t)
		bin := t.TempDir()
		ext := proctest.InstallAt(t, bin, "stack-jev-router", proctest.Scenario{
			Responses: []proctest.Response{{Stdout: `{"status":"pass","context":"EXT CONTEXT"}`}},
		})
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

		f.open("a", map[string]string{"state.md": "A STATE"})
		f.open("b", map[string]string{"NORTH.md": "B NORTH", "state.md": "B STATE"})
		f.open("gone", nil)
		if err := os.RemoveAll(f.kitDir("gone")); err != nil {
			t.Fatal(err)
		}
		boardDir := filepath.Join(f.kitDir("a"), "bin")
		if err := os.MkdirAll(boardDir, 0o700); err != nil {
			t.Fatal(err)
		}
		proctest.InstallAt(t, boardDir, "board",
			proctest.Scenario{Responses: []proctest.Response{{Stdout: "A STATE\nA BOARD\n"}}})
		write(t, filepath.Join(f.home, ".config", "stack", "handover", "t3.md"), "HANDOVER")
		f.env["T3CODE_THREAD_ID"] = "thread"

		ctx, _ := f.start(compactIn("s1"))
		mustContain(t, ctx, "A STATE", "<orchestration-board", "A BOARD", "B NORTH", "B STATE",
			"<orchestration-handover", "HANDOVER")
		if n := strings.Count(ctx, "HANDOVER"); n != 1 {
			t.Fatalf("handover appears %d times:\n%s", n, ctx)
		}
		mustLack(t, ctx, "EXT CONTEXT", f.kitDir("gone"))
		if calls := proctest.Calls(t, ext); len(calls) != 0 {
			t.Fatalf("extension called: %+v", calls)
		}
	})
}

// C20: worker detection and the budget.
func TestWorker(t *testing.T) {
	t.Parallel()

	t.Run("signals", func(t *testing.T) {
		t.Parallel()
		worktree := filepath.Join(t.TempDir(), "repo-worktrees", "agent-x")
		tests := []struct {
			name      string
			prompt    string
			cwd       string
			agentType string
			worker    bool
		}{
			{"marker line", "Packet\n" + marker + "\nmore", "", "", true},
			{"marker after CRLF", "Packet\r\n" + marker + "\r\n", "", "", true},
			{"agent worktree", "plain", worktree, "", true},
			{"agent type", "plain", "", "opus-worker", true},
			{"quoted marker", "see: " + marker, "", "", false},
			{
				"plain worktree",
				"plain",
				filepath.Join(t.TempDir(), "repo-worktrees", "agent-"),
				"",
				false,
			},
			{"other agent type", "plain", "", "planner", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				f.rules("---\r\nname: w\r\n---\r\nRULES BODY\r\n")
				in := compactIn("s1")
				in.TranscriptPath = f.transcript(userLine(tt.prompt))
				in.CWD, in.AgentType = tt.cwd, tt.agentType
				ctx, _ := f.start(in)
				if !tt.worker {
					if ctx != "" {
						t.Fatalf("non-worker got %q", ctx)
					}
					return
				}
				want := workerHeader + "<worker-rules path=" + strconv.Quote(f.rulesPath()) +
					">\nRULES BODY\n</worker-rules>\n\n<original-brief>\n" + tt.prompt +
					"\n</original-brief>\n"
				if ctx != want {
					t.Fatalf("context =\n%q\nwant\n%q", ctx, want)
				}
			})
		}
	})

	t.Run("transcript forms", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name, line, eol, brief string
		}{
			{"string", userLine(marker + "\nS"), "\n", marker + "\nS"},
			{
				"block array",
				`{"type":"user","message":{"content":[{"type":"text","text":"` + marker +
					`"},{"type":"text","text":"B"}]}}`,
				"\n", marker + "\n\nB",
			},
			{
				"tool_result first",
				`{"type":"user","message":{"content":[{"type":"tool_result","content":"r"}]}}` +
					"\n" + userLine(marker+"\nT"),
				"\n", marker + "\nT",
			},
			{"CRLF", userLine(marker + "\nC"), "\r\n", marker + "\nC"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				f.rules("R")
				path := filepath.Join(t.TempDir(), "t.jsonl")
				write(t, path, strings.ReplaceAll(tt.line, "\n", tt.eol)+tt.eol)
				in := compactIn("s1")
				in.TranscriptPath = path
				ctx, _ := f.start(in)
				mustContain(t, ctx, "<original-brief>\n"+tt.brief+"\n</original-brief>\n")
			})
		}
	})

	t.Run("rules and long brief whole, kit cut", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rules := strings.Repeat("r", 3000)
		brief := marker + "\n" + strings.Repeat("b", 4000)
		f.rules(rules)
		f.open("demo", map[string]string{"state.md": strings.Repeat("s", 2900)})
		in := compactIn("s1")
		in.TranscriptPath = f.transcript(userLine(brief))
		ctx, _ := f.start(in)
		mustContain(
			t,
			ctx,
			rules+"\n</worker-rules>",
			"<original-brief>\n"+brief+"\n</original-brief>",
			stateTag(
				filepath.Join(f.kitDir("demo"), "state.md"),
			),
			" chars]\n</orchestration-state>",
		)
	})

	t.Run("brief over budget", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		rules := strings.Repeat("r", 1000)
		f.rules(rules)
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		in := compactIn("s1")
		in.TranscriptPath = f.transcript(userLine(marker + "\n" + strings.Repeat("b", 12_000)))
		ctx, _ := f.start(in)
		mustContain(t, ctx, rules+"\n</worker-rules>", "<original-brief>\n"+marker,
			" chars]\n</original-brief>\n")
		mustLack(t, ctx, "DEMO STATE")
		if n := len(ctx); n < Budget-100 {
			t.Fatalf("context is %d chars; the cut brief should fill the budget", n)
		}
	})

	t.Run("rules over budget", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules(strings.Repeat("r", 12_000))
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		in := compactIn("s1")
		in.TranscriptPath = f.transcript(userLine(marker + "\nBRIEF"))
		ctx, _ := f.start(in)
		if !strings.HasPrefix(ctx, workerHeader+"<worker-rules") ||
			!strings.HasSuffix(ctx, " chars]\n</worker-rules>\n") {
			t.Fatalf("context = %q", ctx)
		}
		mustLack(t, ctx, "BRIEF", "DEMO STATE", "omitted")
	})

	t.Run("broken transcript keeps kits", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules("RULES")
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		in := compactIn("s1")
		in.AgentType = "opus-worker"
		in.TranscriptPath = f.transcript("{")
		ctx, errs := f.start(in)
		mustContain(t, ctx, "RULES", "DEMO STATE")
		mustLack(t, ctx, "original-brief")
		if len(errs) != 1 {
			t.Fatalf("errs = %v, want the transcript error", errs)
		}
	})

	t.Run("unreadable kit keeps brief", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules("RULES")
		f.open("demo", map[string]string{"NORTH.md": "NORTH"})
		statePath := filepath.Join(f.kitDir("demo"), "state.md")
		if err := os.Mkdir(statePath, 0o700); err != nil { // reading a directory fails everywhere
			t.Fatal(err)
		}
		in := compactIn("s1")
		in.TranscriptPath = f.transcript(userLine(marker + "\nBRIEF"))
		ctx, _ := f.start(in)
		mustContain(t, ctx, "RULES", "BRIEF", "NORTH", "[unreadable: "+statePath+"]")
	})

	t.Run("unreadable rules keep brief", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		in := compactIn("s1")
		in.TranscriptPath = f.transcript(userLine(marker + "\nBRIEF"))
		ctx, _ := f.start(in)
		mustContain(t, ctx, "[unreadable: "+f.rulesPath()+"]", "BRIEF")
	})
}

// With several kits, each kit's state files get their start before any kit gets more.
func TestMultiKitReservation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.rules(strings.Repeat("r", 2000))
	in := compactIn("s1")
	in.TranscriptPath = f.transcript(userLine(marker + "\n" + strings.Repeat("b", 4500)))
	slugs := []string{"a", "b", "c"}
	for _, s := range slugs {
		f.open(s, map[string]string{"state.md": strings.Repeat(s, 2900)})
	}
	ctx, _ := f.start(in)
	for _, s := range slugs {
		mustContain(t, ctx, stateTag(filepath.Join(f.kitDir(s), "state.md"))+
			strings.Repeat(s, reserveChars))
	}
	mustLack(t, ctx, "omitted")
}
