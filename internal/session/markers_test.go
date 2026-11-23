package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/hookio"
	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/proc/proctest"
)

// stateBlock is the whole rendering of a kit state file holding body.
func stateBlock(path, body string) string {
	return stateTag(path) + body + "\n</orchestration-state>"
}

// rulesLeaving writes worker rules whose whole block leaves exactly room in the budget once it
// and others, the whole renderings of the pieces placed before the part under test, are placed,
// and returns the input of a worker's compact.
func rulesLeaving(f *fixture, room int, others ...string) hookio.Input {
	f.t.Helper()
	pc := tagged("worker-rules", f.rulesPath(), "", "worker rules")
	n := Budget + 1 - room - cost(workerHeader+pc.head+pc.tail)
	for _, o := range others {
		n -= cost(o)
	}
	if n < 1 {
		f.t.Fatalf("no room for the rules: %d chars", n)
	}
	f.rules(strings.Repeat("r", n))
	in := compactIn(testID)
	in.AgentType = "opus-worker"
	return in
}

// installBoardAt installs a fake board into the kit dir answering resp and returns its path.
func installBoardAt(t *testing.T, kitDir string, resp proctest.Response) string {
	t.Helper()
	bin := filepath.Join(kitDir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return proctest.InstallAt(t, bin, "board",
		proctest.Scenario{Responses: []proctest.Response{resp}})
}

// TestRestoreMarkers covers the parts and markers of compact restore (phase2-core.md §4.2 step 6)
// that L3 of docs/design/kit-contracts.md §7.2 lists beyond the other compact tests.
func TestRestoreMarkers(t *testing.T) {
	t.Parallel()

	t.Run("items block after the state files, before the board", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.open("demo", map[string]string{
			"NORTH.md": "N",
			"state.md": "S",
			"attention/items.json": `{"items": [{"role": "orchestrator", "rule": "r1"}, ` +
				`{"role": "worker", "rule": "w"}, {"role": "all", "rule": "r2"}]}`,
		})
		dir := f.kitDir("demo")
		board := installBoardAt(t, dir, proctest.Response{Stdout: "LIVE\n"})
		ctx, errs := f.start(compactIn(testID))
		want := stateBlock(filepath.Join(dir, "NORTH.md"), "N") + "\n\n" +
			stateBlock(filepath.Join(dir, "state.md"), "S") + "\n\n" +
			"<orchestrator-attention-items path=" +
			strconv.Quote(filepath.Join(dir, "attention", "items.json")) + ">\n- r1\n- r2\n" +
			"</orchestrator-attention-items>\n\n" +
			"<orchestration-board path=" + strconv.Quote(board) + ">\nLIVE\n" +
			"</orchestration-board>\n"
		if ctx != want || errs != nil {
			t.Fatalf("context =\n%s\nwant\n%s\nerrs %v", ctx, want, errs)
		}
	})

	t.Run("kits omitted: .kits unreadable", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules("RULES")
		// A directory in place of the file cannot be read as one on any OS.
		if err := os.MkdirAll(paths.SessionKitsFile(f.home, testID), 0o700); err != nil {
			t.Fatal(err)
		}
		in := compactIn(testID)
		in.AgentType = "opus-worker"
		ctx, errs := f.start(in)
		if !strings.HasSuffix(ctx, "</worker-rules>\n\n[kits omitted: .kits unreadable]\n") ||
			len(errs) != 1 {
			t.Fatalf("context = %q, errs %v; want the worker part, then the marker", ctx, errs)
		}
	})

	t.Run("board omitted: context limit, not launched", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.open("demo", map[string]string{"state.md": "S"})
		state := stateBlock(filepath.Join(f.kitDir("demo"), "state.md"), "S")
		board := installBoardAt(t, f.kitDir("demo"), proctest.Response{Sleep: time.Minute})
		omitted := "[board omitted: context limit: " + board + "]"
		ctx, _ := f.start(rulesLeaving(f, cost(omitted), state))
		if !strings.HasSuffix(ctx, "</worker-rules>\n\n"+state+"\n\n"+omitted+"\n") {
			t.Fatalf("context = %q\nwant suffix %q", squash(ctx), omitted)
		}
		if calls := proctest.Calls(t, board); len(calls) != 0 {
			t.Fatalf("board launched: %+v", calls)
		}
	})

	t.Run("board skipped: deadline", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.open("a", map[string]string{"state.md": "A STATE"})
		f.open("b", map[string]string{"state.md": "B STATE"})
		installBoardAt(t, f.kitDir("a"), proctest.Response{Sleep: time.Minute})
		late := installBoardAt(t, f.kitDir("b"), proctest.Response{Stdout: "B BOARD"})
		// The first board outlives the hook's context, so its deadline has passed at the second.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		f.ctx = ctx
		got, _ := f.start(compactIn(testID))
		if !strings.HasSuffix(
			got,
			"B STATE\n</orchestration-state>\n\n[board skipped: deadline]\n",
		) {
			t.Fatalf("context = %q", got)
		}
		if calls := proctest.Calls(t, late); len(calls) != 0 {
			t.Fatalf("board launched after the deadline: %+v", calls)
		}
	})

	t.Run("items omitted: context limit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.open("demo", map[string]string{
			"state.md":             "S",
			"attention/items.json": `{"items": [{"role": "all", "rule": "RULE"}]}`,
		})
		dir := f.kitDir("demo")
		state := stateBlock(filepath.Join(dir, "state.md"), "S")
		omitted := "[items omitted: context limit: " +
			filepath.Join(dir, "attention", "items.json") + "]"
		ctx, _ := f.start(rulesLeaving(f, cost(omitted), state))
		if !strings.HasSuffix(ctx, "</worker-rules>\n\n"+state+"\n\n"+omitted+"\n") {
			t.Fatalf("context = %q\nwant suffix %q", squash(ctx), omitted)
		}
	})

	t.Run("handover omitted: context limit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		handover := filepath.Join(f.home, "handover.md")
		write(t, handover, "HANDOVER")
		f.cfg.Handover = []config.HandoverEntry{{Env: "T3CODE_THREAD_ID", Path: handover}}
		f.env["T3CODE_THREAD_ID"] = "thread"
		f.open("demo", map[string]string{"state.md": "S"})
		state := stateBlock(filepath.Join(f.kitDir("demo"), "state.md"), "S")
		omitted := "[handover omitted: context limit: " + handover + "]"
		ctx, _ := f.start(rulesLeaving(f, cost(omitted), state))
		if !strings.HasSuffix(ctx, "</worker-rules>\n\n"+state+"\n\n"+omitted+"\n") {
			t.Fatalf("context = %q\nwant suffix %q", squash(ctx), omitted)
		}
	})

	t.Run("worker rules omitted: context limit", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.rules("RULES BODY")
		omitted := "[worker rules omitted: context limit: " + f.rulesPath() + "]"
		// Empty kits reserve their new-kit markers before the rules are placed; their slugs are
		// sized so the markers leave exactly the room of the rules' omitted marker.
		newKit := func(slug string) string {
			return "[new kit: " + f.kitDir(slug) + "; write state.md here]"
		}
		c1 := cost(newKit("x")) // a slug of length L costs c1 + L - 1
		need := Budget + 1 - cost(omitted)
		k := (need + c1 + 62) / (c1 + 63) // kits of at most 64 chars, rounded up
		lengths := need - k*(c1-1)
		markers := make([]string, 0, k)
		for i := range k {
			n := lengths / k
			if i < lengths%k {
				n++
			}
			if n < 4 || n > 64 {
				t.Fatalf("slug length %d out of range", n)
			}
			slug := fmt.Sprintf("k%03d", i) + strings.Repeat("x", n-4)
			f.open(slug, nil)
			markers = append(markers, newKit(slug))
		}
		in := compactIn(testID)
		in.AgentType = "opus-worker"
		ctx, _ := f.start(in)
		if want := omitted + "\n\n" + strings.Join(markers, "\n\n") + "\n"; ctx != want {
			t.Fatalf("context = %q\nwant %q", squash(ctx), squash(want))
		}
	})
}

// TestCompactEntryMarkers covers the entry markers of compact restore that L3 lists, and C19's
// empty `[stages].entry`. Not parallel: it changes PATH.
func TestCompactEntryMarkers(t *testing.T) {
	t.Run("entry extensions skipped: an invalid name", func(t *testing.T) {
		bin := pathDir(t)
		demo := fakeEntry(t, bin, "demo", passWith("ENTRY CTX"))
		f := newFixture(t)
		f.cfg.Stages.Entry = []string{"demo", "../bad"}
		f.open("demo", map[string]string{"state.md": "S"})
		ctx, _ := f.start(compactIn(testID))
		want := stateBlock(filepath.Join(f.kitDir("demo"), "state.md"), "S") + "\n\n" +
			`[entry extensions skipped: invalid extension name: "../bad" ` +
			"(want ^[a-z0-9][a-z0-9-]*$)]\n"
		if ctx != want {
			t.Fatalf("context = %q\nwant %q", ctx, want)
		}
		if calls := proctest.Calls(t, demo); len(calls) != 0 {
			t.Fatalf("extension called: %+v", calls)
		}
	})

	t.Run("C19: empty [stages].entry invokes no extension", func(t *testing.T) {
		bin := pathDir(t)
		demo := fakeEntry(t, bin, "demo", passWith("ENTRY CTX"))
		f := newFixture(t) // [stages].entry is empty
		f.open("demo", map[string]string{"state.md": "S"})
		ctx, _ := f.start(compactIn(testID))
		if want := stateBlock(
			filepath.Join(f.kitDir("demo"), "state.md"),
			"S",
		) + "\n"; ctx != want {
			t.Fatalf("context = %q\nwant %q", ctx, want)
		}
		if calls := proctest.Calls(t, demo); len(calls) != 0 {
			t.Fatalf("extension called: %+v", calls)
		}
	})
}
