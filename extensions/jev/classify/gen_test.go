package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
)

// This file writes fixtures/jev/classify/fixed. It is a maintenance tool, not a check: it runs
// only with JEV_GEN_FIXTURES=1, from this package's directory, and writes the synthetic scenarios
// below. A fixed fixture's `want` is written by hand here and never taken from a run; its records
// are the answers this file assigns to each reply, keyed by the hash of the body the runtime sends
// for it. The cal corpus (gen_cal_test.go) is written pending: labels and `why` only.

const genEnv = "JEV_GEN_FIXTURES"

const fixtureRoot = "../../../fixtures/jev/classify"

// ans is the hand-written answer to one reply: its kind_ probabilities and its dup_ score.
type ans struct {
	probs map[string]float64
	dup   float64
}

func corr() ans     { return ans{probs(LabelCorrection, 0.9), 0.9} }
func newc() ans     { return ans{probs(LabelCorrection, 0.9), 0.1} }
func drift() ans    { return ans{probs(LabelDrift, 0.9), 0.9} }
func driftNew() ans { return ans{probs(LabelDrift, 0.9), 0.1} }
func approval() ans { return ans{probs(LabelApproval, 0.9), 0.1} }
func request() ans  { return ans{probs(LabelRequest, 0.9), 0.1} }

// recorder answers every body from the hand-written answers and records what it answered.
type recorder struct {
	answers map[string]ans
	// byOwner overrides answers for a reply asked under exactly this owner turn.
	byOwner map[string]ans
	// status, when set, answers a body with that HTTP status (a size rejection: 400
	// max_tokens_exceeded) instead.
	status func(b jev.Batch) int
	// drop leaves a question unanswered.
	drop func(q string) bool
	mu   sync.Mutex
	recs map[string]jev.Record
}

func (r *recorder) Send(_ context.Context, b jev.Batch) jev.Outcome {
	rec := jev.Record{Answer: answerFor(b, r.policy())}
	if r.status != nil {
		if s := r.status(b); s != 0 {
			rec = jev.Record{Status: s, Error: jev.CauseMaxTokens}
		}
	}
	r.mu.Lock()
	if r.recs == nil {
		r.recs = map[string]jev.Record{}
	}
	r.recs[b.Hash] = rec
	r.mu.Unlock()
	return rec.Outcome(b.Questions)
}

func (r *recorder) policy() policy {
	return policy{
		kind: func(owner, reply string) map[string]float64 { return r.answer(owner, reply).probs },
		dup:  func(owner, reply, _ string) float64 { return r.answer(owner, reply).dup },
		drop: func(q string) bool { return r.drop != nil && r.drop(q) },
	}
}

func (r *recorder) answer(owner, reply string) ans {
	if a, ok := r.byOwner[owner]; ok {
		return a
	}
	if a, ok := r.answers[reply]; ok {
		return a
	}
	return ans{probs: probs(LabelNoise, 0.9)}
}

// fx is one fixture under construction.
type fx struct {
	t       *testing.T
	name    string
	w       *world
	layouts []LayoutSpec
	memory  map[string]string
	extra   map[string]json.RawMessage
	build   string
	rec     *recorder
	why     string
	status  string
	want    jev.Want
}

func newFx(t *testing.T, name, why string) *fx {
	t.Helper()
	return &fx{
		t: t, name: name, w: newWorld(t), why: why, memory: map[string]string{},
		extra:   map[string]json.RawMessage{},
		rec:     &recorder{answers: map[string]ans{}, byOwner: map[string]ans{}},
		layouts: []LayoutSpec{{Kind: LayoutCheckout, Dir: "repo"}}, status: jev.StatusPass,
	}
}

// reply adds a finished turn in cwd and the owner's reply, with its hand-written answer.
func (f *fx) reply(cwd, task, closing, reply string, a ans) {
	f.rec.answers[reply] = a
	f.w.turn("p/s.jsonl", cwd, task, closing, reply)
}

// t3turn adds a finished T3 turn and the owner's reply; ids are the owner rows' payload IDs.
func (f *fx) t3turn(th string, ids [2]any, task, closing, reply string, a ans) {
	f.rec.answers[reply] = a
	rows := f.w.thread(th, "repo", t3Owner(ids[0], task), t3Text(closing), t3Owner(ids[1], reply))
	f.w.t3Reply(th, rows[2])
}

func (f *fx) json(v any) json.RawMessage {
	f.t.Helper()
	b, err := jev.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// sent adds the owner and assistant texts a reply is expected to be sent with.
func (f *fx) sent(reply, owner, assistant string) {
	f.want.Sent = append(f.want.Sent, jev.Sent{Reply: reply, Owner: owner, Assistant: assistant})
}

// write assembles the fixture, records the bodies the runtime sends and writes it under dir.
func (f *fx) write(dir string) {
	t := f.t
	t.Helper()
	fixture := f.fixture()
	root, env := materialize(t, fixture)
	var stdout, stderr bytes.Buffer
	Main(t.Context(), Env{
		Stdin: bytes.NewReader(fixture.Request), Stdout: &stdout, Stderr: &stderr, Home: realDir(t),
		Dir: root, Getenv: func(string) string { return "" }, Environ: env,
		Decisions: clsFixture{path: "fixed/gen.json", f: fixture}.decisions(), Transport: f.rec,
		Runner: proc.Exec{WaitDelay: jev.PipeWait},
	})
	if len(f.rec.recs) > 0 {
		if err := fixture.SetRecords(f.rec.recs); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := jev.WriteFixture(filepath.Join(dir, f.name+".json"), fixture); err != nil {
		t.Fatal(err)
	}
}

// fixture is the fixture's inputs, without records.
func (f *fx) fixture() jev.Fixture {
	w := f.w
	files := map[string]json.RawMessage{}
	for rel, recs := range w.files {
		files[rel] = f.json(recs)
	}
	maps.Copy(files, f.extra)
	observe := f.json(map[string]any{
		"version": 1,
		"window": Window{
			Since: iso(testNow.Add(-7 * 24 * time.Hour)), Until: iso(testNow.Add(-time.Minute)),
		},
		"sources": w.sources,
		"items":   w.items,
	})
	req := map[string]any{"stage": "cycle", "jev": map[string]any{
		"records": map[string]jev.Record{}, "now": testNow.Format(time.RFC3339),
		"observe": "observe.json", "claude": "claude", "t3": w.t3,
	}}
	return jev.Fixture{
		Synthetic: f.why, Request: f.json(req), Expect: &jev.Expect{Status: f.status},
		Want: &f.want, Repo: f.json(RepoSpec{Layouts: f.layouts}), Observe: observe,
		Files: files, Memory: f.memory, Build: f.build,
	}
}

// Hand-written want helpers: each fixture supplies the values; nothing is read from a run.

const reportDisplayPath = "~/.local/state/stack/jev-classify/latest.json"

func header(n, q, newc, dup, du, unq, unres, uneval int) string {
	return fmt.Sprintf("jev-classify (jev-1.14.0): %d owner replies: %d qualified (%d new, %d "+
		"duplicate, %d dup unassessed), %d unqualified, %d unresolved, %d unevaluated; report %s",
		n, q, newc, dup, du, unq, unres, uneval, reportDisplayPath)
}

func lines(l ...string) string { return strings.Join(l, "\n") }

var okSources = map[string]*ObserveSource{"transcripts": {Status: "ok"}, "t3": {Status: "ok"}}

func (f *fx) report(
	complete bool, sources map[string]*ObserveSource, c Counts, items ...wantItem,
) {
	f.want.Report = f.json(
		wantReport{Complete: complete, Sources: sources, Counts: c, Items: items},
	)
}

// q is a qualified, classified item.
func q(id, where, excerpt, kind, dup, dupReason string) wantItem {
	return wantItem{
		ID: id, Where: where, Status: StatusClassified, Opener: OpenerOwner, Qualified: true,
		Kind: kind, Excerpt: excerpt, Duplicate: dup, DupReason: dupReason,
	}
}

// uq is a classified item that does not qualify.
func uq(id, where string) wantItem {
	return wantItem{ID: id, Where: where, Status: StatusClassified, Opener: OpenerOwner}
}

// un is an unresolved item.
func un(id, where, reason string) wantItem {
	return wantItem{ID: id, Where: where, Status: StatusUnresolved, Reason: reason}
}

// ue is an unevaluated item.
func ue(id, where, reason string) wantItem {
	return wantItem{
		ID:     id,
		Where:  where,
		Status: StatusUnevaluated,
		Reason: reason,
		Opener: OpenerOwner,
	}
}

func withOpener(it wantItem, o string) wantItem { it.Opener = o; return it }

func kindsOf(ks ...string) []jev.Finding {
	out := make([]jev.Finding, 0, len(ks))
	for _, k := range ks {
		out = append(out, jev.Finding{Kind: k})
	}
	return out
}

// counts: items, classified, unevaluated, unresolved, qualified, new, duplicate, dup unassessed,
// of which lookup, not done.
func counts(n, cl, ue, ur, ql, nw, dp, du, dl, nd int) Counts {
	return Counts{
		Items: n, Classified: cl, Unevaluated: ue, Unresolved: ur, Qualified: ql, New: nw,
		Duplicate: dp, DupUnassessed: du, DupUnassessedLookup: dl, NotDone: nd,
	}
}

// filler is n lines of neutral prose, for texts of a chosen size.
func filler(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(
			&b,
			"Note %d about the parser rewrite, kept for context and not relevant here.\n",
			i,
		)
	}
	return b.String()
}

const handWritten = "records written by hand: each reply's kind_ and dup_ answer is assigned in the " +
	"fixture's generator and keyed by the body hash the runtime sends; fixed fixtures check " +
	"behaviour and are never recorded live (jev-calibrate records the cal cases)"

// TestGenerateFixtures writes the fixed fixtures; see the file comment.
//
//nolint:maintidx // one block per fixture
func TestGenerateFixtures(t *testing.T) {
	if os.Getenv(genEnv) != "1" {
		t.Skip(genEnv + " is not set")
	}
	out := filepath.Join(fixtureRoot, "fixed")
	fresh := func(name string) *fx { return newFx(t, name, handWritten) }
	index := "- Always reply in Japanese.\n- Run scripts/test before every push.\n- Keep diffs minimal.\n"

	{ // both locator forms, a thread ID with ':', numeric and null payload IDs
		f := fresh("res-both-locators")
		f.reply("repo", "task A", "closing A", "fix tx", newc())
		f.t3turn("th:a:1", [2]any{"m:1", "m:2"}, "task B", "closing B", "fix t3 string", newc())
		f.t3turn("th:n", [2]any{"x", float64(42)}, "task C", "closing C", "fix numeric", newc())
		f.t3turn("th:z", [2]any{"y", nil}, "task D", "closing D", "fix null id", newc())
		f.sent("fix tx", "task A", "closing A")
		f.sent("fix t3 string", "task B", "closing B")
		f.sent("fix numeric", "task C", "closing C")
		f.sent("fix null id", "task D", "closing D")
		f.want.Context = lines(header(4, 4, 4, 0, 0, 0, 0, 0),
			"correction t3:th:z:: fix null id",
			"correction t3:th:n:42: fix numeric",
			"correction t3:th:a:1:m:2: fix t3 string",
			"correction p/s.jsonl:3: fix tx")
		f.report(true, okSources, counts(4, 4, 0, 0, 4, 4, 0, 0, 0, 0),
			q("it01", "p/s.jsonl:3", "fix tx", LabelCorrection, DupNew, ReasonNoMemory),
			q("it02", "t3:th:a:1:m:2", "fix t3 string", LabelCorrection, DupNew, ReasonNoMemory),
			q("it03", "t3:th:n:42", "fix numeric", LabelCorrection, DupNew, ReasonNoMemory),
			q("it04", "t3:th:z:", "fix null id", LabelCorrection, DupNew, ReasonNoMemory))
		f.write(out)
	}
	{ // a tool result between the assistant text and the reply
		f := fresh("res-tool-result")
		f.rec.answers["fix one"] = newc()
		lines3 := f.w.transcript(
			"p/s.jsonl",
			"repo",
			owner("task A"),
			text("closing A"),
			toolResult(),
			owner("fix one"),
		)
		f.w.reply("p/s.jsonl", lines3[3])
		f.sent("fix one", "task A", "closing A")
		f.want.Context = lines(header(1, 1, 1, 0, 0, 0, 0, 0), "correction p/s.jsonl:4: fix one")
		f.report(true, okSources, counts(1, 1, 0, 0, 1, 1, 0, 0, 0, 0),
			q("it01", "p/s.jsonl:4", "fix one", LabelCorrection, DupNew, ReasonNoMemory))
		f.write(out)
	}
	{ // two owner turns in a row after a turn with text
		f := fresh("res-owner-twice")
		f.rec.answers["hold on"] = request()
		f.rec.answers["fix one"] = newc()
		ls := f.w.transcript(
			"p/s.jsonl",
			"repo",
			owner("task A"),
			text("closing A"),
			owner("hold on"),
			owner("fix one"),
		)
		f.w.reply("p/s.jsonl", ls[2])
		f.w.reply("p/s.jsonl", ls[3])
		f.sent("hold on", "task A", "closing A")
		f.sent("fix one", "task A", "closing A")
		f.want.Context = lines(header(2, 1, 1, 0, 0, 1, 0, 0), "correction p/s.jsonl:4: fix one")
		f.report(true, okSources, counts(2, 2, 0, 0, 1, 1, 0, 0, 0, 0),
			uq("it01", "p/s.jsonl:3"),
			q("it02", "p/s.jsonl:4", "fix one", LabelCorrection, DupNew, ReasonNoMemory))
		f.write(out)
	}
	{ // a tool-only turn followed by two owner replies
		f := fresh("res-tool-only-two-replies")
		ls := f.w.transcript("p/s.jsonl", "repo", owner("older task"), text("older closing"),
			owner("task B"), toolCall(), toolResult(), owner("fix one"), owner("fix two"))
		f.w.reply("p/s.jsonl", ls[5])
		f.w.reply("p/s.jsonl", ls[6])
		f.want.Context = lines(
			header(2, 0, 0, 0, 0, 0, 2, 0),
			"unresolved: 2 (no assistant text 2)",
		)
		f.report(true, okSources, counts(2, 0, 0, 2, 0, 0, 0, 0, 0, 0),
			un("it01", "p/s.jsonl:6", ReasonNoAssistantText),
			un("it02", "p/s.jsonl:7", ReasonNoAssistantText))
		f.write(out)
	}
	{ // a server-created row opening a turn
		f := fresh("res-server-opens")
		f.rec.answers["fix one"] = newc()
		rows := f.w.thread("th:s", "repo", t3Server("s1", "queued prompt"), t3Text("closing S"),
			t3Owner("o1", "fix one"))
		f.w.t3Reply("th:s", rows[2])
		f.sent("fix one", "", "closing S")
		f.want.Context = lines(header(1, 1, 1, 0, 0, 0, 0, 0), "correction t3:th:s:o1: fix one")
		f.report(true, okSources, counts(1, 1, 0, 0, 1, 1, 0, 0, 0, 0),
			withOpener(q("it01", "t3:th:s:o1", "fix one", LabelCorrection, DupNew, ReasonNoMemory),
				OpenerPrompt))
		f.write(out)
	}
	{ // a finished turn, then another prompt or a compaction, then the owner's reply
		f := fresh("res-intervening")
		ls := f.w.transcript(
			"p/s.jsonl",
			"repo",
			owner("task A"),
			text("finished A"),
			transcriptStep(
				sp("a notification"),
			),
			owner("fix one"),
			owner("task C"),
			text("finished C"),
			summary("Summary of earlier work"),
			owner("fix three"),
		)
		f.w.reply("p/s.jsonl", ls[3])
		rows := f.w.thread("th:i", "repo", t3Owner("a", "task B"), t3Text("finished B"),
			t3Server("b", "queued prompt"), t3Owner("c", "fix two"))
		f.w.t3Reply("th:i", rows[3])
		f.w.reply("p/s.jsonl", ls[7])
		f.want.Context = lines(
			header(3, 0, 0, 0, 0, 0, 3, 0),
			"unresolved: 3 (intervening prompt 3)",
		)
		f.report(true, okSources, counts(3, 0, 0, 3, 0, 0, 0, 0, 0, 0),
			un("it01", "p/s.jsonl:4", ReasonIntervening),
			un("it02", "t3:th:i:c", ReasonIntervening),
			un("it03", "p/s.jsonl:8", ReasonIntervening))
		f.write(out)
	}
	{ // a qualified reply in a turn opened by a compaction summary
		f := fresh("res-compaction-opens")
		f.rec.answers["fix one"] = newc()
		ls := f.w.transcript("p/s.jsonl", "repo", summary("Summary of the earlier work"),
			text("resumed after the compaction"), owner("fix one"))
		f.w.reply("p/s.jsonl", ls[2])
		f.sent("fix one", "", "resumed after the compaction")
		f.want.Context = lines(header(1, 1, 1, 0, 0, 0, 0, 0), "correction p/s.jsonl:3: fix one")
		f.report(true, okSources, counts(1, 1, 0, 0, 1, 1, 0, 0, 0, 0),
			withOpener(q("it01", "p/s.jsonl:3", "fix one", LabelCorrection, DupNew, ReasonNoMemory),
				OpenerCompaction))
		f.write(out)
	}
	{ // a reply edited since observe
		f := fresh("res-changed")
		f.rec.answers["fix two"] = newc()
		ls := f.w.transcript(
			"p/s.jsonl",
			"repo",
			owner("task A"),
			text("closing A"),
			owner("fix one"),
			owner("task B"),
			text("closing B"),
			owner("fix two"),
		)
		f.w.reply("p/s.jsonl", ls[2])
		f.w.reply("p/s.jsonl", ls[5])
		f.w.files["p/s.jsonl"][ls[2]-1]["message"] = map[string]any{"content": "fix one, edited"}
		f.sent("fix two", "task B", "closing B")
		f.want.Context = lines(header(2, 1, 1, 0, 0, 0, 1, 0), "correction p/s.jsonl:6: fix two",
			"unresolved: 1 (record changed 1)")
		f.report(true, okSources, counts(2, 1, 0, 1, 1, 1, 0, 0, 0, 0),
			un("it01", "p/s.jsonl:3", ReasonRecordChanged),
			q("it02", "p/s.jsonl:6", "fix two", LabelCorrection, DupNew, ReasonNoMemory))
		f.write(out)
	}
	{ // a thread absent from jev.t3
		f := fresh("res-thread-absent")
		f.t3turn("th:p", [2]any{"a", "b"}, "task P", "closing P", "fix present", newc())
		f.w.item("t3:th:gone:g1", "fix gone", testNow.Add(-time.Hour))
		f.sent("fix present", "task P", "closing P")
		f.want.Context = lines(header(2, 1, 1, 0, 0, 0, 1, 0), "correction t3:th:p:b: fix present",
			"unresolved: 1 (source gone 1)")
		f.report(true, okSources, counts(2, 1, 0, 1, 1, 1, 0, 0, 0, 0),
			q("it01", "t3:th:p:b", "fix present", LabelCorrection, DupNew, ReasonNoMemory),
			un("it02", "t3:th:gone:g1", ReasonSourceGone))
		f.write(out)
	}

	{ // a known duplicate
		f := fresh("dec-duplicate")
		f.memory["repo"] = index
		f.reply("repo", "Refactor the parser", "I answered in English and ran the build.",
			"fix: reply in Japanese", corr())
		f.want.Context = header(1, 1, 0, 1, 0, 0, 0, 0)
		f.report(true, okSources, counts(1, 1, 0, 0, 1, 0, 1, 0, 0, 0),
			q("it01", "p/s.jsonl:3", "fix: reply in Japanese", LabelCorrection, DupDuplicate, ""))
		f.write(out)
	}
	{ // a known new: its dup_ record, deleted alone, fails the fixture
		f := fresh("dec-new")
		f.memory["repo"] = index
		f.reply("repo", "Refactor the parser", "I indented the Makefile with spaces.",
			"fix: use tabs in Makefiles", newc())
		f.want.Context = lines(header(1, 1, 1, 0, 0, 0, 0, 0),
			"correction p/s.jsonl:3: fix: use tabs in Makefiles")
		f.report(true, okSources, counts(1, 1, 0, 0, 1, 1, 0, 0, 0, 0),
			q("it01", "p/s.jsonl:3", "fix: use tabs in Makefiles", LabelCorrection, DupNew, ""))
		f.write(out)
	}
	{ // an approval
		f := fresh("dec-approval")
		f.memory["repo"] = index
		f.reply("repo", "Refactor the parser", "Done; CI passed.", "ok, merged", approval())
		f.want.Context = header(1, 0, 0, 0, 0, 1, 0, 0)
		f.report(true, okSources, counts(1, 1, 0, 0, 0, 0, 0, 0, 0, 0), uq("it01", "p/s.jsonl:3"))
		f.write(out)
	}
	{ // the antecedent pair: the same assistant and reply under two owner turns
		f := fresh("dec-antecedent-pair")
		f.memory["repo"] = "- Do not touch the config files unless asked.\n- Run scripts/test before every push.\n"
		o1 := filler(80) + "Do not touch the config files."
		o2 := "Please rewrite config.toml so the timeout is configurable."
		a := "I rewrote config.toml and the CI settings."
		f.rec.byOwner[o1] = drift()
		f.rec.byOwner[o2] = request()
		ls := f.w.transcript(
			"p/s.jsonl",
			"repo",
			owner(o1),
			text(a),
			owner("revert that"),
			owner(o2),
			text(a),
			owner("revert that"),
		)
		f.w.reply("p/s.jsonl", ls[2])
		f.w.reply("p/s.jsonl", ls[5])
		f.sent("revert that", o1, a)
		f.sent("revert that", o2, a)
		f.want.Context = header(2, 1, 0, 1, 0, 1, 0, 0)
		f.report(true, okSources, counts(2, 2, 0, 0, 1, 0, 1, 0, 0, 0),
			q("it01", "p/s.jsonl:3", "revert that", LabelDrift, DupDuplicate, ""),
			uq("it02", "p/s.jsonl:6"))
		f.write(out)
	}
	{ // a duplicate pair where only the opening instruction differs
		f := fresh("dec-dup-instruction")
		f.memory["repo"] = "- Never change the public API without asking.\n- Run scripts/test before every push.\n"
		o1, o2 := "Do not change the public API.", "Stay inside the billing module."
		a := "I changed the signature of Parse and Format."
		f.rec.byOwner[o1] = drift()
		f.rec.byOwner[o2] = driftNew()
		ls := f.w.transcript(
			"p/s.jsonl",
			"repo",
			owner(o1),
			text(a),
			owner("put that back"),
			owner(o2),
			text(a),
			owner("put that back"),
		)
		f.w.reply("p/s.jsonl", ls[2])
		f.w.reply("p/s.jsonl", ls[5])
		f.sent("put that back", o1, a)
		f.sent("put that back", o2, a)
		f.want.Context = lines(header(2, 2, 1, 1, 0, 0, 0, 0), "drift p/s.jsonl:6: put that back")
		f.report(true, okSources, counts(2, 2, 0, 0, 2, 1, 1, 0, 0, 0),
			q("it01", "p/s.jsonl:3", "put that back", LabelDrift, DupDuplicate, ""),
			q("it02", "p/s.jsonl:6", "put that back", LabelDrift, DupNew, ""))
		f.write(out)
	}
	{ // a duplicate whose referent sits at the start of a long closing message
		f := fresh("dec-long-closing")
		f.memory["repo"] = "- Never rename config keys without asking.\n- Keep diffs minimal.\n"
		closing := "I renamed the config key `timeout` to `deadline` in every file.\n" + filler(260)
		f.reply("repo", "Tidy the config loader", closing, "not that, revert the rename", corr())
		f.sent("not that, revert the rename", "Tidy the config loader", closing)
		f.want.Context = header(1, 1, 0, 1, 0, 0, 0, 0)
		f.report(
			true,
			okSources,
			counts(1, 1, 0, 0, 1, 0, 1, 0, 0, 0),
			q(
				"it01",
				"p/s.jsonl:3",
				"not that, revert the rename",
				LabelCorrection,
				DupDuplicate,
				"",
			),
		)
		f.write(out)
	}

	{ // the duplicate check disabled: a reply the index states and a newer new one, both unchecked
		f := fresh("dec-no-duplicate")
		f.build = "no-duplicate"
		f.memory["repo"] = index
		f.reply("repo", "Refactor the parser", "I answered in English and ran the build.",
			"fix: reply in Japanese", corr())
		f.reply("repo", "Refactor the parser", "I indented the Makefile with spaces.",
			"fix: use tabs in Makefiles", newc())
		f.want.Context = lines(header(2, 2, 0, 0, 2, 0, 0, 0),
			"correction p/s.jsonl:6: fix: use tabs in Makefiles (dup unassessed: duplicate check "+
				"disabled)",
			"correction p/s.jsonl:3: fix: reply in Japanese (dup unassessed: duplicate check "+
				"disabled)",
			"the duplicate check is disabled: disabled in this test build; candidates are not "+
				"checked against memory")
		f.report(true, okSources, counts(2, 2, 0, 0, 2, 0, 0, 2, 0, 0),
			q("it01", "p/s.jsonl:3", "fix: reply in Japanese", LabelCorrection, DupUnassessed,
				ReasonDupDisabled),
			q("it02", "p/s.jsonl:6", "fix: use tabs in Makefiles", LabelCorrection, DupUnassessed,
				ReasonDupDisabled))
		f.write(out)
	}

	{ // the index of the main checkout, a decoy at the linked worktree's
		f := fresh("mem-main-vs-worktree")
		f.layouts = append(f.layouts, LayoutSpec{Kind: LayoutWorktree, Dir: "wt", Of: "repo"})
		f.memory["repo"] = "- Always run scripts/test before pushing.\n"
		f.memory["wt"] = "- Decoy: never run the tests.\n"
		f.reply("wt", "Fix the flaky test", "I pushed without running anything.",
			"fix: run scripts/test first", corr())
		f.want.Context = header(1, 1, 0, 1, 0, 0, 0, 0)
		f.report(
			true,
			okSources,
			counts(1, 1, 0, 0, 1, 0, 1, 0, 0, 0),
			q(
				"it01",
				"p/s.jsonl:3",
				"fix: run scripts/test first",
				LabelCorrection,
				DupDuplicate,
				"",
			),
		)
		f.write(out)
	}
	{ // a repository's own settings set autoMemoryDirectory
		f := fresh("mem-override")
		f.layouts = append(f.layouts, LayoutSpec{Kind: LayoutCheckout, Dir: "repo2"},
			LayoutSpec{Kind: LayoutWorktree, Dir: "wt2", Of: "repo2"})
		setting := f.json(`{"autoMemoryDirectory": "/srv/override-memory"}`)
		f.extra["repo/.claude/settings.json"] = setting
		f.extra["repo2/.claude/settings.local.json"] = setting
		f.memory["repo"] = "- Decoy at the default location.\n"
		f.memory["repo2"] = "- Decoy at the main checkout's default location.\n"
		f.reply("repo", "Task A", "closing A", "fix a", corr())
		f.reply("wt2", "Task B", "closing B", "fix b", corr())
		f.want.Context = lines(header(2, 2, 0, 0, 2, 0, 0, 0),
			"correction p/s.jsonl:6: fix b (dup unassessed: project memory override)",
			"correction p/s.jsonl:3: fix a (dup unassessed: project memory override)")
		f.report(true, okSources, counts(2, 2, 0, 0, 2, 0, 0, 2, 2, 0),
			q("it01", "p/s.jsonl:3", "fix a", LabelCorrection, DupUnassessed, ReasonOverride),
			q("it02", "p/s.jsonl:6", "fix b", LabelCorrection, DupUnassessed, ReasonOverride))
		f.write(out)
	}
	{ // a separate Git directory named .git, and a gitfile pointing at another checkout's .git
		f := fresh("mem-unsupported-layout")
		f.layouts = append(f.layouts, LayoutSpec{Kind: LayoutSeparate, Dir: "sep", Of: "repo"},
			LayoutSpec{Kind: LayoutForeign, Dir: "fgn", Of: "repo"})
		f.memory["repo"] = "- Decoy at the checkout.\n"
		f.memory["sep"] = "- Decoy at the separate directory.\n"
		f.memory["fgn"] = "- Decoy at the gitfile.\n"
		f.reply("sep", "Task A", "closing A", "fix in sep", corr())
		f.reply("fgn", "Task B", "closing B", "fix in fgn", corr())
		f.want.Context = lines(header(2, 2, 0, 0, 2, 0, 0, 0),
			"correction p/s.jsonl:6: fix in fgn (dup unassessed: unsupported layout)",
			"correction p/s.jsonl:3: fix in sep (dup unassessed: unsupported layout)")
		f.report(true, okSources, counts(2, 2, 0, 0, 2, 0, 0, 2, 2, 0),
			q("it01", "p/s.jsonl:3", "fix in sep", LabelCorrection, DupUnassessed, ReasonLayout),
			q("it02", "p/s.jsonl:6", "fix in fgn", LabelCorrection, DupUnassessed, ReasonLayout))
		f.write(out)
	}
	{ // a project directory without MEMORY.md, and a main checkout with no project directory
		f := fresh("mem-new-no-request")
		f.layouts = append(f.layouts, LayoutSpec{Kind: LayoutCheckout, Dir: "repo2"})
		f.extra["@project:repo/memory/notes.md"] = f.json(
			"A topic note; the index file is absent.\n",
		)
		f.reply("repo", "Task A", "closing A", "fix a", corr())
		f.reply("repo2", "Task B", "closing B", "fix b", corr())
		f.want.Context = lines(header(2, 2, 2, 0, 0, 0, 0, 0), "correction p/s.jsonl:6: fix b",
			"correction p/s.jsonl:3: fix a")
		f.report(true, okSources, counts(2, 2, 0, 0, 2, 2, 0, 0, 0, 0),
			q("it01", "p/s.jsonl:3", "fix a", LabelCorrection, DupNew, ReasonNoMemory),
			q("it02", "p/s.jsonl:6", "fix b", LabelCorrection, DupNew, ReasonNoMemory))
		f.write(out)
	}
	{ // a removed cwd
		f := fresh("mem-cwd-gone")
		f.memory["removed"] = "- Decoy where the removed directory would encode.\n"
		f.reply("removed", "Task A", "closing A", "fix a", corr())
		f.want.Context = lines(header(1, 1, 0, 0, 1, 0, 0, 0),
			"correction p/s.jsonl:3: fix a (dup unassessed: cwd gone)")
		f.report(true, okSources, counts(1, 1, 0, 0, 1, 0, 0, 1, 1, 0),
			q("it01", "p/s.jsonl:3", "fix a", LabelCorrection, DupUnassessed, ReasonCwdGone))
		f.write(out)
	}

	{ // a kind_ batch rejected for size
		f := fresh("fail-kind-too-large")
		f.rec.status = func(jev.Batch) int { return 400 }
		f.reply("repo", "Task A", "closing A", "fix a", corr())
		f.reply("repo", "Task B", "closing B", "fix b", corr())
		f.want.Findings = kindsOf(jev.KindUnassessed)
		f.want.Context = lines(header(2, 0, 0, 0, 0, 0, 0, 2), "unevaluated: 2 (too large 2)",
			"complete: false: 2 items not done (too large 2)")
		f.report(false, okSources, counts(2, 0, 2, 0, 0, 0, 0, 0, 0, 2),
			ue("it01", "p/s.jsonl:3", ReasonTooLarge), ue("it02", "p/s.jsonl:6", ReasonTooLarge))
		f.write(out)
	}
	{ // a kind_ answer missing from an otherwise valid response
		f := fresh("fail-missing-answer")
		f.rec.drop = func(q string) bool { return q == "kind_r1" }
		f.reply("repo", "Task A", "closing A", "fix a", corr())
		f.reply("repo", "Task B", "closing B", "fix b", corr())
		f.status = jev.StatusWarn
		f.want.Findings = kindsOf(jev.KindError)
		f.want.Context = lines(
			header(2, 1, 1, 0, 0, 0, 0, 1),
			"correction p/s.jsonl:3: fix a",
			"unevaluated: 1 (malformed answer 1)",
			"complete: false: 1 items not done (malformed answer 1)",
			"jev-classify: Jev returned missing or wrongly typed answer for kind_ requests: the Jev "+
				"API may have changed; check its docs and status",
		)
		f.report(false, okSources, counts(2, 1, 1, 0, 1, 1, 0, 0, 0, 1),
			q("it01", "p/s.jsonl:3", "fix a", LabelCorrection, DupNew, ReasonNoMemory),
			ue("it02", "p/s.jsonl:6", ReasonMalformed))
		f.write(out)
	}
	{ // 12 qualified candidates
		f := fresh("shape-12-candidates")
		items := make([]wantItem, 0, 12)
		tail := make([]string, 0, 10)
		for i := 1; i <= 12; i++ {
			text := fmt.Sprintf("fix c%d", i)
			f.reply("repo", "task", "done", text, corr())
			where := fmt.Sprintf("p/s.jsonl:%d", 3*i)
			items = append(items, q(fmt.Sprintf("it%02d", i), where, text, LabelCorrection, DupNew,
				ReasonNoMemory))
			if i >= 3 {
				tail = append([]string{"correction " + where + ": " + text}, tail...)
			}
		}
		ctx := slices.Concat([]string{header(12, 12, 12, 0, 0, 0, 0, 0)}, tail,
			[]string{"(+2 more in the report)"})
		f.want.Context = lines(ctx...)
		f.report(true, okSources, counts(12, 12, 0, 0, 12, 12, 0, 0, 0, 0), items...)
		f.write(out)
	}
	{ // an empty healthy week
		f := fresh("shape-empty-week")
		f.want.Context = header(0, 0, 0, 0, 0, 0, 0, 0)
		f.report(true, okSources, counts(0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
		f.write(out)
	}
	{ // an empty week with observe's T3 source failed
		f := fresh("shape-empty-t3-failed")
		f.w.sources["t3"] = &ObserveSource{Status: "fail", Error: "sqlite3 failed"}
		f.want.Context = lines(header(0, 0, 0, 0, 0, 0, 0, 0),
			"observe source t3 fail: sqlite3 failed; its replies are missing from this report")
		f.report(true, map[string]*ObserveSource{
			"transcripts": {Status: "ok"}, "t3": {Status: "fail", Error: "sqlite3 failed"},
		}, counts(0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
		f.write(out)
	}
	{ // an empty week with qualification disabled and a failed source
		f := fresh("shape-empty-disabled-failed")
		f.build = "no-qualification"
		f.w.sources["transcripts"] = &ObserveSource{Status: "timeout", Error: "deadline passed"}
		f.want.Context = lines(header(0, 0, 0, 0, 0, 0, 0, 0),
			"observe source transcripts timeout: deadline passed; its replies are missing from "+
				"this report",
			"qualification is disabled: disabled in this test build")
		f.report(true, map[string]*ObserveSource{
			"transcripts": {Status: "timeout", Error: "deadline passed"}, "t3": {Status: "ok"},
		}, counts(0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
		f.write(out)
	}
	{ // an empty week with both decisions disabled: the duplicate line ends at its reason
		f := fresh("shape-empty-no-decisions")
		f.build = "no-decisions"
		f.want.Context = lines(header(0, 0, 0, 0, 0, 0, 0, 0),
			"qualification is disabled: disabled in this test build",
			"the duplicate check is disabled: disabled in this test build")
		f.report(true, okSources, counts(0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
		f.write(out)
	}
	{ // an empty week with the duplicate check disabled: its line keeps the suffix
		f := fresh("shape-empty-no-duplicate")
		f.build = "no-duplicate"
		f.want.Context = lines(header(0, 0, 0, 0, 0, 0, 0, 0),
			"the duplicate check is disabled: disabled in this test build; candidates are not "+
				"checked against memory")
		f.report(true, okSources, counts(0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
		f.write(out)
	}
	{ // the duplicate check disabled and a kind_ answer missing: those items are not done
		f := fresh("shape-no-duplicate-missing-answer")
		f.build = "no-duplicate"
		f.rec.drop = func(q string) bool { return q == "kind_r1" }
		f.reply("repo", "Task A", "closing A", "fix a", corr())
		f.reply("repo", "Task B", "closing B", "fix b", corr())
		f.status = jev.StatusWarn
		f.want.Findings = kindsOf(jev.KindError)
		f.want.Context = lines(
			header(2, 1, 0, 0, 1, 0, 0, 1),
			"correction p/s.jsonl:3: fix a (dup unassessed: duplicate check disabled)",
			"unevaluated: 1 (malformed answer 1)",
			"the duplicate check is disabled: disabled in this test build; candidates are not "+
				"checked against memory",
			"complete: false: 1 items not done (malformed answer 1)",
			"jev-classify: Jev returned missing or wrongly typed answer for kind_ requests: the Jev "+
				"API may have changed; check its docs and status",
		)
		f.report(false, okSources, counts(2, 1, 1, 0, 1, 0, 0, 1, 0, 1),
			q("it01", "p/s.jsonl:3", "fix a", LabelCorrection, DupUnassessed, ReasonDupDisabled),
			ue("it02", "p/s.jsonl:6", ReasonMalformed))
		f.write(out)
	}
	{ // an all-unresolved week
		f := fresh("shape-all-unresolved")
		for i := range 3 {
			f.w.items = append(f.w.items, ObserveItem{
				ID:     fmt.Sprintf("it%02d", i+1),
				Signal: signalOwnerReply,
				Where: fmt.Sprintf(
					"p/none%d.jsonl:2",
					i,
				),
				Excerpt: "x",
				TS:      iso(testNow.Add(-time.Hour)),
			})
		}
		f.want.Context = lines(header(3, 0, 0, 0, 0, 0, 3, 0), "unresolved: 3 (source gone 3)")
		f.report(true, okSources, counts(3, 0, 0, 3, 0, 0, 0, 0, 0, 0),
			un("it01", "p/none0.jsonl:2", ReasonSourceGone),
			un("it02", "p/none1.jsonl:2", ReasonSourceGone),
			un("it03", "p/none2.jsonl:2", ReasonSourceGone))
		f.write(out)
	}
}
