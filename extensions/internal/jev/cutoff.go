package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
)

// Calibration constants (§3.5).
const (
	percent      = 100
	minCutoff    = 50 // hundredths
	maxCutoff    = 99
	minRecall    = 0.5
	unstableBand = 0.05
	minFitCases  = 6
	minHeldCases = 3
	cutoffStep   = 1
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
	var s Stats
	pos, neg := map[string]bool{}, map[string]bool{}
	for _, x := range samples {
		hit := x.Score != nil && Hit(*x.Score, cutoff)
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

// FitCutoff is the lowest cutoff of 0.50–0.99 (step 0.01) whose precision on fit meets target
// with TP > 0 and recall ≥ 0.5; ok is false when none does.
func FitCutoff(fit []Sample, target float64) (float64, bool) {
	for k := minCutoff; k <= maxCutoff; k += cutoffStep {
		c := float64(k) / percent
		s := Evaluate(fit, c)
		if s.TP > 0 && s.Precision >= target && s.Recall >= minRecall {
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
	// Unstable lists samples within ±0.05 of the cutoff.
	Unstable []string `json:"unstable,omitempty"`
}

// Calibrate fits the cutoff on fit alone, then checks the floors and held-out eligibility: ≥ 6 + 6
// positive and negative fit cases, ≥ 3 + 3 held-out ones, and the precision target and recall ≥ 0.5
// on heldout. A decision failing any of them is disabled.
func Calibrate(fit, heldout []Sample, target float64) Decision {
	d := Decision{Target: target}
	c, ok := FitCutoff(fit, target)
	if ok {
		d.Cutoff = &c
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
		d.Reason = fmt.Sprintf("no cutoff reaches precision %.2f with recall %.2f on fit",
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
