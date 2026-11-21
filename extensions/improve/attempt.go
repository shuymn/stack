package improve

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// stop ends an attempt with an outcome.
type stop struct{ outcome, reason string }

func (e *stop) Error() string { return e.outcome + ": " + e.reason }

func halt(outcome, format string, args ...any) *stop {
	return &stop{outcome: outcome, reason: fmt.Sprintf(format, args...)}
}

// attempt is one claimed week (steps 7-13).
type attempt struct {
	s  *state
	st stamp

	worktree string
	added    bool
	tmp      string

	// modelStarted is false until the claude child has started; zeroCost is set when a complete
	// result proves the model did no billed work (step 10).
	modelStarted, zeroCost bool
	costKnown              bool

	briefTxt string
	briefIDs map[string]item

	title, body, message string
	titleFile, bodyFile  string
	oid                  string
}

func iso(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// attempt claims the week, runs the attempt, finalises the stamp and cleans up.
func (s *state) attempt(public []item) (string, string) {
	a := &attempt{
		s: s, briefIDs: map[string]item{},
		st: stamp{
			Week: s.week, Started: iso(s.o.Now()), Repo: s.slug, BaseOID: s.baseOID,
			Outcome: outStarted,
		},
	}
	for _, it := range public {
		a.briefIDs[it.ID] = it
	}
	a.briefTxt = brief(public)
	// 7. Claim the week.
	data, err := encodeStamp(a.st.redacted(s.redact))
	if err == nil {
		err = claimStamp(s.stampPath, data)
	}
	if err != nil {
		s.logf("claim %s: %v", s.stampPath, err)
		return StatusWarn, "cannot claim the week stamp " + s.stampPath
	}
	a.run()
	// Final stamp, then cleanup (13), then the unbilled stamp removal (10).
	a.st.Finished = iso(s.o.Now())
	if !a.modelStarted {
		a.st.CostUSD = 0
	}
	if err := a.save(); err != nil {
		s.logf("final stamp: %v", err)
		s.finding(
			kindOutcome,
			s.stampPath,
			"cannot write the final stamp; it may still say started",
		)
	}
	a.cleanup()
	a.hook("cleanup")
	removed := false
	if !a.modelStarted || a.zeroCost {
		if err := os.Remove(s.stampPath); err != nil {
			s.logf("remove stamp: %v", err)
		} else {
			removed = true
		}
	}
	return a.respond(removed)
}

// run performs steps 8-12, turning a stop or a panic into the stamp's outcome.
func (a *attempt) run() {
	defer func() {
		if r := recover(); r != nil {
			a.s.logf("internal error: %v\n%s", r, debug.Stack())
			a.st.Outcome, a.st.Reason = "failed:internal", fmt.Sprintf("internal error: %v", r)
		}
	}()
	err := a.steps()
	if err == nil {
		return
	}
	a.st.Outcome, a.st.Reason = err.outcome, err.reason
}

func (a *attempt) save() error {
	data, err := encodeStamp(a.st.redacted(a.s.redact))
	if err != nil {
		return err
	}
	return replaceFile(a.s.stampPath, data)
}

func (a *attempt) hook(point string) {
	if a.s.o.hook != nil {
		a.s.o.hook(point)
	}
}

// steps runs steps 8-12; nil means the outcome is already set (a PR).
func (a *attempt) steps() *stop {
	s := a.s
	// 8. Worktree.
	work := filepath.Join(s.stateDir, "work")
	if entries, err := os.ReadDir(work); err == nil {
		for _, e := range entries {
			p := filepath.Join(work, e.Name())
			s.finding(kindLeftover, p, "left by an earlier run that ended abnormally; remove it "+
				"(git worktree remove --force) once nothing uses it")
		}
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return halt("failed:worktree", "cannot create the work directory")
	}
	tmp, err := os.MkdirTemp(s.stateDir, ".run-")
	if err != nil {
		return halt("failed:worktree", "cannot create a temp directory")
	}
	a.tmp = tmp
	if out := s.git(s.repo, "worktree", "prune"); !out.ok() {
		return halt("failed:worktree", "git worktree prune: %s", out.describe())
	}
	wt := filepath.Join(work, s.week)
	if out := s.git(s.repo, "worktree", "add", "--detach", wt, s.baseOID); !out.ok() {
		return halt("failed:worktree", "git worktree add: %s", out.describe())
	}
	a.worktree, a.added = wt, true
	a.hook("worktree")
	if s.modelTime() < s.o.limits.modelMin {
		return halt("failed:deadline", "not enough time left for the model")
	}
	// 9-11.
	if err := a.model(); err != nil {
		return err
	}
	// 12.
	return a.publish()
}

// cleanup is step 13: the worktree is removed only when this run added it.
func (a *attempt) cleanup() {
	s := a.s
	if a.added {
		if out := s.git(s.repo, "worktree", "remove", "--force", a.worktree); !out.ok() {
			s.logf("git worktree remove: %s", out.describe())
			s.finding(kindLeftover, a.worktree, "this run could not remove its worktree: "+
				out.describe())
		} else {
			s.logf("worktree removed: %s", a.worktree)
		}
	}
	if a.tmp != "" {
		if err := os.RemoveAll(a.tmp); err != nil {
			s.logf("remove temp directory: %v", err)
		}
	}
}

// respond states the outcome, the week, the cost, summary and watch, and the PR or the stamp.
func (a *attempt) respond(removed bool) (string, string) {
	s := a.s
	cost := "unknown"
	switch c := a.st.CostUSD.(type) {
	case float64:
		cost = fmt.Sprintf("$%.2f", c)
	case int:
		cost = fmt.Sprintf("$%d", c)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "improve %s: %s (cost %s)", a.st.Week, a.st.Outcome, cost)
	if a.st.Reason != "" {
		fmt.Fprintf(&b, "\nreason: %s", cut(a.st.Reason, detailLen))
	}
	if a.st.Summary != "" {
		fmt.Fprintf(&b, "\nsummary: %s", a.st.Summary)
	}
	if a.st.Watch != "" {
		fmt.Fprintf(&b, "\nwatch: %s", a.st.Watch)
	}
	switch {
	case a.st.PR != "":
		fmt.Fprintf(&b, "\nPR: %s", a.st.PR)
	case removed:
		fmt.Fprintf(&b, "\nstamp removed (the model did no billed work): %s", s.stampPath)
	default:
		fmt.Fprintf(&b, "\nstamp: %s", s.stampPath)
	}
	if a.modelStarted && !a.costKnown {
		s.finding(kindCostUnknown, s.stampPath, "the model's cost is unknown")
	}
	if a.st.Outcome == outNoChange || strings.HasPrefix(a.st.Outcome, prefixPR) {
		return StatusPass, b.String()
	}
	detail := a.st.Outcome
	if a.st.Reason != "" {
		detail += ": " + a.st.Reason
	}
	s.finding(kindOutcome, s.stampPath, detail)
	return StatusWarn, b.String()
}
