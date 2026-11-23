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

// dupInstructions are the dup_ question's instructions around the reply ID (§3.2 Duplicate).
const (
	dupInstructionsHead = "Does `memory_index` already cover every required rule of reply `"
	dupInstructionsTail = "`? First settle from the `owner` turn and the `assistant` message " +
		"which of the agent's changes the reply refers to and what it asks: a reply that undoes " +
		"or stops that change ('roll that back', 'なしにして') asks for the instruction the change " +
		"broke, from `owner` or a standing rule, plus any further instruction the reply states. A " +
		"rule is an action (what the agent must do or not do) under a condition (when it " +
		"applies); a rule that only sets this task's scope (what to change this time) is " +
		"task-local. The required rules are all the reply asks, leaving out a task-local rule " +
		"when the reply also asks a reusable rule that a line covers. A line covers a rule when " +
		"it states the same condition and the same action, in any language or wording; a line " +
		"requiring a result covers the fix that restores it in this case, while a method the " +
		"reply sets for every case is a rule of its own; a task-local rule is covered by a line " +
		"stating it as a standing rule or by a general line that applies to every case of its " +
		"kind. A line sharing a rule's topic, object or keyword does not cover it when it " +
		"governs another object of the same kind, applies only under another condition, asks a " +
		"different action about the same thing, or is a rule the agent's action also broke " +
		"while the reply asks for another."
)

// dupCriteria define the required rules once: false is true's complement, so `duplicate` needs
// positive evidence and `new` means coverage not established.
var dupCriteria = map[string]string{
	"true": "the conversation establishes at least one required rule, settles any referent " +
		"needed to interpret it, and for each required rule at least one line of the index " +
		"covers it; an explicit standing instruction needs no earlier change to refer to",
	"false": "any of these does not hold: no required rule is established, a referent needed " +
		"to read one is not settled, or a required rule has no covering line",
}

// DupQuestion is the noul question whether the index covers every required rule of reply id.
func DupQuestion(id string) jev.Question {
	return jev.Question{
		Name: "dup_" + id, Type: jev.TypeNoul,
		Instructions: dupInstructionsHead + id + dupInstructionsTail,
		Criteria:     dupCriteria,
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
