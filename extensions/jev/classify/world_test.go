package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/internal/t3loc"
	"github.com/shuymn/stack/internal/gittest"
	"github.com/shuymn/stack/internal/proc/proctest"
)

func TestMain(m *testing.M) {
	proctest.Main()
	if p := os.Getenv(lockHolderEnv); p != "" {
		holdLock(p)
	}
	startSpy()
	cleanup := buildBinaries()
	code := gittest.Main(m)
	cleanup()
	spy.server.Close()
	os.Exit(code)
}

// testNow is the clock of every test world.
var testNow = time.Date(2026, 1, 8, 12, 0, 0, 0, time.UTC)

// world is a temporary cwd and home with an observe file, transcripts and T3 rows.
type world struct {
	t          *testing.T
	root, home string
	items      []ObserveItem
	sources    map[string]*ObserveSource
	t3         replayT3
	files      map[string][]map[string]any
	clock      time.Time
	// live puts the observe file under the home directory and writes absolute locators, for a
	// request with no jev object.
	live bool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		t: t, root: realDir(t), home: realDir(t), t3: replayT3{},
		files: map[string][]map[string]any{},
		sources: map[string]*ObserveSource{
			"transcripts": {Status: "ok"}, "t3": {Status: "ok"},
		},
		clock: testNow.Add(-3 * time.Hour),
	}
	return w
}

// tick returns the next record time, one second apart.
func (w *world) tick() string {
	w.clock = w.clock.Add(time.Second)
	return w.clock.Format(time.RFC3339Nano)
}

// Transcript records.
func owner(text string) map[string]any {
	return map[string]any{
		"type": "user", "origin": map[string]any{"kind": "human"},
		"message": map[string]any{"role": "user", "content": text},
	}
}

func prompt(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"content": text}}
}

func summary(text string) map[string]any {
	r := prompt(text)
	r["isCompactSummary"] = true
	return r
}

func text(t string) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{
		"content": []any{map[string]any{"type": "text", "text": t}},
	}}
}

func toolCall() map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{
		"content": []any{map[string]any{"type": "tool_use", "id": "tu", "name": "Bash"}},
	}}
}

func toolResult() map[string]any {
	return map[string]any{
		"type": "user", "toolUseResult": map[string]any{},
		"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu", "content": "ok"},
		}},
	}
}

// transcript appends records to the transcript at rel (relative to the root), stamping each with
// a time and cwd, and returns their 1-based line numbers.
func (w *world) transcript(rel, cwd string, recs ...map[string]any) []int {
	lines := make([]int, 0, len(recs))
	for _, r := range recs {
		r["timestamp"] = w.tick()
		r["cwd"] = cwd
		w.files[rel] = append(w.files[rel], r)
		lines = append(lines, len(w.files[rel]))
	}
	return lines
}

// reply adds observe's owner-reply item for the transcript record at rel:line.
func (w *world) reply(rel string, line int) string {
	r := w.files[rel][line-1]
	stamp, _ := r["timestamp"].(string)
	ts, _ := parseTime(stamp)
	msg, _ := r["message"].(map[string]any)
	content, _ := msg["content"].(string)
	where := rel
	if w.live {
		where = filepath.Join(w.root, filepath.FromSlash(rel))
	}
	return w.item(fmt.Sprintf("%s:%d", where, line), content, ts)
}

func (w *world) item(where, text string, ts time.Time) string {
	id := fmt.Sprintf("it%02d", len(w.items)+1)
	w.items = append(w.items, ObserveItem{
		ID: id, Signal: signalOwnerReply, Where: where, Excerpt: excerpt(text), TS: iso(ts),
	})
	return id
}

// T3 rows.
func t3User(id any, text, source string) T3Row {
	p := map[string]any{"createdBy": "user", "creationSource": source, "text": text, "id": id}
	b, _ := jev.Marshal(p)
	return T3Row{Role: "user", Payload: string(b)}
}

func t3Owner(id any, text string) T3Row { return t3User(id, text, "web") }

func t3Server(id any, text string) T3Row { return t3User(id, text, "server") }

func t3Agent(text string) T3Row {
	b, _ := jev.Marshal(map[string]any{"createdBy": "agent", "text": text})
	return T3Row{Role: "user", Payload: string(b)}
}

func t3Text(text string) T3Row {
	b, _ := jev.Marshal(map[string]any{"text": text})
	return T3Row{Role: "assistant", Payload: string(b)}
}

func t3Tool() T3Row { return T3Row{Role: "assistant", Payload: `{"text":""}`} }

// thread adds rows to a T3 thread, each a second after the last, and returns their indexes.
func (w *world) thread(id, cwd string, rows ...T3Row) []int {
	th := w.t3[id]
	th.Cwd = cwd
	idx := make([]int, 0, len(rows))
	for _, r := range rows {
		w.clock = w.clock.Add(time.Second)
		r.CreatedAt = t3Time(w.clock)
		r.MessageID = fmt.Sprintf("m%03d", len(th.Rows))
		th.Rows = append(th.Rows, r)
		idx = append(idx, len(th.Rows)-1)
	}
	w.t3[id] = th
	return idx
}

// t3Reply adds observe's item for the T3 row at index i of thread id.
func (w *world) t3Reply(id string, i int) string {
	row := w.t3[id].Rows[i]
	p, _ := row.payload()
	ts, _ := parseTime(row.CreatedAt)
	text, _ := p["text"].(string)
	return w.item(t3loc.Locator(id, p["id"]), text, ts)
}

// write writes the transcripts and the observe file under the root.
func (w *world) write() {
	w.t.Helper()
	for rel, recs := range w.files {
		var b bytes.Buffer
		for _, r := range recs {
			line, err := json.Marshal(r)
			if err != nil {
				w.t.Fatal(err)
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		w.writeFile(rel, b.String())
	}
	obs := map[string]any{
		"version": 1,
		"window": Window{
			Since: iso(testNow.Add(-7 * 24 * time.Hour)), Until: iso(testNow.Add(-time.Minute)),
		},
		"sources": w.sources,
		"items":   w.items,
	}
	data, err := json.Marshal(obs)
	if err != nil {
		w.t.Fatal(err)
	}
	w.writeFile("observe.json", string(data))
	if w.live {
		p := observePath(w.home)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) writeFile(rel, content string) {
	w.t.Helper()
	p := filepath.Join(w.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

//nolint:unparam // other tests name other directories
func (w *world) mkdir(rel string) string {
	w.t.Helper()
	p := filepath.Join(w.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(p, 0o700); err != nil {
		w.t.Fatal(err)
	}
	return p
}

// memory writes an index where the lookup places a plain directory's memory: under the world's
// Claude configuration directory (root/claude), at the directory's encoded path.
//
//nolint:unparam // other tests name other directories
func (w *world) memory(dir, index string) {
	w.t.Helper()
	abs := filepath.Join(w.root, filepath.FromSlash(dir))
	p := filepath.Join(w.root, "claude", "projects", ProjectName(abs), "memory", memoryIndexFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(index), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

// request is the cycle request of a replay over the world, without records.
func (w *world) request() map[string]any {
	return map[string]any{"stage": "cycle", "jev": map[string]any{
		"records": map[string]any{}, "now": testNow.Format(time.RFC3339),
		"observe": "observe.json", "claude": "claude", "t3": w.t3,
	}}
}

// policy answers a fake request: kind_ probabilities by reply text, dup_ score by reply and
// index.
type policy struct {
	kind func(owner, reply string) map[string]float64
	dup  func(owner, reply, index string) float64
	// status, when set, answers a request with that HTTP status instead.
	status func(b jev.Batch) int
	// drop leaves a question unanswered.
	drop func(q string) bool
}

// fake is a transport answering from a policy and recording every body.
type fake struct {
	p      policy
	mu     sync.Mutex
	bodies [][]byte
	log    []*event
	seq    int
	// gate, when set, is called before each answer (to hold or time requests).
	gate func(b jev.Batch)
	// block, when set and true for a batch, holds the request until its context ends.
	block func(b jev.Batch) bool
}

// event is one request's start and end in a global sequence, with its body.
type event struct {
	start, end int
	body       []byte
}

func (f *fake) Send(ctx context.Context, b jev.Batch) jev.Outcome {
	ev := &event{body: b.Body}
	f.mu.Lock()
	f.bodies = append(f.bodies, b.Body)
	f.log = append(f.log, ev)
	f.mu.Unlock()
	ev.start = f.tick()
	defer func() { ev.end = f.tick() }()
	if f.gate != nil {
		f.gate(b)
	}
	if f.block != nil && f.block(b) {
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return jev.Outcome{Class: jev.Deadline, Cause: jev.CauseDeadline}
	}
	if f.p.status != nil {
		if s := f.p.status(b); s != 0 {
			return jev.Record{Status: s}.Outcome(b.Questions)
		}
	}
	return jev.Record{Answer: answerFor(b, f.p)}.Outcome(b.Questions)
}

func (f *fake) events() []event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event, len(f.log))
	for i, e := range f.log {
		out[i] = *e
	}
	return out
}

func (f *fake) tick() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.seq
}

func (f *fake) sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.bodies)
}

// sentBody is a decoded request body.
type sentBody struct {
	Model string `json:"model"`
	State struct {
		Items []struct {
			ID        string `json:"id"`
			Owner     string `json:"owner"`
			Assistant string `json:"assistant"`
			Reply     string `json:"reply"`
		} `json:"items"`
		MemoryIndex string `json:"memory_index"` //nolint:tagliatelle // the state key
		Owner       string `json:"owner"`
		Assistant   string `json:"assistant"`
		Reply       string `json:"reply"`
	} `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

func decodeBody(body []byte) sentBody {
	var s sentBody
	_ = json.Unmarshal(body, &s)
	return s
}

// answerFor builds the 200 body the policy gives for a batch.
func answerFor(b jev.Batch, p policy) json.RawMessage {
	s := decodeBody(b.Body)
	answers := map[string]any{}
	replies := map[string][2]string{}
	for _, it := range s.State.Items {
		replies[it.ID] = [2]string{it.Owner, it.Reply}
	}
	for _, q := range b.Questions {
		if p.drop != nil && p.drop(q.Name) {
			continue
		}
		switch {
		case strings.HasPrefix(q.Name, "kind_"):
			it := replies[strings.TrimPrefix(q.Name, "kind_")]
			probs := p.kind(it[0], it[1])
			choice := LabelNoise
			for _, l := range Labels {
				if probs[l] > probs[choice] {
					choice = l
				}
			}
			answers[q.Name] = map[string]any{
				"type": "choice", "choice": choice, "confidence": probs[choice],
				"probabilities": probs,
			}
		case strings.HasPrefix(q.Name, "dup_"):
			answers[q.Name] = map[string]any{
				"type": "noul", "noul": p.dup(s.State.Owner, s.State.Reply, s.State.MemoryIndex),
			}
		}
	}
	out, _ := jev.Marshal(map[string]any{
		"model": jev.Pin, "answers": answers,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
	})
	return out
}

// probs gives label l probability p and spreads the rest evenly.
//
//nolint:unparam // a helper for other probabilities
func probs(l string, p float64) map[string]float64 {
	out := map[string]float64{}
	for _, x := range Labels {
		out[x] = (1 - p) / float64(len(Labels)-1)
	}
	out[l] = p
	return out
}

// byPrefix labels a reply by its first word: "fix" correction, "back" drift, "ok" approval,
// "q" request, else noise; dup_ is 0.9 when the index contains the reply's second word.
var byPrefix = policy{
	kind: func(_, reply string) map[string]float64 {
		switch strings.Fields(reply + " x")[0] {
		case "fix":
			return probs(LabelCorrection, 0.9)
		case "back":
			return probs(LabelDrift, 0.9)
		case "ok":
			return probs(LabelApproval, 0.9)
		case "q":
			return probs(LabelRequest, 0.9)
		}
		return probs(LabelNoise, 0.9)
	},
	dup: func(_, reply, index string) float64 {
		f := strings.Fields(reply + " x x")
		if strings.Contains(index, f[1]) {
			return 0.9
		}
		return 0.1
	},
}

// testDecisions enables both decisions at 0.5.
func testDecisions() map[string]jev.Decision {
	return ParseTestCutoffs("qualification=0.5,duplicate=0.5")
}

type result struct {
	code   int
	resp   jev.Response
	stderr string
	report *Report
}

// run runs jev-classify in process over the world with transport t answering the replay
// request.
func (w *world) run(t *testing.T, env Env, req any) result {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env.Stdin, env.Stdout, env.Stderr = bytes.NewReader(data), &stdout, &stderr
	if env.Home == "" {
		env.Home = w.home
	}
	if env.Dir == "" {
		env.Dir = w.root
	}
	if env.Decisions == nil {
		env.Decisions = testDecisions()
	}
	if env.Runner == nil {
		env.Runner = proc.Exec{WaitDelay: jev.PipeWait}
	}
	if env.Environ == nil {
		env.Environ = gittest.CleanEnv(os.Environ())
	}
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	code := Main(t.Context(), env)
	res := result{code: code, stderr: stderr.String()}
	if err := json.Unmarshal(stdout.Bytes(), &res.resp); err != nil {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if data, err := os.ReadFile(reportPath(env.Home)); err == nil {
		res.report = &Report{}
		if err := json.Unmarshal(data, res.report); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

// item returns the report item with observe ID id.
func (r result) item(t *testing.T, id string) ReportItem {
	t.Helper()
	if r.report == nil {
		t.Fatalf("no report: %+v", r.resp)
	}
	for _, it := range r.report.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %s in %+v", id, r.report.Items)
	return ReportItem{}
}
