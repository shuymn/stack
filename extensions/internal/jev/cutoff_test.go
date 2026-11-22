package jev

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// cases makes n cases with one sample each, named prefix-i.
func cases(prefix string, truth bool, scores ...float64) []Sample {
	s := make([]Sample, 0, len(scores))
	for i, p := range scores {
		s = append(s, Sample{Case: fmt.Sprintf("%s-%d", prefix, i), Score: new(p), Truth: truth})
	}
	return s
}

//nolint:gocyclo // one subtest per claim of C10
func TestCutoff(t *testing.T) {
	fit := slices.Concat(
		cases("pos", true, 0.95, 0.9, 0.85, 0.8, 0.75, 0.6),
		cases("neg", false, 0.1, 0.2, 0.3, 0.55, 0.72, 0.4),
	)
	held := slices.Concat(
		cases("hpos", true, 0.9, 0.85, 0.8),
		cases("hneg", false, 0.1, 0.2, 0.3),
	)

	t.Run("lowest cutoff passing the margin test on fit alone", func(t *testing.T) {
		c, ok := FitCutoff(fit, 0.8)
		// Under the margin test a negative counts from t-0.05 and a positive from t+0.05. Up to
		// 0.60 the 0.55 negative and the 0.72 one both count: precision at most 6/8. At 0.61
		// only 0.72 does, and five positives reach 0.66: precision 5/6, recall 5/6. The plain
		// rule (score >= t) would take 0.56.
		if !ok || c != 0.61 {
			t.Fatalf("cutoff %v ok %v, want 0.61", c, ok)
		}
		if b, ok := BaselineCutoff(fit, 0.8); !ok || b != 0.56 {
			t.Fatalf("baseline %v ok %v, want 0.56", b, ok)
		}
		m := MarginEvaluate(fit, c)
		if m.TP != 5 || m.FP != 1 || m.FN != 1 || m.TN != 5 {
			t.Fatalf("margin stats %+v", m)
		}
	})

	t.Run("a score at either margin boundary counts", func(t *testing.T) {
		pos := cases("pos", true, 0.7, 0.7, 0.7, 0.7)
		// Negatives at 0.59 count up to 0.64, so 0.65 qualifies only if a positive at exactly
		// 0.65+0.05 is a hit.
		if c, ok := FitCutoff(slices.Concat(pos, cases("neg", false, 0.59, 0.59)), 0.8); !ok ||
			c != 0.65 {
			t.Fatalf("positive boundary: cutoff %v ok %v, want 0.65", c, ok)
		}
		// Negatives at 0.55 count at exactly 0.60-0.05, so 0.60 fails and 0.61 is the first.
		if c, ok := FitCutoff(slices.Concat(pos, cases("neg", false, 0.55, 0.55)), 0.8); !ok ||
			c != 0.61 {
			t.Fatalf("negative boundary: cutoff %v ok %v, want 0.61", c, ok)
		}
	})

	t.Run("the scan runs from 0.50 to 0.95", func(t *testing.T) {
		// The margin test passes from 0.16 here, but a score under 0.50 leans no: the floor holds.
		low := slices.Concat(cases("pos", true, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9),
			cases("neg", false, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1))
		if c, ok := FitCutoff(low, 0.8); !ok || c != 0.5 {
			t.Fatalf("cutoff %v ok %v, want 0.50", c, ok)
		}
		// Clearing negatives at 0.92 needs 0.98, past the scan.
		high := slices.Concat(cases("pos", true, 1, 1, 1, 1, 1, 1),
			cases("neg", false, 0.92, 0.92, 0.92, 0.92, 0.92, 0.92))
		if c, ok := FitCutoff(high, 0.8); ok {
			t.Fatalf("cutoff %v past the scan", c)
		}
	})

	t.Run("decision function at, below and above the cutoff", func(t *testing.T) {
		if Hit(0.55, 0.56) || !Hit(0.56, 0.56) || !Hit(0.57, 0.56) {
			t.Fatal("Hit is not p >= cutoff")
		}
	})

	t.Run("a missing score stays a miss", func(t *testing.T) {
		withNil := slices.Concat(fit, []Sample{{Case: "pos-x", Truth: true}, {Case: "neg-x"}})
		s := Evaluate(withNil, 0.61)
		m := MarginEvaluate(withNil, 0.61)
		if s.FN != 2 || s.TN != 6 || m.FN != 2 || m.TN != 6 {
			t.Fatalf("stats %+v, margin %+v: a missing score was not a miss", s, m)
		}
		// Enough unscored positives drop recall under 0.5 at every cutoff.
		many := make([]Sample, 0, 8)
		for i := range 8 {
			many = append(many, Sample{Case: fmt.Sprintf("u-%d", i), Truth: true})
		}
		if _, ok := FitCutoff(slices.Concat(fit, many), 0.8); ok {
			t.Fatal("unscored positives were dropped")
		}
	})

	t.Run("held-out scores never move the fitted cutoff and can disable", func(t *testing.T) {
		d := Calibrate(fit, held, 0.8)
		if !d.Enabled || d.Cutoff == nil || *d.Cutoff != 0.61 {
			t.Fatalf("decision %+v", d)
		}
		if d.Baseline == nil || *d.Baseline != 0.56 || d.Margin == nil || d.Margin.TP != 5 {
			t.Fatalf("baseline %v, margin %+v", d.Baseline, d.Margin)
		}
		bad := slices.Concat(cases("hpos", true, 0.9, 0.3, 0.2), cases("hneg", false, 0.9, 0.95,
			0.99))
		d2 := Calibrate(fit, bad, 0.8)
		if d2.Enabled || d2.Cutoff == nil || *d2.Cutoff != 0.61 ||
			!strings.Contains(d2.Reason, "heldout precision") {
			t.Fatalf("decision %+v", d2)
		}
	})

	t.Run("under a floor it is disabled", func(t *testing.T) {
		d := Calibrate(fit[1:], held, 0.8)
		if d.Enabled || !strings.Contains(d.Reason, "fit has 5 positive") {
			t.Fatalf("fit floor: %+v", d)
		}
		d = Calibrate(fit, held[1:], 0.8)
		if d.Enabled || !strings.Contains(d.Reason, "heldout has 2 positive") {
			t.Fatalf("heldout floor: %+v", d)
		}
	})

	t.Run("no cutoff passing the margin test disables", func(t *testing.T) {
		// The plain rule finds 0.61 here; the margin test finds nothing.
		near := slices.Concat(cases("pos", true, 0.65, 0.65, 0.65, 0.65, 0.65, 0.65),
			cases("neg", false, 0.6, 0.6, 0.6, 0.6, 0.6, 0.6))
		d := Calibrate(near, held, 0.8)
		if d.Enabled || d.Cutoff != nil || !strings.Contains(d.Reason, "no cutoff") ||
			d.Baseline == nil || *d.Baseline != 0.61 {
			t.Fatalf("decision %+v", d)
		}
	})

	t.Run("samples near the cutoff are listed unstable", func(t *testing.T) {
		d := Calibrate(fit, held, 0.8)
		if !slices.Contains(d.Unstable, "pos-5: 0.600") ||
			slices.Contains(d.Unstable, "pos-0: 0.950") {
			t.Fatalf("unstable %v", d.Unstable)
		}
	})
}

func TestTable(t *testing.T) {
	if _, err := ParseTable([]byte(`{"model":"jev-latest","decisions":{}}`)); err == nil {
		t.Fatal("a table naming another model was accepted")
	}
	if _, err := ParseTable(
		[]byte(`{"model":"jev-1.14.0","decisions":{"a":{"enabled":true}}}`),
	); err == nil {
		t.Fatal("an enabled decision without a cutoff was accepted")
	}
}
