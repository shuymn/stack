package classify

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// reportVersion is the report's schema.
const reportVersion = 1

// Item statuses in the report.
const (
	StatusClassified  = "classified"
	StatusUnresolved  = "unresolved"
	StatusUnevaluated = "unevaluated"
)

// Duplicate outcomes.
const (
	DupDuplicate  = "duplicate"
	DupNew        = "new"
	DupUnassessed = "unassessed"
)

// Work reasons: why an item is unevaluated or its duplicate check unassessed.
const (
	ReasonClassificationDisabled = "classification disabled"
	ReasonDupDisabled            = "duplicate check disabled"
	ReasonTooLarge               = "too large"
	ReasonTransient              = "transient"
	ReasonDeadline               = "deadline"
	ReasonPrepDeadline           = "preparation deadline"
	ReasonMalformed              = "malformed answer"
	ReasonRequestFailed          = "request failed"
	ReasonUnrecorded             = "unrecorded"
)

// lookupReasons leave a qualified item done although its duplicate check is unassessed: they
// are coverage, not unfinished work.
var lookupReasons = []string{
	ReasonCwdGone, ReasonCwdUnknown, ReasonLayout, ReasonOverride, ReasonSettings, ReasonIndexBad,
}

// maxCandidates bounds the candidate lines of the context.
const maxCandidates = 10

// ReportItem is one owner-reply item in the report.
//
//nolint:tagliatelle // the report uses snake_case keys (§3.2).
type ReportItem struct {
	ID            string             `json:"id"`
	Where         string             `json:"where"`
	Status        string             `json:"status"`
	Reason        string             `json:"reason,omitempty"`
	Opener        string             `json:"opener,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Qualified     bool               `json:"qualified"`
	Kind          string             `json:"kind,omitempty"`
	Excerpt       string             `json:"excerpt,omitempty"`
	Duplicate     string             `json:"duplicate,omitempty"`
	DupP          *float64           `json:"dup_p,omitempty"`
	DupReason     string             `json:"dup_reason,omitempty"`

	// rank is the item's position newest first, for the context's order; -1 when unranked.
	rank int
}

// Done reports whether the item's required work finished (§3.2 complete).
func (it ReportItem) Done() bool {
	switch it.Status {
	case StatusUnresolved:
		return true
	case StatusUnevaluated:
		return it.Reason == ReasonClassificationDisabled
	}
	if !it.Qualified {
		return true
	}
	switch it.Duplicate {
	case DupDuplicate, DupNew:
		return true
	case DupUnassessed:
		return it.DupReason == ReasonDupDisabled || slices.Contains(lookupReasons, it.DupReason)
	}
	return false
}

// notDoneReason is why an item is not done.
func (it ReportItem) notDoneReason() string {
	if it.Status == StatusUnevaluated {
		return it.Reason
	}
	return "dup " + it.DupReason
}

// RunInfo identifies one run.
type RunInfo struct {
	ID       string `json:"id"`
	Started  string `json:"started"`
	Finished string `json:"finished"`
}

// Counts summarises the items.
//
//nolint:tagliatelle // the report uses snake_case keys (§3.2).
type Counts struct {
	Items               int `json:"items"`
	Classified          int `json:"classified"`
	Unresolved          int `json:"unresolved"`
	Unevaluated         int `json:"unevaluated"`
	Qualified           int `json:"qualified"`
	Duplicate           int `json:"duplicate"`
	New                 int `json:"new"`
	DupUnassessed       int `json:"dup_unassessed"`
	DupUnassessedLookup int `json:"dup_unassessed_lookup"`
	NotDone             int `json:"not_done"`
}

// Usage totals the live attempts' token usage.
//
//nolint:tagliatelle // the report uses snake_case keys (§3.2).
type Usage struct {
	Requests    int   `json:"requests"`
	InputTokens int64 `json:"input_tokens"`
	// Unknown counts answers without a usage.input_tokens count.
	Unknown int `json:"unknown"`
}

// Report is jev-classify/latest.json.
type Report struct {
	Version   int                       `json:"version"`
	Run       RunInfo                   `json:"run"`
	Window    Window                    `json:"window"`
	Sources   map[string]*ObserveSource `json:"sources"`
	Complete  bool                      `json:"complete"`
	Model     string                    `json:"model"`
	Decisions map[string]string         `json:"decisions"`
	Counts    Counts                    `json:"counts"`
	Items     []ReportItem              `json:"items"`
	Usage     Usage                     `json:"usage"`
}

// finish computes counts and complete from the items.
func (r *Report) finish() {
	c := Counts{Items: len(r.Items)}
	for _, it := range r.Items {
		switch it.Status {
		case StatusClassified:
			c.Classified++
		case StatusUnresolved:
			c.Unresolved++
		case StatusUnevaluated:
			c.Unevaluated++
		}
		if it.Qualified {
			c.Qualified++
			switch it.Duplicate {
			case DupDuplicate:
				c.Duplicate++
			case DupNew:
				c.New++
			case DupUnassessed:
				c.DupUnassessed++
				if slices.Contains(lookupReasons, it.DupReason) {
					c.DupUnassessedLookup++
				}
			}
		}
		if !it.Done() {
			c.NotDone++
		}
	}
	r.Counts = c
	r.Complete = c.NotDone == 0
}

// reportDisplay names the report in the context, independent of the home directory.
const reportDisplay = "~/.local/state/stack/jev-classify/latest.json"

// tally renders reason counts in descending count, then name order: "deadline 3, transient 1".
func tally(m map[string]int) string {
	keys := slices.Sorted(maps.Keys(m))
	slices.SortStableFunc(keys, func(a, b string) int { return m[b] - m[a] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// contextLines builds the context (§3.2 Context): the counts; new qualified items newest first,
// then qualified items whose duplicate check is unassessed, up to 10 lines and a "+k more"
// marker; the unresolved and unevaluated counts; any failed observe source, any disabled
// decision, and complete: false with the items not done and why.
func (r *Report) contextLines(ds map[string]jev.Decision) []string {
	c := r.Counts
	lines := []string{fmt.Sprintf(
		"%s (%s): %d owner replies: %d qualified (%d new, %d duplicate, %d dup unassessed), "+
			"%d unqualified, %d unresolved, %d unevaluated; report %s",
		name, jev.Pin, c.Items, c.Qualified, c.New, c.Duplicate, c.DupUnassessed,
		c.Classified-c.Qualified, c.Unresolved, c.Unevaluated, reportDisplay)}
	var cands []ReportItem
	for _, it := range r.Items {
		if it.Qualified && it.Duplicate == DupNew {
			cands = append(cands, it)
		}
	}
	newest := func(a, b ReportItem) int { return a.rank - b.rank }
	slices.SortStableFunc(cands, newest)
	var unassessed []ReportItem
	for _, it := range r.Items {
		if it.Qualified && it.Duplicate == DupUnassessed {
			unassessed = append(unassessed, it)
		}
	}
	slices.SortStableFunc(unassessed, newest)
	cands = append(cands, unassessed...)
	for i, it := range cands {
		if i == maxCandidates {
			lines = append(lines, fmt.Sprintf("(+%d more in the report)", len(cands)-i))
			break
		}
		l := fmt.Sprintf("%s %s: %s", it.Kind, it.Where, it.Excerpt)
		if it.Duplicate == DupUnassessed {
			l += " (dup unassessed: " + it.DupReason + ")"
		}
		lines = append(lines, l)
	}
	unresolved, unevaluated, notDone := map[string]int{}, map[string]int{}, map[string]int{}
	for _, it := range r.Items {
		switch it.Status {
		case StatusUnresolved:
			unresolved[it.Reason]++
		case StatusUnevaluated:
			unevaluated[it.Reason]++
		}
		if !it.Done() {
			notDone[it.notDoneReason()]++
		}
	}
	if c.Unresolved > 0 {
		lines = append(lines, fmt.Sprintf("unresolved: %d (%s)", c.Unresolved, tally(unresolved)))
	}
	if c.Unevaluated > 0 {
		lines = append(lines,
			fmt.Sprintf("unevaluated: %d (%s)", c.Unevaluated, tally(unevaluated)))
	}
	for _, src := range []string{"transcripts", "t3"} {
		if s := r.Sources[src]; s.Failed() {
			lines = append(lines, fmt.Sprintf("observe source %s %s: %s; its replies are missing "+
				"from this report", src, s.Status, s.Error))
		}
	}
	for _, d := range []struct{ key, label string }{
		{Qualification, "qualification"}, {Duplicate, "the duplicate check"},
	} {
		if !enabled(ds[d.key]) {
			lines = append(lines, fmt.Sprintf("%s is disabled: %s", d.label, ds[d.key].Reason))
		}
	}
	if !r.Complete {
		lines = append(lines, fmt.Sprintf("complete: false: %d items not done (%s)", c.NotDone,
			tally(notDone)))
	}
	return lines
}
