// Package classify is the jev-classify cycle extension (docs/design/phase4-jev.md §3.2): it reads
// the week's owner replies from observe's latest.json, resolves each to the turn it answers, asks
// Jev what each reply does, checks the qualified ones against their project's memory index and
// writes a local report. It is report-only: it never blocks, never feeds improve, and with no key
// (TYPESAFE_API_KEY or the key file) it answers pass without reading anything.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
)

// name prefixes context lines and names the ledger's extension.
const name = "jev-classify"

// Budgets (§3.1): variables only for tests.
var (
	budget  = 5 * time.Minute
	reserve = 20 * time.Second
)

// Finding kind of a report not written (as observe's).
const kindNotWritten = "latest-not-written"

// Env is everything jev-classify takes from the process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Home is the home directory: the key file, the observe file, the report, the ledger, the T3
	// database and the default Claude configuration directory; "" when unknown.
	Home string
	// Dir is the working directory replay paths resolve against; "" means the current one.
	Dir string
	// Getenv reads TYPESAFE_API_KEY and CLAUDE_CONFIG_DIR.
	Getenv func(string) string
	// Environ is the children's base environment.
	Environ []string
	// Runner runs git and sqlite3; nil means proc.Exec with the pipe wait of §3.1.
	Runner proc.Runner
	// Git names the git executable; "" means git on PATH.
	Git string
	// Endpoint is the API URL; "" means the one this binary was built with.
	Endpoint string
	// Decisions replaces the shipped decisions when not nil.
	Decisions map[string]jev.Decision
	// Now is the live clock; nil means time.Now.
	Now func() time.Time
	// Transport, when set, answers a request carrying a jev object in place of its records:
	// jev-calibrate's recording and the tests' fakes run the runtime path on a fixture's inputs.
	Transport jev.Transport
	// T3, when set, replaces the live T3 reader (tests).
	T3 T3
	// Order, when set, is told each resolved item's sent r<n> ID by observe ID, once the items
	// are ordered: jev-calibrate maps a question back to its case.
	Order func(sent map[string]string)
}

// Main answers one protocol request on env.Stdin and returns the exit code.
func Main(ctx context.Context, env Env) int {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	work, cancelWork := context.WithTimeout(ctx, budget-reserve)
	defer cancelWork()
	r := run{env: env, ctx: ctx, work: work, lockBy: start.Add(budget - reserve/2)}
	resp := r.respond()
	if err := jev.Write(env.Stdout, resp); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

// replayDoc is a fixture request's jev object: what classify reads at run time (§3.4).
type replayDoc struct {
	Now     string                  `json:"now"`
	Observe string                  `json:"observe"`
	Claude  string                  `json:"claude"`
	T3      map[string]ReplayThread `json:"t3"`
}

type run struct {
	env       Env
	ctx, work context.Context //nolint:containedctx // one run's process and work deadlines
	lockBy    time.Time

	replay    *replayDoc
	transport jev.Transport
	now       time.Time
	base      string
	ds        map[string]jev.Decision
	reply     jev.Reply
	states    prepared
	lookup    *Lookup
	// errs aggregates persistent failures by phase and cause, in first-seen order.
	errs []*failure
	// skipped counts unassessed requests by cause, in first-seen order.
	skipped      map[string]int
	skippedOrder []string
	unrecorded   int
	usage        Usage
}

// failure is one persistent cause in one phase, with the items it left unfinished.
type failure struct {
	phase, cause string
	items        int
	outcome      jev.Outcome
}

func pass() jev.Response {
	return jev.Response{Status: jev.StatusPass, Findings: []jev.Finding{}}
}

func warnLine(f jev.Finding, line string) jev.Response {
	r := &jev.Reply{}
	r.Warn(f, line)
	return r.Response()
}

func passLine(line string) jev.Response {
	r := &jev.Reply{}
	r.Line(line)
	return r.Response()
}

func (r *run) respond() jev.Response {
	env := r.env
	data, err := io.ReadAll(env.Stdin)
	var req jev.Request
	if err == nil {
		req, err = jev.ParseRequest(data)
	}
	if err != nil {
		return warnLine(jev.Finding{Kind: jev.KindError, Detail: "invalid request"},
			name+": invalid request")
	}
	if req.Stage != "cycle" {
		return pass()
	}
	r.base = env.Dir
	if r.base == "" {
		r.base, _ = os.Getwd()
	}
	if resp, done := r.connect(req); done {
		return resp
	}
	return r.classify()
}

// connect chooses the transport: a replay's records for a request with a jev object (never the
// key or the network), else the live API under the key. done is true when the answer is already
// known: an invalid replay, an unusable key file, or no key.
func (r *run) connect(req jev.Request) (jev.Response, bool) {
	env := r.env
	if req.Jev != nil {
		replay, doc, err := parseReplay(req.Jev)
		if err != nil {
			return warnLine(jev.Finding{Kind: jev.KindError, Detail: "invalid replay records"},
				name+": invalid jev replay records in the request"), true
		}
		r.transport, r.replay = replay, doc
		if env.Transport != nil {
			r.transport = env.Transport
		}
		return jev.Response{}, false
	}
	key, err := jev.Key(env.Getenv, env.Home)
	if ke, ok := errors.AsType[*jev.KeyError](err); ok {
		return warnLine(jev.Finding{Kind: jev.KindError, Detail: ke.Detail()},
			name+": "+ke.Error()), true
	}
	if key == "" {
		return pass(), true
	}
	r.transport = env.live(key)
	return jev.Response{}, false
}

// parseReplay reads the records and the run-time inputs of a fixture request's jev object.
func parseReplay(raw json.RawMessage) (jev.Replay, *replayDoc, error) {
	replay, err := jev.ParseReplay(raw)
	if err != nil {
		return jev.Replay{}, nil, err
	}
	var doc replayDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return jev.Replay{}, nil, fmt.Errorf("%w: %w", jev.ErrRecords, err)
	}
	if _, ok := parseTime(doc.Now); !ok || doc.Observe == "" {
		return jev.Replay{}, nil, fmt.Errorf("%w: now and observe are required", jev.ErrRecords)
	}
	return replay, &doc, nil
}

func (env Env) live(key string) jev.Live {
	endpoint := env.Endpoint
	if endpoint == "" {
		endpoint = jev.Endpoint()
	}
	l := jev.Live{Endpoint: endpoint, Key: key}
	if env.Home != "" {
		l.Ledger = &jev.Ledger{
			Path: jev.LedgerPath(env.Home), Run: jev.RunID(), Extension: name, Stderr: env.Stderr,
		}
	} else {
		_, _ = fmt.Fprintf(env.Stderr, "jev: usage ledger: home directory unknown\n")
	}
	return l
}

func (r *run) clock() time.Time {
	if r.replay != nil {
		t, _ := parseTime(r.replay.Now)
		return t
	}
	if r.env.Now != nil {
		return r.env.Now()
	}
	return time.Now()
}

func (r *run) abs(p string) string {
	return (&resolver{base: r.base}).abs(p)
}

func (r *run) runner() proc.Runner {
	if r.env.Runner != nil {
		return r.env.Runner
	}
	return proc.Exec{WaitDelay: jev.PipeWait}
}

func (r *run) observeFile() string {
	if r.replay != nil {
		return r.abs(r.replay.Observe)
	}
	if r.env.Home == "" {
		return ""
	}
	return observePath(r.env.Home)
}

func (r *run) claudeConfig() string {
	if r.replay != nil {
		if r.replay.Claude == "" {
			return ""
		}
		return r.abs(r.replay.Claude)
	}
	return ClaudeConfig(r.env.Getenv, r.env.Home)
}

func (r *run) t3() T3 {
	if r.env.T3 != nil {
		return r.env.T3
	}
	if r.replay != nil {
		return replayT3(r.replay.T3)
	}
	if r.env.Home == "" {
		return nil
	}
	return SQLite{
		Runner: r.runner(), DB: t3Database(r.env.Home), Env: r.env.Environ,
	}
}

// classify runs from the observe file to the published report.
func (r *run) classify() jev.Response {
	r.now = r.clock()
	path := r.observeFile()
	if path == "" {
		return passLine(name + ": home directory unknown; nothing read")
	}
	obs, err := readObserve(path, r.now)
	if fault, ok := errors.AsType[*observeError](err); ok {
		if fault.warn {
			return warnLine(
				jev.Finding{Kind: jev.KindError, Where: "observe", Detail: fault.detail},
				name+": "+fault.line,
			)
		}
		return passLine(name + ": " + fault.line + "; no report written")
	}
	r.ds = r.env.Decisions
	if r.ds == nil {
		r.ds = Decisions()
	}
	items := ownerReplies(obs)
	r.prepare(items, obs)
	rep := r.report(obs, items)
	return r.finish(rep, obs)
}

// ownerReplies are observe's owner-reply items, in observe's order.
func ownerReplies(obs *Observe) []*Item {
	var items []*Item
	for i, o := range obs.Items {
		if o.Signal != signalOwnerReply {
			continue
		}
		it := &Item{Obs: o, Index: i}
		if t, ok := parseTime(o.TS); ok {
			it.ts = t
		}
		items = append(items, it)
	}
	return items
}

// state is one item's outcome while the run classifies.
type state struct {
	status, reason string
	choice         string
	probs          map[string]float64
	qualified      bool
	kind           string
	dup, dupReason string
	dupP           *float64
	rank           int
}

// prepared holds the run's per-item state, keyed by item.
type prepared map[*Item]*state

func (r *run) prepare(items []*Item, obs *Observe) {
	r.states = prepared{}
	for _, it := range items {
		r.states[it] = &state{rank: -1}
	}
	if len(items) == 0 {
		return
	}
	res := &resolver{base: r.base, t3: r.t3()}
	err := res.resolve(r.work, items, obs.since, obs.until)
	if err != nil {
		r.prepDeadline(items)
		return
	}
	var resolved []*Item
	for _, it := range items {
		st := r.states[it]
		if !it.Resolved {
			st.status, st.reason = StatusUnresolved, it.Reason
			continue
		}
		resolved = append(resolved, it)
	}
	ordered := Order(resolved)
	for i, it := range ordered {
		r.states[it].rank = i
	}
	if r.env.Order != nil {
		sent := make(map[string]string, len(ordered))
		for i, it := range ordered {
			sent[it.Obs.ID] = ID(i)
		}
		r.env.Order(sent)
	}
	if !enabled(r.ds[Qualification]) {
		for _, it := range resolved {
			r.states[it].status, r.states[it].reason = StatusUnevaluated,
				ReasonClassificationDisabled
		}
		return
	}
	r.lookup = &Lookup{
		Runner: r.runner(), Git: r.env.Git, Env: r.env.Environ, Config: r.claudeConfig(),
		Home: r.env.Home,
	}
	r.classifyItems(ordered)
}

// memoryOf looks up the memory of a qualified item's cwd; ok is false when the work deadline came
// during the lookup.
func (r *run) memoryOf(it *Item) (Memory, bool) {
	if it.CwdUnknown || it.Cwd == "" {
		return unassessed(ReasonCwdUnknown), true
	}
	m := r.lookup.Find(r.work, it.Cwd)
	return m, r.work.Err() == nil
}

// prepDeadline marks every item not already unresolved unevaluated (preparation deadline):
// nothing is sent.
func (r *run) prepDeadline(items []*Item) {
	for _, it := range items {
		st := r.states[it]
		if it.Reason != "" && !it.Resolved {
			st.status, st.reason = StatusUnresolved, it.Reason
			continue
		}
		*st = state{status: StatusUnevaluated, reason: ReasonPrepDeadline, rank: st.rank}
	}
}

// classifyItems sends the kind_ requests in groups of InFlight, newest first, each group followed
// by the dup_ requests of the items it qualified (§3.2 Order and deadline).
func (r *run) classifyItems(ordered []*Item) {
	plan, err := jev.Pack(KindInput(ordered))
	if err != nil {
		for _, it := range ordered {
			r.states[it].status, r.states[it].reason = StatusUnevaluated, ReasonRequestFailed
		}
		r.persistent("kind_", "cannot encode the request", len(ordered), jev.Outcome{})
		return
	}
	for _, i := range plan.Unpacked {
		st := r.states[ordered[i]]
		st.status, st.reason = StatusUnevaluated, ReasonTooLarge
	}
	for g := 0; g < len(plan.Batches); g += jev.InFlight {
		group := plan.Batches[g:min(g+jev.InFlight, len(plan.Batches))]
		outcomes := jev.Do(r.work, r.transport, group)
		var qualified []int
		for k, b := range group {
			qualified = append(qualified, r.kindOutcome(b, outcomes[k], ordered)...)
		}
		r.duplicates(qualified, ordered)
	}
}

// kindOutcome applies one kind_ request's outcome to its items and returns the indexes of the
// items it qualified, newest first.
func (r *run) kindOutcome(b jev.Batch, o jev.Outcome, ordered []*Item) []int {
	r.count(o)
	var qualified []int
	reason := r.failure("kind_", o, len(b.Pieces))
	for _, p := range b.Pieces {
		i := p.Unit
		st := r.states[ordered[i]]
		if reason != "" {
			st.status, st.reason = StatusUnevaluated, reason
			continue
		}
		q := "kind_" + ID(i)
		if cause, bad := o.Invalid[q]; bad {
			st.status, st.reason = StatusUnevaluated, ReasonMalformed
			r.persistent("kind_", cause, 1, o)
			continue
		}
		a := o.Answers[q]
		st.status, st.choice, st.probs = StatusClassified, a.Choice, a.Probabilities
		st.qualified, st.kind = Qualify(a.Probabilities, *r.ds[Qualification].Cutoff)
		if st.qualified {
			qualified = append(qualified, i)
		} else {
			st.kind = ""
		}
	}
	return qualified
}

// failure returns the work reason of a request that gave no answers ("" for OK), recording its
// cause for the findings.
func (r *run) failure(phase string, o jev.Outcome, items int) string {
	switch {
	case o.Class == jev.OK:
		return ""
	case o.Class.Unassessed():
		r.skip(o.Describe())
		if o.Class == jev.TooLarge {
			return ReasonTooLarge
		}
		if o.Class == jev.Deadline {
			return ReasonDeadline
		}
		return ReasonTransient
	case o.Class == jev.Unrecorded:
		r.unrecorded++
		return ReasonUnrecorded
	default:
		r.persistent(phase, o.Describe(), items, o)
		return ReasonRequestFailed
	}
}

func (r *run) skip(cause string) {
	if r.skipped == nil {
		r.skipped = map[string]int{}
	}
	if _, ok := r.skipped[cause]; !ok {
		r.skippedOrder = append(r.skippedOrder, cause)
	}
	r.skipped[cause]++
}

// count adds an attempt's usage.
func (r *run) count(o jev.Outcome) {
	if o.Class == jev.Deadline && o.Status == 0 || o.Class == jev.Unrecorded {
		return
	}
	r.usage.Requests++
	if o.Usage != nil && o.Usage.InputTokens != nil {
		r.usage.InputTokens += *o.Usage.InputTokens
	} else if o.Class == jev.OK {
		r.usage.Unknown++
	}
}

// persistent records a failure the owner must act on.
func (r *run) persistent(phase, cause string, items int, o jev.Outcome) {
	for _, f := range r.errs {
		if f.phase == phase && f.cause == cause {
			f.items += items
			return
		}
	}
	r.errs = append(r.errs, &failure{phase: phase, cause: cause, items: items, outcome: o})
}

// duplicates checks the qualified items of one group against their memory index.
func (r *run) duplicates(qualified []int, ordered []*Item) {
	batches := make([]jev.Batch, 0, len(qualified))
	owners := make([]int, 0, len(qualified))
	for _, i := range qualified {
		it := ordered[i]
		st := r.states[it]
		if !enabled(r.ds[Duplicate]) {
			st.dup, st.dupReason = DupUnassessed, ReasonDupDisabled
			continue
		}
		mem, inTime := r.memoryOf(it)
		switch {
		case !inTime:
			st.dup, st.dupReason = DupUnassessed, ReasonDeadline
			continue
		case mem.Outcome != "":
			st.dup, st.dupReason = mem.Outcome, mem.Reason
			continue
		}
		b, ok, err := DupBatch(ID(i), mem.Index, it)
		switch {
		case err != nil:
			st.dup, st.dupReason = DupUnassessed, ReasonRequestFailed
			r.persistent("dup_", "cannot encode the request", 1, jev.Outcome{})
			continue
		case !ok:
			st.dup, st.dupReason = DupUnassessed, ReasonTooLarge
			continue
		}
		batches = append(batches, b)
		owners = append(owners, i)
	}
	if len(batches) == 0 {
		return
	}
	outcomes := jev.Do(r.work, r.transport, batches)
	for k, o := range outcomes {
		i := owners[k]
		st := r.states[ordered[i]]
		r.count(o)
		if reason := r.failure("dup_", o, 1); reason != "" {
			st.dup, st.dupReason = DupUnassessed, reason
			continue
		}
		q := "dup_" + ID(i)
		if cause, bad := o.Invalid[q]; bad {
			st.dup, st.dupReason = DupUnassessed, ReasonMalformed
			r.persistent("dup_", cause, 1, o)
			continue
		}
		p := o.Answers[q].Noul
		st.dupP = &p
		st.dup = DupNew
		if jev.Hit(p, *r.ds[Duplicate].Cutoff) {
			st.dup = DupDuplicate
		}
	}
}

// report builds the report from the run's states.
func (r *run) report(obs *Observe, items []*Item) *Report {
	rep := &Report{
		Version: reportVersion,
		Run: RunInfo{
			ID: jev.RunID(), Started: r.now.UTC().Format(time.RFC3339Nano),
		},
		Window: obs.Window,
		Sources: map[string]*ObserveSource{
			"transcripts": obs.Sources["transcripts"], "t3": obs.Sources["t3"],
		},
		Model:     jev.Pin,
		Decisions: map[string]string{},
		Items:     make([]ReportItem, 0, len(items)),
		Usage:     r.usage,
	}
	for _, d := range []string{Qualification, Duplicate} {
		rep.Decisions[d] = "disabled"
		if enabled(r.ds[d]) {
			rep.Decisions[d] = "enabled"
		}
	}
	for _, it := range items {
		st := r.states[it]
		ri := ReportItem{
			ID: it.Obs.ID, Where: it.Obs.Where, Status: st.status, Reason: st.reason,
			Choice: st.choice, Probabilities: st.probs, Qualified: st.qualified, Kind: st.kind,
			Duplicate: st.dup, DupP: st.dupP, DupReason: st.dupReason, rank: st.rank,
		}
		if it.Resolved {
			ri.Opener = it.Opener
		}
		if ri.Status == StatusClassified {
			ri.Reason = ""
		}
		if st.qualified {
			ri.Excerpt = it.Obs.Excerpt
		}
		rep.Items = append(rep.Items, ri)
	}
	rep.finish()
	return rep
}

// finish adds the findings, publishes the report and answers.
func (r *run) finish(rep *Report, obs *Observe) jev.Response {
	_ = obs
	rep.Run.Finished = rep.Run.Started
	if r.replay == nil {
		rep.Run.Finished = r.clock().UTC().Format(time.RFC3339Nano)
	}
	data, err := marshalReport(rep)
	cause := ""
	if err != nil {
		cause = "cannot encode the report"
	} else if r.env.Home == "" {
		cause = "home directory unknown"
	}
	drop := false
	if cause == "" {
		started, _ := parseTime(rep.Run.Started)
		res := publish(r.ctx, reportPath(r.env.Home), data, obs.until, started, r.lockBy)
		switch res.outcome {
		case failed:
			cause = res.cause
		case dropped:
			drop = true
		}
	}
	// A dropped report's counts and candidates are not in the report that stands: the context
	// is only the notice.
	if !drop {
		for _, l := range rep.contextLines(r.ds) {
			r.reply.Line(l)
		}
	}
	r.findings(rep)
	if drop {
		r.reply.Line(name + ": a report for a newer window or a later run already stands; " +
			"this run's report was dropped")
	}
	if cause != "" {
		r.reply.Warn(jev.Finding{Kind: kindNotWritten, Detail: cause},
			name+": report not written: "+cause)
	}
	return r.reply.Response()
}

func marshalReport(rep *Report) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return nil, fmt.Errorf("encode report: %w", err)
	}
	return b.Bytes(), nil
}
