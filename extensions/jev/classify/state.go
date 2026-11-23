package classify

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// Labels of the kind_ question (§3.2 Classification).
const (
	LabelCorrection = "correction"
	LabelDrift      = "drift"
	LabelApproval   = "approval"
	LabelRequest    = "request"
	LabelNoise      = "noise"
)

// Labels are the kind_ labels in the order the question lists them.
var Labels = []string{LabelCorrection, LabelDrift, LabelApproval, LabelRequest, LabelNoise}

var kindCriteria = map[string]string{
	LabelCorrection: "says the agent's approach, assumption, reading of the task or claim was " +
		"wrong, or states a standing rule for how the agent should work",
	LabelDrift: "brings the agent back to the scope or goal the owner set (see `owner`), after " +
		"`assistant` went past or away from it",
	LabelApproval: "accepts, permits, picks an offered option or acknowledges a result (OK, " +
		"merged, CI passed)",
	LabelRequest: "asks a question, reports a status or a defect in what was built, or gives a " +
		"new task or idea, without saying the agent worked wrongly",
	LabelNoise: "not about the agent's work: an input test, a lone ID, hash, link or attachment, " +
		"a fragment",
}

// KindQuestion is the choice question about reply id.
func KindQuestion(id string) jev.Question {
	return jev.Question{
		Name: "kind_" + id, Type: jev.TypeChoice,
		Instructions: "What does reply `" + id + "` in `items` do, given the `assistant` message " +
			"it answers and the `owner` turn before that message?",
		Criteria: kindCriteria,
		Labels:   Labels,
	}
}

// DupQuestion is the noul question whether the index already states what reply id asks.
func DupQuestion(id string) jev.Question {
	return jev.Question{
		Name: "dup_" + id, Type: jev.TypeNoul,
		Instructions: "Does a line of `memory_index` already state what reply `" + id +
			"` asks of the agent?",
		Criteria: map[string]string{
			"true": "a line states the way of working the reply corrects, or the scope or goal " +
				"it restores, in any language or wording",
			"false": "no line does; a line that only shares its topic or states a different rule " +
				"does not count",
		},
	}
}

// Order sorts resolved items newest first by observe's ts, ties in observe's item order, and
// numbers them r1, r2, … in that order (§3.2 Classification).
func Order(items []*Item) []*Item {
	out := slices.Clone(items)
	slices.SortStableFunc(out, func(a, b *Item) int {
		if c := b.ts.Compare(a.ts); c != 0 {
			return c
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return out
}

// ID is the sent id of the item at position i (0-based) of Order.
func ID(i int) string { return fmt.Sprintf("r%d", i+1) }

// KindInput packs the ordered resolved items, each whole with its own kind_ question
// (§3.1 unit-question mode).
func KindInput(ordered []*Item) jev.Input {
	units := make([]jev.Unit, len(ordered))
	for i, it := range ordered {
		id := ID(i)
		units[i] = jev.Unit{
			Name: id,
			Fields: []jev.Field{
				{Key: "owner", Value: it.Owner},
				{Key: "assistant", Value: it.Assistant},
				{Key: "reply", Value: it.Reply},
			},
			Questions: []jev.Question{KindQuestion(id)},
		}
	}
	return jev.Input{
		UnitsKey: "items", NameKey: "id",
		Units: units, Whole: true,
	}
}

// DupBatch is the dup_ request of one qualified item: state {memory_index, owner, assistant,
// reply}, each text whole. ok is false when it does not fit one request.
func DupBatch(id, index string, it *Item) (jev.Batch, bool, error) {
	return jev.Single([]jev.Field{
		{Key: "memory_index", Value: index},
		{Key: "owner", Value: it.Owner},
		{Key: "assistant", Value: it.Assistant},
		{Key: "reply", Value: it.Reply},
	}, []jev.Question{DupQuestion(id)})
}
