package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestChars(t *testing.T) {
	t.Parallel()
	if got := chars("a😀\xff"); got != 4 { // 1 + a surrogate pair + U+FFFD
		t.Errorf("chars = %d, want 4", got)
	}
	for n, want := range map[int]string{0: "", 1: "a", 2: "a", 3: "a😀", 4: "a😀b"} {
		if got := prefix("a😀b", n); got != want {
			t.Errorf("prefix(%d) = %q, want %q", n, got, want)
		}
	}
}

// A later pass with a larger limit must not shrink a prefix placed by an earlier one.
func TestPlaceNeverShrinks(t *testing.T) {
	t.Parallel()
	p := tagged("t", "", strings.Repeat("x", 1000), "k")
	reserved := p.fit(Budget, 600)
	a := &allocator{room: cost(reserved)}
	a.place(p, 600)
	a.place(p, blockLimit)
	if p.out != reserved || a.room != 0 {
		t.Fatalf("out = %q (room %d), want the 600-char prefix kept", squash(p.out), a.room)
	}
}

// The budget counts UTF-16 units, as Claude Code does, and cuts at code-point boundaries.
func TestBudgetUTF16(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.rules("RULES")
	f.open("demo", map[string]string{"state.md": "DEMO STATE"})
	in := compactIn(testID)
	in.TranscriptPath = f.transcript(userLine(marker + "\n" + strings.Repeat("😀", 5200) +
		"\nBRIEF TAIL"))
	ctx, _ := f.start(in) // fails when over Budget UTF-16 units
	mustContain(t, ctx, "RULES", " chars]\n</original-brief>")
	if presence(ctx, filepath.Join(f.kitDir("demo"), "state.md")) == "" {
		t.Fatal("kit state disappeared")
	}
	if !utf8.ValidString(ctx) || strings.ContainsRune(ctx, utf8.RuneError) {
		t.Fatal("cut inside a code point")
	}
}

// presence returns, for a kit file, its tag or its omitted marker, whichever ctx holds, or "".
func presence(ctx, path string) string {
	for _, s := range []string{
		"<orchestration-state path=" + strconv.Quote(path) + ">\n",
		"[state omitted: context limit: " + path + "]",
	} {
		if strings.Contains(ctx, s) {
			return s
		}
	}
	return ""
}

// With several kits, every kit file keeps its start or at least a marker naming it.
func TestMultiKitReservation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		rules, brief int
		wantPrefix   bool // every file keeps its first 600 chars
	}{
		{"all prefixes fit", 1000, 2000, true},
		{"not every prefix fits", 2000, 4500, false},
		{"rules take nearly all", 8500, 4500, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.rules(strings.Repeat("r", tt.rules))
			in := compactIn(testID)
			in.TranscriptPath = f.transcript(
				userLine(marker + "\n" + strings.Repeat("_", tt.brief)),
			)
			files := map[string]string{"NORTH.md": "N", "state.md": "S"}
			for _, s := range []string{"a", "b", "c"} {
				kitFiles := map[string]string{}
				for name, c := range files {
					kitFiles[name] = strings.Repeat(s+c, 1450) // 2,900 chars
				}
				f.open(s, kitFiles)
			}
			ctx, _ := f.start(in)
			if !strings.Contains(ctx, "<original-brief>") &&
				!strings.Contains(ctx, "[brief omitted: context limit]") {
				t.Fatal("brief disappeared")
			}
			for _, s := range []string{"a", "b", "c"} {
				for _, name := range []string{"NORTH.md", "state.md"} {
					path := filepath.Join(f.kitDir(s), name)
					got := presence(ctx, path)
					if got == "" {
						t.Fatalf("%s/%s disappeared:\n%s", s, name, squash(ctx))
					}
					if tt.wantPrefix {
						mustContain(t, ctx, got+strings.Repeat(s+files[name], 300))
					}
				}
			}
		})
	}
}

// The `.kits locked` and brief markers are reserved before any text is placed.
func TestReservedMarkers(t *testing.T) {
	t.Parallel()
	t.Run("kits locked after a long brief", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules("RULES")
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		lock := flock.New(paths.SessionLockFile(f.home, testID))
		if err := lock.Lock(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Close() }()
		in := compactIn(testID)
		in.TranscriptPath = f.transcript(userLine(marker + "\n" + strings.Repeat("b", 12_000)))
		ctx, _ := f.start(in)
		mustContain(t, ctx, " chars]\n</original-brief>", "[kits omitted: .kits locked]")
	})
	t.Run("brief after rules that nearly fill the budget", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules(strings.Repeat("r", 9200))
		in := compactIn(testID)
		in.TranscriptPath = f.transcript(userLine(marker + "\nBRIEF"))
		ctx, _ := f.start(in)
		mustContain(t, ctx, "</worker-rules>", "[brief omitted: context limit]")
	})
}

// A relative `[kit] root` is taken against the config file's dir, not the working directory.
func TestRelativeKitRoot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Kit.Root = "kits"
	dir := filepath.Join(f.home, ".config", "stack", "kits", "demo")
	write(t, filepath.Join(dir, "state.md"), "DEMO STATE")
	f.open("demo", nil) // records the slug; the default-root dir it also creates is not read
	ctx, _ := f.start(compactIn(testID))
	mustContain(t, ctx, stateTag(filepath.Join(dir, "state.md"))+"DEMO STATE")
}

func TestBoards(t *testing.T) {
	t.Parallel()
	installBoard := func(t *testing.T, kitDir string, r proctest.Response) string {
		t.Helper()
		bin := filepath.Join(kitDir, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		return proctest.InstallAt(t, bin, "board", proctest.Scenario{
			Responses: []proctest.Response{r},
		})
	}

	t.Run("no budget left: not launched", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules(strings.Repeat("r", 12_000))
		f.open("demo", map[string]string{"state.md": "DEMO STATE"})
		board := installBoard(t, f.kitDir("demo"), proctest.Response{Sleep: time.Minute})
		in := compactIn(testID)
		in.AgentType = "opus-worker"
		start := time.Now()
		ctx, _ := f.start(in)
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("compact took %v", d)
		}
		if calls := proctest.Calls(t, board); len(calls) != 0 {
			t.Fatalf("board launched: %+v", calls)
		}
		mustContain(t, ctx, "</worker-rules>")
	})

	t.Run("leading repeat of another kit's state dropped", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		other := strings.Repeat("B", 2500)
		f.open("a", map[string]string{"state.md": "A STATE"})
		f.open("b", map[string]string{"state.md": other})
		board := installBoard(
			t,
			f.kitDir("a"),
			proctest.Response{Stdout: other + "\nLIVE: 3 PRs\n"},
		)
		ctx, _ := f.start(compactIn(testID))
		mustContain(t, ctx, "<orchestration-board path="+strconv.Quote(board)+
			">\nLIVE: 3 PRs\n</orchestration-board>")
	})
}
