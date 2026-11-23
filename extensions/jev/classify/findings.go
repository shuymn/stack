package classify

import (
	"fmt"
	"net/http"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// unassessedReasons are the work reasons reported as jev-unassessed: work the run could not do
// this time, which changes no status.
var unassessedReasons = []string{
	ReasonPrepDeadline, ReasonDeadline, ReasonTransient, ReasonTooLarge,
}

// findings adds the persistent failures (warn jev-error), the replay misses (warn
// jev-unrecorded) and the unassessed work (jev-unassessed, no status change), with one stderr
// line per skipped cause.
func (r *run) findings(rep *Report) {
	for _, f := range r.errs {
		detail := fmt.Sprintf("%s: %s (%d items)", f.phase, f.cause, f.items)
		r.reply.Warn(jev.Finding{Kind: jev.KindError, Where: f.phase, Detail: detail},
			fmt.Sprintf("%s: Jev returned %s for %s requests: %s", name, f.cause, f.phase,
				action(f.outcome)))
	}
	if r.unrecorded > 0 {
		r.reply.Warn(jev.Finding{
			Kind: jev.KindUnrecorded, Where: "requests",
			Detail: fmt.Sprintf("%d requests: %s", r.unrecorded, jev.CauseUnrecorded),
		}, fmt.Sprintf("%s: no recorded answer for %d requests: re-record the fixture with "+
			"jev-calibrate", name, r.unrecorded))
	}
	kinds, dups := map[string]int{}, map[string]int{}
	for _, it := range rep.Items {
		if it.Status == StatusUnevaluated {
			kinds[it.Reason]++
		}
		if it.Duplicate == DupUnassessed {
			dups[it.DupReason]++
		}
	}
	for _, reason := range unassessedReasons {
		if n := kinds[reason]; n > 0 {
			r.reply.Note(jev.Finding{
				Kind: jev.KindUnassessed, Where: "kind_",
				Detail: fmt.Sprintf("kind_ on %d items: %s", n, reason),
			}, "")
		}
	}
	for _, reason := range unassessedReasons {
		if n := dups[reason]; n > 0 {
			r.reply.Note(jev.Finding{
				Kind: jev.KindUnassessed, Where: "dup_",
				Detail: fmt.Sprintf("dup_ on %d items: %s", n, reason),
			}, "")
		}
	}
	for _, cause := range r.skippedOrder {
		_, _ = fmt.Fprintf(r.env.Stderr, "jev: %d requests skipped: %s\n", r.skipped[cause], cause)
	}
}

// action is what the owner should do about a persistent failure.
func action(o jev.Outcome) string {
	switch {
	case o.Status == http.StatusUnauthorized || o.Status == http.StatusForbidden:
		return "verify the Jev API key in " + jev.KeyEnv + " if set and non-blank, otherwise in " +
			jev.KeyDisplay
	case o.Status == http.StatusPaymentRequired:
		return "check the TypeSafe account's billing"
	case o.Status == http.StatusNotFound || o.Cause == jev.CauseModel:
		return "the pinned model " + jev.Pin + " may be retired; bump the pin"
	default:
		return "the Jev API may have changed; check its docs and status"
	}
}
