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

func TestCutoff(t *testing.T) {
	fit := slices.Concat(
		cases("pos", true, 0.95, 0.9, 0.85, 0.8, 0.75, 0.6),
		cases("neg", false, 0.1, 0.2, 0.3, 0.55, 0.72, 0.4),
	)
	held := slices.Concat(
		cases("hpos", true, 0.9, 0.85, 0.8),
		cases("hneg", false, 0.1, 0.2, 0.3),
	)

	t.Run("lowest cutoff meeting precision with TP and recall on fit alone", func(t *testing.T) {
		c, ok := FitCutoff(fit, 0.8)
		// At 0.73 precision is 5/5 with recall 5/6; at 0.56..0.72 the 0.72 negative counts.
		if !ok || c != 0.56 {
			// 0.56: TP 6 (incl. 0.6), FP 1 (0.72) → precision 6/7 = 0.857 ≥ 0.8.
			t.Fatalf("cutoff %v ok %v, want 0.56", c, ok)
		}
		s := Evaluate(fit, c)
		if s.TP != 6 || s.FP != 1 || s.Precision < 0.8 || s.Recall < 0.5 {
			t.Fatalf("stats %+v", s)
		}
	})

	t.Run("decision function at, below and above the cutoff", func(t *testing.T) {
		if Hit(0.55, 0.56) || !Hit(0.56, 0.56) || !Hit(0.57, 0.56) {
			t.Fatal("Hit is not p >= cutoff")
		}
	})

	t.Run("an unscored planned call is a negative prediction", func(t *testing.T) {
		withNil := slices.Concat(fit, []Sample{{Case: "pos-x", Truth: true}})
		s := Evaluate(withNil, 0.56)
		if s.FN != 1 || s.TP != 6 {
			t.Fatalf("stats %+v: the unscored positive was not a false negative", s)
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
		if !d.Enabled || d.Cutoff == nil || *d.Cutoff != 0.56 {
			t.Fatalf("decision %+v", d)
		}
		bad := slices.Concat(cases("hpos", true, 0.9, 0.3, 0.2), cases("hneg", false, 0.9, 0.95,
			0.99))
		d2 := Calibrate(fit, bad, 0.8)
		if d2.Enabled || d2.Cutoff == nil || *d2.Cutoff != 0.56 ||
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

	t.Run("no cutoff reaching the target disables", func(t *testing.T) {
		noisy := slices.Concat(cases("pos", true, 0.6, 0.6, 0.6, 0.6, 0.6, 0.6),
			cases("neg", false, 0.7, 0.7, 0.7, 0.7, 0.7, 0.7))
		d := Calibrate(noisy, held, 0.8)
		if d.Enabled || d.Cutoff != nil || !strings.Contains(d.Reason, "no cutoff") {
			t.Fatalf("decision %+v", d)
		}
	})

	t.Run("samples near the cutoff are listed unstable", func(t *testing.T) {
		d := Calibrate(fit, held, 0.8)
		if !slices.Contains(d.Unstable, "pos-5: 0.600") {
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
