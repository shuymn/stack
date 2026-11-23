package classify

import (
	_ "embed"
	"strconv"
	"strings"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// Decision names in the cutoff table (§3.5).
const (
	Qualification = "qualification"
	Duplicate     = "duplicate"
)

// cutoffsJSON is the table jev-calibrate writes.
//
//go:embed cutoffs.json
var cutoffsJSON []byte

// testCutoffs, set only by test builds (`-ldflags -X`), replaces the table: e.g.
// "qualification=0.5,duplicate=0.5"; a decision it leaves out is disabled, and "none" disables
// both. It is never configuration.
var testCutoffs = ""

// Decisions returns the decisions this binary ships: the test cutoffs of a test build, else the
// committed table. A decision missing from the table is disabled.
func Decisions() map[string]jev.Decision {
	if testCutoffs != "" {
		return ParseTestCutoffs(testCutoffs)
	}
	t, err := jev.ParseTable(cutoffsJSON)
	ds := map[string]jev.Decision{}
	for _, name := range []string{Qualification, Duplicate} {
		d, ok := t.Decisions[name]
		switch {
		case err != nil:
			d = jev.Decision{Reason: "the cutoff table is unreadable"}
		case !ok:
			d = jev.Decision{Reason: "not calibrated"}
		}
		ds[name] = d
	}
	return ds
}

// ParseTestCutoffs reads "name=cutoff,…" or "none"; decisions it does not name are disabled.
func ParseTestCutoffs(s string) map[string]jev.Decision {
	ds := map[string]jev.Decision{
		Qualification: {Reason: "disabled in this test build"},
		Duplicate:     {Reason: "disabled in this test build"},
	}
	for kv := range strings.SplitSeq(s, ",") {
		k, v, _ := strings.Cut(kv, "=")
		c, err := strconv.ParseFloat(v, 64)
		if k == "" || k == "none" {
			continue
		}
		if err != nil {
			ds[k] = jev.Decision{Reason: "invalid test cutoff"}
			continue
		}
		ds[k] = jev.Decision{Enabled: true, Cutoff: &c}
	}
	return ds
}

// enabled reports a decision that can decide.
func enabled(d jev.Decision) bool { return d.Enabled && d.Cutoff != nil }

// Score is the qualification score: p(correction) + p(drift), so a reply that both corrects and
// restores scope cannot split its probability below the cutoff.
func Score(p map[string]float64) float64 {
	return p[LabelCorrection] + p[LabelDrift]
}

// Qualify is the one qualification decision, used at runtime, in calibration and on held-out
// cases: the summed score at or above the cutoff. kind is the larger of the two labels,
// correction on a tie; it is advisory.
func Qualify(p map[string]float64, cutoff float64) (bool, string) {
	kind := LabelCorrection
	if p[LabelDrift] > p[LabelCorrection] {
		kind = LabelDrift
	}
	return jev.Hit(Score(p), cutoff), kind
}
