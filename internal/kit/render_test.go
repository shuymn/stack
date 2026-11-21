package kit

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/proc"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// newKit returns an absolute kit dir holding the given files.
func newKit(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "kits", "demo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		write(t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
	return dir
}

func render(t *testing.T, r Renderer, dir string) string {
	t.Helper()
	if r.Runner == nil {
		r.Runner = proc.Exec{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), BoardDeadline)
	defer cancel()
	return Join(r.Render(ctx, dir))
}

// block is the expected rendering of a tagged block; the path is quoted as Go quotes it, so
// Windows backslashes appear escaped.
func block(tag, path, body string) string {
	return "<" + tag + " path=" + strconv.Quote(path) + ">\n" + body + "\n</" + tag + ">"
}

func TestRenderStateAndItems(t *testing.T) {
	t.Parallel()
	dir := newKit(t, map[string]string{
		"NORTH.md": "north\n",
		"state.md": "state\r\n",
		"attention/items.json": `{"items": [
			{"role": "orchestrator", "rule": "r1"},
			{"role": "worker", "rule": "w"},
			{"role": "all", "rule": "r2"},
			{"role": "all", "rule": 3},
			"not an object"
		]}`,
	})
	got := render(t, Renderer{}, dir)
	want := block("orchestration-state", filepath.Join(dir, "NORTH.md"), "north") + "\n\n" +
		block("orchestration-state", filepath.Join(dir, "state.md"), "state") + "\n\n" +
		block("orchestrator-attention-items", filepath.Join(dir, "attention", "items.json"),
			"- r1\n- r2") + "\n"
	if got != want {
		t.Fatalf("render =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderNewKit(t *testing.T) {
	t.Parallel()
	dir := newKit(t, map[string]string{"attention/items.json": "{malformed"})
	got := render(t, Renderer{}, dir)
	if got != "[new kit: "+dir+"; write state.md here]\n" {
		t.Fatalf("new kit = %q; malformed items must be omitted", got)
	}
}

// TestRender covers C17: handover selection by `[handover]` order and unreadable parts as markers;
// and C12 of docs/design/core-stages.md: state files of any size render verbatim, with no size note.
func TestRender(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	write(t, filepath.Join(home, "h", "t3.md"), "T3 HOW-TO\n")
	write(t, filepath.Join(home, "h", "herdr.md"), "HERDR HOW-TO\n")
	relative := filepath.Join(home, ".config", "stack", "rel", "t3.md")
	write(t, relative, "RELATIVE HOW-TO")
	t3 := block("orchestration-handover", filepath.Join(home, "h", "t3.md"), "T3 HOW-TO")
	herdr := block("orchestration-handover", filepath.Join(home, "h", "herdr.md"), "HERDR HOW-TO")
	defaults := []config.HandoverEntry{
		{Env: "T3CODE_THREAD_ID", Path: "~/h/t3.md"},
		{Env: "HERDR_ENV", Path: "~/h/herdr.md"},
	}
	tests := []struct {
		name     string
		handover []config.HandoverEntry
		vars     map[string]string
		want     string // the handover part of the output; "" means none
	}{
		{
			"T3 beats herdr",
			defaults,
			map[string]string{"T3CODE_THREAD_ID": "t", "HERDR_ENV": "1"},
			t3,
		},
		{"HERDR_ENV=0 is none", defaults, map[string]string{"HERDR_ENV": "0"}, ""},
		{"HERDR_ENV=1 is herdr", defaults, map[string]string{"HERDR_ENV": "1"}, herdr},
		{"nothing set", defaults, nil, ""},
		{
			"file order decides",
			[]config.HandoverEntry{defaults[1], defaults[0]},
			map[string]string{"T3CODE_THREAD_ID": "t", "HERDR_ENV": "1"},
			herdr,
		},
		{
			"relative path is under the config dir",
			[]config.HandoverEntry{{Env: "T3CODE_THREAD_ID", Path: filepath.Join("rel", "t3.md")}},
			map[string]string{"T3CODE_THREAD_ID": "t"},
			block("orchestration-handover", relative, "RELATIVE HOW-TO"),
		},
		{
			"missing handover file is a marker",
			[]config.HandoverEntry{{Env: "T3CODE_THREAD_ID", Path: "~/h/none.md"}},
			map[string]string{"T3CODE_THREAD_ID": "t"},
			"[unreadable: " + filepath.Join(home, "h", "none.md") + "]",
		},
	}
	dir := newKit(t, map[string]string{"state.md": "s"})
	state := block("orchestration-state", filepath.Join(dir, "state.md"), "s")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := state + "\n"
			if tt.want != "" {
				want = state + "\n\n" + tt.want + "\n"
			}
			r := Renderer{Home: home, Getenv: env(tt.vars), Handover: tt.handover}
			if got := render(t, r, dir); got != want {
				t.Fatalf("render =\n%s\nwant\n%s", got, want)
			}
		})
	}

	t.Run("large state files render verbatim", func(t *testing.T) {
		t.Parallel()
		north, state := strings.Repeat("n", 3001), strings.Repeat("s\n", 5000)+"end"
		dir := newKit(t, map[string]string{"NORTH.md": north, "state.md": state})
		want := block("orchestration-state", filepath.Join(dir, "NORTH.md"), north) + "\n\n" +
			block("orchestration-state", filepath.Join(dir, "state.md"), state) + "\n"
		if got := render(t, Renderer{}, dir); got != want {
			t.Fatalf("render =\n%.300s\nwant the state files verbatim", got)
		}
	})

	t.Run("dangling symlinks are markers", func(t *testing.T) {
		t.Parallel()
		dir := newKit(t, map[string]string{"NORTH.md": "north", "attention/keep": ""})
		state := filepath.Join(dir, "state.md")
		items := filepath.Join(dir, "attention", "items.json")
		if err := os.Symlink("missing.md", state); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Symlink("missing.json", items); err != nil {
			t.Fatal(err)
		}
		got := render(t, Renderer{}, dir)
		if !strings.Contains(got, "[unreadable: "+state+"]") ||
			!strings.Contains(got, "[unreadable: "+items+"]") || strings.Contains(got, "new kit") {
			t.Fatalf("render =\n%s", got)
		}
	})

	t.Run("out-of-kit symlink is a marker", func(t *testing.T) {
		t.Parallel()
		outside := filepath.Join(t.TempDir(), "secret.md")
		write(t, outside, "OUTSIDE")
		dir := newKit(t, map[string]string{"NORTH.md": "north"})
		if err := os.Symlink(outside, filepath.Join(dir, "state.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		got := render(t, Renderer{}, dir)
		if strings.Contains(got, "OUTSIDE") ||
			!strings.Contains(got, "[unreadable: "+filepath.Join(dir, "state.md")+"]") ||
			!strings.Contains(got, "north") {
			t.Fatalf("render =\n%s", got)
		}
	})

	t.Run("unreadable state file keeps the other blocks", func(t *testing.T) {
		t.Parallel()
		// A directory named state.md cannot be read as a file on any OS.
		dir := newKit(t, map[string]string{"NORTH.md": "north", "state.md/x": ""})
		got := render(t, Renderer{}, dir)
		if !strings.Contains(got, "[unreadable: "+filepath.Join(dir, "state.md")+"]") ||
			!strings.Contains(got, "north") {
			t.Fatalf("render =\n%s", got)
		}
	})

	t.Run("board not runnable is a marker", func(t *testing.T) {
		t.Parallel()
		name := "bin/board"
		if runtime.GOOS == "windows" {
			name = "bin/board.cmd"
		}
		dir := newKit(t, map[string]string{"state.md": "s", name: "echo hi"})
		got := render(t, Renderer{}, dir)
		want := "[board not runnable on this OS: " + filepath.Join(
			dir,
			filepath.FromSlash(name),
		) + "]"
		if !strings.Contains(got, want) || !strings.Contains(got, "<orchestration-state") {
			t.Fatalf("render =\n%s\nwant marker %s", got, want)
		}
	})
}

func installBoard(t *testing.T, dir string, r proctest.Response) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return proctest.InstallAt(t, bin, "board", proctest.Scenario{Responses: []proctest.Response{r}})
}

func TestRenderBoard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		resp proctest.Response
		want string
	}{
		{
			"strips a leading repeat of the state files",
			proctest.Response{Stdout: "north\n\nthe state\n\nlive: 3 PRs\n"},
			"live: 3 PRs",
		},
		{
			"strips a CRLF repeat",
			proctest.Response{Stdout: "north\r\nthe state\r\nlive\r\n"},
			"live",
		},
		{
			"keeps a line that only starts like the state",
			proctest.Response{Stdout: "northern: 3 PRs\n"},
			"northern: 3 PRs",
		},
		{
			"non-zero exit keeps output and adds a marker",
			proctest.Response{Stdout: "partial\n", Stderr: "warn\nboom\n", Exit: 3},
			"partial\n[board exit 3: boom]",
		},
		{"no output", proctest.Response{}, "[board exit 0: no output]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := newKit(t, map[string]string{"NORTH.md": "north", "state.md": "the state"})
			board := installBoard(t, dir, tt.resp)
			got := render(t, Renderer{}, dir)
			want := block("orchestration-board", board, tt.want) + "\n"
			if !strings.HasSuffix(got, want) {
				t.Fatalf("render =\n%s\nwant suffix\n%s", got, want)
			}
			calls := proctest.Calls(t, board)
			if len(calls) != 1 || len(calls[0].Args) != 0 {
				t.Fatalf("board calls = %+v, want one call without arguments", calls)
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		dir := newKit(t, map[string]string{"state.md": "s"})
		installBoard(t, dir, proctest.Response{Stdout: "late", Sleep: 10 * time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		got := Join(Renderer{Runner: proc.Exec{}}.Render(ctx, dir))
		if !strings.Contains(got, "[board timed out]") {
			t.Fatalf("render =\n%s", got)
		}
	})

	t.Run("deadline already passed launches nothing", func(t *testing.T) {
		t.Parallel()
		dir := newKit(t, map[string]string{"state.md": "s"})
		board := installBoard(t, dir, proctest.Response{Stdout: "live"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got := Join(Renderer{Runner: proc.Exec{}}.Render(ctx, dir))
		if !strings.HasSuffix(got, "\n[board skipped: deadline]\n") ||
			len(proctest.Calls(t, board)) != 0 {
			t.Fatalf("render =\n%s", got)
		}
	})

	t.Run("board runs in the kit dir", func(t *testing.T) {
		t.Parallel()
		dir := newKit(t, map[string]string{"state.md": "s"})
		installBoard(t, dir, proctest.Response{Stdout: "ok"})
		rec := &recorder{}
		_ = Renderer{Runner: rec}.Render(context.Background(), dir)
		if rec.cmd.Dir != dir {
			t.Fatalf("board dir = %q, want %q", rec.cmd.Dir, dir)
		}
	})
}

type recorder struct{ cmd proc.Cmd }

func (r *recorder) Run(_ context.Context, c proc.Cmd) (proc.Result, error) {
	r.cmd = c
	return proc.Result{Stdout: []byte("ok")}, nil
}

func (r *recorder) Stream(context.Context, proc.Cmd, func(io.Reader) error) error {
	return nil
}
