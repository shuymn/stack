package classify

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// step is one record of a neutral script, built as a transcript record or a T3 row.
type step struct {
	kind, text string
}

// Neutral kinds: owner turn, other prompt (a task notification, a server-created row), compaction
// (a summary, a /compact row), assistant text, tool call, tool result and the reply under test.
func so(t string) step { return step{"owner", t} }
func sp(t string) step { return step{"prompt", t} }
func sc() step         { return step{"compaction", "summary"} }

// scm is a compaction summary also marked isMeta, as Claude Code writes some.
func scm() step        { return step{"compaction-meta", "summary"} }
func st(t string) step { return step{"text", t} }
func tc() step         { return step{"tool", ""} }
func tr() step         { return step{"result", ""} }
func sr(t string) step { return step{"reply", t} }

// transcriptStep maps a step to a transcript record; task notifications stand for other prompts.
func transcriptStep(s step) map[string]any {
	switch s.kind {
	case "owner", "reply":
		return owner(s.text)
	case "prompt":
		return prompt("<task-notification>" + s.text + "</task-notification>")
	case "compaction":
		return summary(s.text)
	case "compaction-meta":
		r := summary(s.text)
		r["isMeta"] = true
		return r
	case "text":
		return text(s.text)
	case "tool":
		return toolCall()
	default:
		return toolResult()
	}
}

func t3Step(i int, s step) T3Row {
	switch s.kind {
	case "owner", "reply":
		return t3Owner("id"+strconv.Itoa(i), s.text)
	case "prompt":
		return t3Server("id"+strconv.Itoa(i), s.text)
	case "compaction", "compaction-meta":
		return t3Server("id"+strconv.Itoa(i), "/compact")
	case "text":
		return t3Text(s.text)
	case "tool":
		return t3Tool()
	default:
		return t3Agent("result")
	}
}

// reader builds a script into one source and returns the observe item IDs of its replies.
type reader struct {
	name  string
	build func(w *world, script []step) []string
}

var readers = []reader{
	{"transcript", func(w *world, script []step) []string {
		var ids []string
		for i, s := range script {
			line := w.transcript("p/s.jsonl", "work", transcriptStep(s))[0]
			if s.kind == "reply" {
				ids = append(ids, w.reply("p/s.jsonl", line))
			}
			_ = i
		}
		return ids
	}},
	{"t3", func(w *world, script []step) []string {
		var ids []string
		for i, s := range script {
			idx := w.thread("th:1:x", "work", t3Step(i, s))[0]
			if s.kind == "reply" {
				ids = append(ids, w.t3Reply("th:1:x", idx))
			}
		}
		return ids
	}},
}

// expect is what a reply resolves to.
type expect struct {
	reason, owner, assistant, opener string
}

// TestResolve is C9's resolution table, in both readers: what each reply resolves to, asserted on
// the exact owner and assistant sent, or the reason it is unresolved.
func TestResolve(t *testing.T) {
	for _, c := range []struct {
		name   string
		script []step
		want   []expect
	}{
		{
			"a tool result between the assistant text and the reply",
			[]step{so("task A"), st("closing A"), tr(), sr("fix one")},
			[]expect{{"", "task A", "closing A", OpenerOwner}},
		},
		{
			"the closing message is the last run of text before the tool calls and results",
			[]step{so("task"), st("early"), tc(), tr(), st("mid1"), st("mid2"), tc(), tr(), sr("fix one")},
			[]expect{{"", "task", "mid1\n\nmid2", OpenerOwner}},
		},
		{
			"a compaction between a finished turn and the reply",
			[]step{so("task"), st("done"), sc(), sr("fix one")},
			[]expect{{reason: ReasonIntervening}},
		},
		{
			"a compaction opening the answered turn",
			[]step{sc(), st("resumed"), sr("fix one")},
			[]expect{{"", "", "resumed", OpenerCompaction}},
		},
		{
			"a marked compaction between a finished turn and the reply",
			[]step{so("task"), st("done"), scm(), sr("fix one")},
			[]expect{{reason: ReasonIntervening}},
		},
		{
			"a marked compaction opening the answered turn",
			[]step{scm(), st("resumed"), sr("fix one")},
			[]expect{{"", "", "resumed", OpenerCompaction}},
		},
		{
			"two owner turns in a row after a turn with text",
			[]step{so("task"), st("done"), so("second"), sr("fix one")},
			[]expect{{"", "task", "done", OpenerOwner}},
		},
		{
			"a tool-only turn followed by two owner replies",
			[]step{so("task"), tc(), tr(), sr("fix one"), sr("fix two")},
			[]expect{{reason: ReasonNoAssistantText}, {reason: ReasonNoAssistantText}},
		},
		{
			"the older turn's text is not borrowed by a tool-only turn",
			[]step{so("task"), st("older text"), so("task two"), tc(), tr(), sr("fix one")},
			[]expect{{reason: ReasonNoAssistantText}},
		},
		{
			"another prompt opening a turn",
			[]step{sp("server"), st("done"), sr("fix one")},
			[]expect{{"", "", "done", OpenerPrompt}},
		},
		{
			"a finished turn, then another prompt, then the reply",
			[]step{so("task"), st("done"), sp("server"), sr("fix one")},
			[]expect{{reason: ReasonIntervening}},
		},
		{
			"a source starting mid-turn",
			[]step{st("mid"), sr("fix one")},
			[]expect{{"", "", "mid", OpenerNone}},
		},
		{
			"text before a later tool call is still the closing run",
			[]step{so("task"), tc(), st("said"), tc(), tr(), sr("fix one")},
			[]expect{{"", "task", "said", OpenerOwner}},
		},
	} {
		for _, rd := range readers {
			t.Run(c.name+"/"+rd.name, func(t *testing.T) {
				w := newWorld(t)
				ids := rd.build(w, c.script)
				w.write()
				f := &fake{p: byPrefix}
				res := w.run(t, Env{Transport: f}, w.request())
				var replies []string
				for _, s := range c.script {
					if s.kind == "reply" {
						replies = append(replies, s.text)
					}
				}
				sent := map[string][2]string{}
				for _, b := range f.sent() {
					for _, it := range decodeBody(b).State.Items {
						sent[it.Reply] = [2]string{it.Owner, it.Assistant}
					}
				}
				for i, id := range ids {
					got := res.item(t, id)
					want := c.want[i]
					if want.reason != "" {
						if got.Status != StatusUnresolved || got.Reason != want.reason {
							t.Fatalf("%s: status %s reason %q, want unresolved %q", id, got.Status,
								got.Reason, want.reason)
						}
						if _, ok := sent[replies[i]]; ok {
							t.Fatalf("%s: an unresolved reply was sent", id)
						}
						continue
					}
					if got.Status != StatusClassified || got.Opener != want.opener {
						t.Fatalf("%s: %+v, want classified with opener %s", id, got, want.opener)
					}
					if o := sent[replies[i]]; o[0] != want.owner || o[1] != want.assistant {
						t.Fatalf("%s: sent owner %q assistant %q, want %q %q", id, o[0], o[1],
							want.owner, want.assistant)
					}
				}
			})
		}
	}
}

// TestResolveChanged: a reply edited since observe, a rewritten line and a missing record or
// file are unresolved with their reason.
func TestResolveChanged(t *testing.T) {
	t.Run("a rewritten transcript line", func(t *testing.T) {
		w := newWorld(t)
		w.transcript("p/s.jsonl", "work", owner("task"), text("done"))
		line := w.transcript("p/s.jsonl", "work", owner("fix one"))[0]
		id := w.reply("p/s.jsonl", line)
		w.files["p/s.jsonl"][line-1]["message"] = map[string]any{"content": "fix one, edited"}
		w.write()
		res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if it := res.item(
			t,
			id,
		); it.Status != StatusUnresolved ||
			it.Reason != ReasonRecordChanged {
			t.Fatalf("%+v", it)
		}
	})
	t.Run("a changed timestamp", func(t *testing.T) {
		w := newWorld(t)
		w.transcript("p/s.jsonl", "work", owner("task"), text("done"))
		line := w.transcript("p/s.jsonl", "work", owner("fix one"))[0]
		id := w.reply("p/s.jsonl", line)
		w.files["p/s.jsonl"][line-1]["timestamp"] = testNow.Add(-time.Hour).Format(time.RFC3339)
		w.write()
		res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if it := res.item(t, id); it.Reason != ReasonRecordChanged {
			t.Fatalf("%+v", it)
		}
	})
	t.Run("a missing file and a line past the end", func(t *testing.T) {
		w := newWorld(t)
		w.transcript("p/s.jsonl", "work", owner("task"), text("done"))
		line := w.transcript("p/s.jsonl", "work", owner("fix one"))[0]
		past := w.reply("p/s.jsonl", line)
		w.items[0].Where = "p/s.jsonl:99"
		w.items = append(w.items, ObserveItem{
			ID:      "gone",
			Signal:  signalOwnerReply,
			Where:   "p/none.jsonl:3",
			Excerpt: "x",
			TS:      iso(testNow),
		})
		w.write()
		res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		for _, id := range []string{past, "gone"} {
			if it := res.item(
				t,
				id,
			); it.Status != StatusUnresolved ||
				it.Reason != ReasonSourceGone {
				t.Fatalf("%s: %+v", id, it)
			}
		}
	})
	t.Run("lines are numbered as observe numbers them", func(t *testing.T) {
		w := newWorld(t)
		ls := w.transcript("p/s.jsonl", "work", owner("task"), text("done"), owner("fix one"))
		id := w.reply("p/s.jsonl", ls[2])
		// A malformed first line counts, so the reply is line 4; a final unterminated line is
		// not counted, so a locator naming it is gone.
		w.items[0].Where = "p/s.jsonl:4"
		w.items = append(w.items, ObserveItem{
			ID:      "tail",
			Signal:  signalOwnerReply,
			Where:   "p/s.jsonl:5",
			Excerpt: "x",
			TS:      iso(testNow),
		})
		w.write()
		w.writeFile(
			"p/s.jsonl",
			"not json\n"+readFile(t, w.root+"/p/s.jsonl")+`{"type":"user","unfinished`,
		)
		f := &fake{p: byPrefix}
		res := w.run(t, Env{Transport: f}, w.request())
		if it := res.item(t, id); it.Status != StatusClassified || it.Opener != OpenerOwner {
			t.Fatalf("%+v", it)
		}
		if it := res.item(t, "tail"); it.Reason != ReasonSourceGone {
			t.Fatalf("%+v", it)
		}
		if s := decodeBody(f.sent()[0]).State.Items[0]; s.Owner != "task" || s.Assistant != "done" {
			t.Fatalf("sent %+v", s)
		}
	})
}

// failingT3 fails the queries named in fail.
type failingT3 struct {
	T3

	fail map[string]error
}

func (f failingT3) Users(ctx context.Context, s, u time.Time) ([]T3Row, error) {
	if err := f.fail["users"]; err != nil {
		return nil, err
	}
	return f.T3.Users(ctx, s, u)
}

func (f failingT3) Thread(ctx context.Context, th string) ([]T3Row, error) {
	if err := f.fail["thread:"+th]; err != nil {
		return nil, err
	}
	return f.T3.Thread(ctx, th)
}

func (f failingT3) Cwds(ctx context.Context, ths []string) (map[string]string, error) {
	if err := f.fail["cwds"]; err != nil {
		return nil, err
	}
	return f.T3.Cwds(ctx, ths)
}

// TestResolveFailures: the locator query, a thread query and the cwd query each failing leave the
// outcomes §3.2 names; transcripts are unaffected by T3.
func TestResolveFailures(t *testing.T) {
	setup := func(t *testing.T) (*world, map[string]string) {
		t.Helper()
		w := newWorld(t)
		ids := map[string]string{}
		line := w.transcript("p/s.jsonl", "work", owner("task"), text("done"), owner("fix tx"))[2]
		ids["tx"] = w.reply("p/s.jsonl", line)
		for _, th := range []string{"a", "b"} {
			rows := w.thread(
				th,
				"work",
				t3Owner("t0", "task"),
				t3Text("done"),
				t3Owner("t1", "fix "+th),
			)
			ids[th] = w.t3Reply(th, rows[2])
		}
		w.write()
		return w, ids
	}
	boom := errors.New("sqlite3 failed: exit 1")
	t.Run("the locator query", func(t *testing.T) {
		w, ids := setup(t)
		res := w.run(t, Env{
			Transport: &fake{p: byPrefix},
			T3:        failingT3{w.t3, map[string]error{"users": boom}},
		}, w.request())
		for _, th := range []string{"a", "b"} {
			if it := res.item(t, ids[th]); it.Reason != ReasonSQLiteFailed {
				t.Fatalf("%s: %+v", th, it)
			}
		}
		if it := res.item(t, ids["tx"]); it.Status != StatusClassified {
			t.Fatalf("a transcript item was affected by T3: %+v", it)
		}
	})
	t.Run("a thread query fails that thread's items only", func(t *testing.T) {
		w, ids := setup(t)
		res := w.run(t, Env{
			Transport: &fake{p: byPrefix},
			T3:        failingT3{w.t3, map[string]error{"thread:a": boom}},
		}, w.request())
		if it := res.item(t, ids["a"]); it.Reason != ReasonSQLiteFailed {
			t.Fatalf("a: %+v", it)
		}
		if it := res.item(t, ids["b"]); it.Status != StatusClassified {
			t.Fatalf("b: %+v", it)
		}
	})
	t.Run("the cwd query leaves T3 items classified and their duplicate check unassessed",
		func(t *testing.T) {
			w, ids := setup(t)
			res := w.run(t, Env{
				Transport: &fake{p: byPrefix},
				T3:        failingT3{w.t3, map[string]error{"cwds": boom}},
			}, w.request())
			for _, th := range []string{"a", "b"} {
				it := res.item(t, ids[th])
				if it.Status != StatusClassified || !it.Qualified ||
					it.Duplicate != DupUnassessed ||
					it.DupReason != ReasonCwdUnknown {
					t.Fatalf("%s: %+v", th, it)
				}
			}
		})
	t.Run("a thread missing from the database, and a missing message", func(t *testing.T) {
		w, ids := setup(t)
		delete(w.t3, "a")
		w.t3["b"] = ReplayThread{Cwd: "work", Rows: w.t3["b"].Rows[:2]}
		w.items = append(w.items[:0:0], w.items...)
		res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		for _, th := range []string{"a", "b"} {
			if it := res.item(
				t,
				ids[th],
			); it.Status != StatusUnresolved ||
				it.Reason != ReasonSourceGone {
				t.Fatalf("%s: %+v", th, it)
			}
		}
	})
	t.Run("two rows forming one locator are ambiguous", func(t *testing.T) {
		w := newWorld(t)
		rows := w.thread(
			"a",
			"work",
			t3Owner("dup", "task"),
			t3Text("done"),
			t3Owner("same", "fix one"),
			t3Owner("same", "fix two"),
		)
		id := w.t3Reply("a", rows[2])
		w.write()
		res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
		if it := res.item(t, id); it.Reason != ReasonAmbiguous {
			t.Fatalf("%+v", it)
		}
	})
}

// TestLocators: a thread ID holding ':', and numeric and null payload IDs, resolve through the
// locator observe forms.
func TestLocators(t *testing.T) {
	w := newWorld(t)
	ids := make([]string, 0, 4)
	for i, pid := range []any{"m:1", float64(42), 1.5, nil} {
		th := "thread:with:colons:" + strconv.Itoa(i)
		rows := w.thread(
			th,
			"work",
			t3Owner("first", "task"),
			t3Text("done"),
			t3Owner(pid, "fix one"),
		)
		ids = append(ids, w.t3Reply(th, rows[2]))
	}
	w.write()
	res := w.run(t, Env{Transport: &fake{p: byPrefix}}, w.request())
	for _, id := range ids {
		if it := res.item(t, id); it.Status != StatusClassified {
			t.Fatalf("%s: %+v", id, it)
		}
	}
	_ = jev.Pin
}
