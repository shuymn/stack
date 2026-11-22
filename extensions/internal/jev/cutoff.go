package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
)

// Calibration constants (§3.5); cutoffs are scanned in hundredths.
const (
	percent = 100
	// The cutoff scan of the margin rule.
	minCutoff = 5
	maxCutoff = 95
	// The scan of the old rule, kept as the reported baseline.
	minBaseline = 50
	maxBaseline = 99
	cutoffStep  = 1
	// Margin is the predeclared margin δ of the margin test, in hundredths (0.05).
	Margin       = 5
	minRecall    = 0.5
	unstableBand = 0.05
	minFitCases  = 6
	minHeldCases = 3
	// scoreEpsilon absorbs float error when a score is compared with a cutoff in hundredths.
	scoreEpsilon = 1e-9
)

// errTable reports a cutoff table that cannot be used.
var errTable = errors.New("invalid cutoff table")

// Hit is the one decision function: a score at or above the cutoff.
func Hit(p, cutoff float64) bool { return p >= cutoff }

// Sample is one single-call score of a scored unit with its ground truth. A nil Score is a planned
// call with no score (e.g. a size rejection): a negative prediction, never dropped.
type Sample struct {
	Case  string
	Score *float64
	Truth bool
}

// Stats counts a decision's outcomes at a cutoff.
//
//nolint:tagliatelle // the cutoff table uses snake_case keys, as the fixtures do.
type Stats struct {
	TP        int     `json:"tp"`
	FP        int     `json:"fp"`
	FN        int     `json:"fn"`
	TN        int     `json:"tn"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	Positives int     `json:"positive_cases"`
	Negatives int     `json:"negative_cases"`
}

// Evaluate scores samples at cutoff with the decision function.
func Evaluate(samples []Sample, cutoff float64) Stats {
	return count(samples, func(x Sample) bool { return Hit(*x.Score, cutoff) })
}

// MarginEvaluate scores samples at cutoff under the margin test: a positive is a hit only at
// cutoff+δ or above, a negative a false positive already at cutoff−δ. A missing score is a miss.
func MarginEvaluate(samples []Sample, cutoff float64) Stats {
	d := float64(Margin) / percent
	return count(samples, func(x Sample) bool {
		if x.Truth {
			return *x.Score >= cutoff+d-scoreEpsilon
		}
		return *x.Score >= cutoff-d-scoreEpsilon
	})
}

// count counts samples, hit deciding each scored one; an unscored one is a negative prediction.
func count(samples []Sample, hit func(Sample) bool) Stats {
	var s Stats
	pos, neg := map[string]bool{}, map[string]bool{}
	for _, x := range samples {
		hit := x.Score != nil && hit(x)
		switch {
		case hit && x.Truth:
			s.TP++
		case hit:
			s.FP++
		case x.Truth:
			s.FN++
		default:
			s.TN++
		}
		if x.Truth {
			pos[x.Case] = true
		} else {
			neg[x.Case] = true
		}
	}
	s.Positives, s.Negatives = len(pos), len(neg)
	if s.TP+s.FP > 0 {
		s.Precision = float64(s.TP) / float64(s.TP+s.FP)
	}
	if s.TP+s.FN > 0 {
		s.Recall = float64(s.TP) / float64(s.TP+s.FN)
	}
	return s
}

// FitCutoff is the lowest cutoff of 0.05–0.95 (step 0.01) that passes the margin test on fit:
// precision ≥ target, TP > 0 and recall ≥ 0.5 under MarginEvaluate; ok is false when none does.
// The runtime decision at the cutoff stays Hit.
func FitCutoff(fit []Sample, target float64) (float64, bool) {
	return scan(fit, target, minCutoff, maxCutoff, MarginEvaluate)
}

// BaselineCutoff is the old rule, reported for comparison: the lowest cutoff of 0.50–0.99 whose
// plain Evaluate on fit meets the same targets.
func BaselineCutoff(fit []Sample, target float64) (float64, bool) {
	return scan(fit, target, minBaseline, maxBaseline, Evaluate)
}

func scan(
	fit []Sample,
	target float64,
	lo, hi int,
	eval func([]Sample, float64) Stats,
) (float64, bool) {
	for k := lo; k <= hi; k += cutoffStep {
		c := float64(k) / percent
		if s := eval(fit, c); s.TP > 0 && s.Precision >= target && s.Recall >= minRecall {
			return c, true
		}
	}
	return 0, false
}

// Decision is one decision's calibrated state as the cutoff table holds it.
type Decision struct {
	Enabled bool     `json:"enabled"`
	Cutoff  *float64 `json:"cutoff"`
	Target  float64  `json:"target"`
	// Reason says why a decision is disabled.
	Reason  string `json:"reason,omitempty"`
	Fit     *Stats `json:"fit,omitempty"`
	Heldout *Stats `json:"heldout,omitempty"`
	// Baseline is the old rule's cutoff on the same fit samples, for comparison only.
	Baseline *float64 `json:"baseline,omitempty"`
	// Margin is the fit stats under the margin test at the cutoff.
	Margin *Stats `json:"margin,omitempty"`
	// Unstable lists samples within ±0.05 of the cutoff.
	Unstable []string `json:"unstable,omitempty"`
}

// Calibrate fits the cutoff on fit alone with the margin test, then checks the floors and held-out
// eligibility with the runtime decision: ≥ 6 + 6 positive and negative fit cases, ≥ 3 + 3 held-out
// ones, and the precision target and recall ≥ 0.5 on heldout. A decision failing any of them is
// disabled.
func Calibrate(fit, heldout []Sample, target float64) Decision {
	d := Decision{Target: target}
	if b, ok := BaselineCutoff(fit, target); ok {
		d.Baseline = &b
	}
	c, ok := FitCutoff(fit, target)
	if ok {
		d.Cutoff = &c
		ms := MarginEvaluate(fit, c)
		d.Margin = &ms
	}
	fs := Evaluate(fit, c)
	hs := Evaluate(heldout, c)
	d.Fit, d.Heldout = &fs, &hs
	switch {
	case fs.Positives < minFitCases || fs.Negatives < minFitCases:
		d.Reason = fmt.Sprintf("fit has %d positive and %d negative cases, under %d + %d",
			fs.Positives, fs.Negatives, minFitCases, minFitCases)
	case hs.Positives < minHeldCases || hs.Negatives < minHeldCases:
		d.Reason = fmt.Sprintf("heldout has %d positive and %d negative cases, under %d + %d",
			hs.Positives, hs.Negatives, minHeldCases, minHeldCases)
	case !ok:
		d.Reason = fmt.Sprintf("no cutoff of 0.05-0.95 passes the margin test (±%.2f) with "+
			"precision %.2f and recall %.2f on fit; review the corpus", float64(Margin)/percent,
			target, minRecall)
	case hs.TP == 0 || hs.Precision < target || hs.Recall < minRecall:
		d.Reason = fmt.Sprintf("heldout precision %.2f, recall %.2f miss the target %.2f / %.2f",
			hs.Precision, hs.Recall, target, minRecall)
	default:
		d.Enabled = true
	}
	if ok {
		for _, x := range slices.Concat(fit, heldout) {
			if x.Score != nil && math.Abs(*x.Score-c) <= unstableBand+1e-9 {
				d.Unstable = append(d.Unstable, fmt.Sprintf("%s: %.3f", x.Case, *x.Score))
			}
		}
	}
	return d
}

// Table is an extension's committed cutoff table, written by jev-calibrate.
type Table struct {
	Model string `json:"model"`
	// Placeholder, when set, marks a table not yet produced by a live calibration run.
	Placeholder string              `json:"placeholder,omitempty"`
	Decisions   map[string]Decision `json:"decisions"`
}

// ParseTable decodes a cutoff table; it must name the pin.
func ParseTable(data []byte) (Table, error) {
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return Table{}, fmt.Errorf("cutoff table: %w", err)
	}
	if t.Model != Pin {
		return Table{}, fmt.Errorf("%w: it names %q, not the pin %s", errTable, t.Model, Pin)
	}
	for name, d := range t.Decisions {
		if d.Enabled && d.Cutoff == nil {
			return Table{}, fmt.Errorf("%w: %s is enabled without a cutoff", errTable, name)
		}
	}
	return t, nil
}

// WriteTable writes t as indented JSON.
func WriteTable(path string, t Table) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cutoff table: %w", err)
	}
	// A committed source file, world-readable like the rest of the checkout.
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // G306
		return fmt.Errorf("write cutoff table: %w", err)
	}
	return nil
}
