package semantic

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// Finding kinds of hits.
const (
	KindMetaDuty    = "jev-meta-duty"
	KindUnsupported = "jev-unsupported-claim"
)

// name prefixes context lines.
const name = "jev-semantic"

// cutoffsJSON is the table jev-calibrate writes.
//
//go:embed cutoffs.json
var cutoffsJSON []byte

// testCutoffs, set only by test builds (`-ldflags -X`), replaces the table with enabled cutoffs:
// "meta_duty_regression=0.5,unsupported_claim=0.5". It is never configuration.
var testCutoffs = ""

// Decisions returns the decisions this binary ships: the test cutoffs of a test build, else the
// committed table. A decision missing from the table is disabled.
func Decisions() map[string]jev.Decision {
	if testCutoffs != "" {
		return parseTestCutoffs(testCutoffs)
	}
	t, err := jev.ParseTable(cutoffsJSON)
	ds := map[string]jev.Decision{}
	for _, q := range Questions {
		d, ok := t.Decisions[q.Name]
		switch {
		case err != nil:
			d = jev.Decision{Reason: "the cutoff table is unreadable"}
		case !ok:
			d = jev.Decision{Reason: "not calibrated"}
		}
		ds[q.Name] = d
	}
	return ds
}

func parseTestCutoffs(s string) map[string]jev.Decision {
	ds := map[string]jev.Decision{}
	for kv := range strings.SplitSeq(s, ",") {
		k, v, _ := strings.Cut(kv, "=")
		c, err := strconv.ParseFloat(v, 64)
		if err != nil {
			ds[k] = jev.Decision{Reason: "invalid test cutoff"}
			continue
		}
		ds[k] = jev.Decision{Enabled: true, Cutoff: &c}
	}
	return ds
}

// AllDisabled reports that no decision can flag, so nothing is worth sending.
func AllDisabled(ds map[string]jev.Decision) bool {
	for _, q := range Questions {
		if ds[q.Name].Enabled {
			return false
		}
	}
	return true
}

// Disabled is the answer when every decision is disabled: pass, nothing sent, and the context
// says why.
func Disabled(ds map[string]jev.Decision) jev.Response {
	reasons := make([]string, 0, len(Questions))
	for _, q := range Questions {
		reasons = append(reasons, q.Name+": "+ds[q.Name].Reason)
	}
	r := &jev.Reply{}
	r.Line(fmt.Sprintf("%s: every decision is disabled (%s); nothing was sent", name,
		strings.Join(reasons, "; ")))
	return r.Response()
}

// Evaluate sends a plan's batches and answers from their outcomes; with every decision disabled
// it sends nothing. jev-calibrate refreshes cal fixtures through it with a replay transport.
func Evaluate(
	ctx context.Context, plan jev.Plan, units []jev.Unit, t jev.Transport,
	ds map[string]jev.Decision, stderr io.Writer,
) jev.Response {
	if AllDisabled(ds) {
		return Disabled(ds)
	}
	return Respond(plan, units, jev.Do(ctx, t, plan.Batches), ds, stderr)
}

// group collects the requests left unassessed for one cause.
type group struct {
	cause     string
	requests  int
	questions []string
	files     []string
}

// Respond turns a plan's outcomes into the response. Unassessed work never changes the status;
// a hit, a persistent failure and a missing record warn.
func Respond(
	plan jev.Plan, units []jev.Unit, outcomes []jev.Outcome, ds map[string]jev.Decision,
	stderr io.Writer,
) jev.Response {
	r := &jev.Reply{}
	var groups []*group
	byCause := map[string]*group{}
	errSeen := map[string]bool{}
	for i, b := range plan.Batches {
		o := jev.Outcome{Class: jev.Deadline, Cause: jev.CauseDeadline}
		if i < len(outcomes) {
			o = outcomes[i]
		}
		files := b.Units(units)
		where := strings.Join(files, ", ")
		switch {
		case o.Class == jev.OK:
			for _, q := range b.Questions {
				if cause, bad := o.Invalid[q.Name]; bad {
					detail := q.Name + ": " + cause
					if !errSeen[detail] {
						errSeen[detail] = true
						r.Warn(jev.Finding{Kind: jev.KindError, Where: where, Detail: detail},
							fmt.Sprintf("%s: Jev answered %s: %s", name, detail, action(o)))
					}
					continue
				}
				p := o.Answers[q.Name].Noul
				d := ds[q.Name]
				if !d.Enabled || d.Cutoff == nil || !jev.Hit(p, *d.Cutoff) {
					continue
				}
				detail := fmt.Sprintf("p=%s ≥ %s (%s)", num(p), num(*d.Cutoff), jev.Pin)
				if q.Name == MetaDuty {
					r.Warn(jev.Finding{Kind: KindMetaDuty, Where: where, Detail: detail},
						fmt.Sprintf("%s: %s: the diff may remove or weaken a test, check or rule "+
							"without the PR text saying why (%s)", name, where, detail))
				} else {
					r.Warn(jev.Finding{Kind: KindUnsupported, Where: "message", Detail: detail},
						fmt.Sprintf("%s: the PR text claims something the diff does not show (%s)",
							name, detail))
				}
			}
		case o.Class.Unassessed():
			c := o.Describe()
			g := byCause[c]
			if g == nil {
				g = &group{cause: c}
				byCause[c] = g
				groups = append(groups, g)
			}
			g.requests++
			for _, q := range b.Questions {
				if !slices.Contains(g.questions, q.Name) {
					g.questions = append(g.questions, q.Name)
				}
			}
			g.files = appendNew(g.files, files...)
		case o.Class == jev.Unrecorded:
			r.Warn(jev.Finding{Kind: jev.KindUnrecorded, Where: where, Detail: o.Cause},
				fmt.Sprintf("%s: no recorded answer for the request on %s: re-record the fixture "+
					"with jev-calibrate", name, where))
		default:
			c := o.Describe()
			if errSeen[c] {
				continue
			}
			errSeen[c] = true
			r.Warn(jev.Finding{Kind: jev.KindError, Detail: c},
				fmt.Sprintf("%s: Jev answered %s: %s", name, c, action(o)))
		}
	}
	for _, g := range groups {
		where := strings.Join(g.files, ", ")
		detail := fmt.Sprintf("%s on %d of %d requests: %s", strings.Join(g.questions, ", "),
			g.requests, len(plan.Batches), g.cause)
		r.Note(jev.Finding{Kind: jev.KindUnassessed, Where: where, Detail: detail},
			fmt.Sprintf("%s: not assessed: %s (%s)", name, detail, where))
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "jev: %d requests skipped: %s\n", g.requests, g.cause)
		}
	}
	for _, u := range plan.Unasked {
		where := "diff"
		if u.Question == Unsupported {
			where = "message"
		}
		detail := u.Question + ": " + u.Reason
		r.Note(jev.Finding{Kind: jev.KindUnassessed, Where: where, Detail: detail},
			fmt.Sprintf("%s: not assessed: %s", name, detail))
	}
	if len(plan.Unpacked) > 0 {
		files := make([]string, 0, len(plan.Unpacked))
		for _, i := range plan.Unpacked {
			files = append(files, units[i].Name)
		}
		where := strings.Join(files, ", ")
		detail := MetaDuty + ": " + jev.ReasonPartTooBig
		if len(plan.Batches) == 0 {
			detail = MetaDuty + ": " + jev.ReasonSmallBudget
		}
		if !slices.ContainsFunc(plan.Unasked, func(u jev.Unasked) bool {
			return u.Question == MetaDuty
		}) {
			r.Note(jev.Finding{Kind: jev.KindUnassessed, Where: where, Detail: detail},
				fmt.Sprintf("%s: not assessed: %s (%s)", name, detail, where))
		}
	}
	if len(plan.Batches) > 0 {
		for _, q := range Questions {
			if d := ds[q.Name]; !d.Enabled {
				r.Line(fmt.Sprintf("%s: %s is disabled: %s", name, q.Name, d.Reason))
			}
		}
	}
	return r.Response()
}

// action is what the owner should do about a persistent failure.
func action(o jev.Outcome) string {
	switch {
	case o.Status == http.StatusUnauthorized || o.Status == http.StatusForbidden:
		return "check " + jev.KeyEnv
	case o.Status == http.StatusPaymentRequired:
		return "check the TypeSafe account's billing"
	case o.Status == http.StatusNotFound || o.Cause == jev.CauseModel:
		return "the pinned model " + jev.Pin + " may be retired; bump the pin"
	default:
		return "the Jev API may have changed; check its docs and status"
	}
}

func appendNew(list []string, items ...string) []string {
	for _, s := range items {
		if !slices.Contains(list, s) {
			list = append(list, s)
		}
	}
	return list
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
