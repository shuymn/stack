package classify

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// The dup_ question as §3.2 states it, written out here rather than taken from the code.
const (
	wantDupInstructions = "Does `memory_index` already cover every required rule of reply `r7`? " +
		"First settle from the `owner` turn and the `assistant` message which of the agent's " +
		"changes the reply refers to and what it asks: a reply that undoes or stops that change " +
		"('roll that back', 'なしにして') asks for the instruction the change broke, from `owner` " +
		"or a standing rule, plus any further instruction the reply states. A rule is an action " +
		"(what the agent must do or not do) under a condition (when it applies); a rule that only " +
		"sets this task's scope (what to change this time) is task-local. The required rules are " +
		"all the reply asks, leaving out a task-local rule when the reply also asks a reusable " +
		"rule that a line covers. A line covers a rule when it states the same condition and the " +
		"same action, in any language or wording; a line requiring a result covers the fix that " +
		"restores it in this case, while a method the reply sets for every case is a rule of its " +
		"own; a task-local rule is covered by a line stating it as a standing rule or by a general " +
		"line that applies to every case of its kind. A line sharing a rule's topic, object or " +
		"keyword does not cover it when it governs another object of the same kind, applies only " +
		"under another condition, asks a different action about the same thing, or is a rule the " +
		"agent's action also broke while the reply asks for another."
	wantDupTrue = "the conversation establishes at least one required rule, settles any " +
		"referent needed to interpret it, and for each required rule at least one line of the " +
		"index covers it; an explicit standing instruction needs no earlier change to refer to"
	wantDupFalse = "any of these does not hold: no required rule is established, a referent " +
		"needed to read one is not settled, or a required rule has no covering line"
)

// TestDupQuestion is C9's dup_r<n> request: the state keys memory_index, owner, assistant, reply
// in that order, each text whole, and one question with the name, type, instructions and both
// criteria exactly as §3.2 states them.
func TestDupQuestion(t *testing.T) {
	it := &Item{
		Owner: "Fix the seed script only.", Assistant: "I edited migration 0042.",
		Reply: "revert that",
	}
	index := "- Never edit an applied migration.\n"
	b, ok, err := DupBatch("r7", index, it)
	if err != nil || !ok {
		t.Fatalf("DupBatch: ok %v, %v", ok, err)
	}
	var body struct {
		State     json.RawMessage `json:"state"`
		Questions map[string]struct {
			Type         string            `json:"type"`
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(b.Body, &body); err != nil {
		t.Fatal(err)
	}
	keys, values := stateKeys(t, body.State)
	if !slices.Equal(keys, []string{"memory_index", "owner", "assistant", "reply"}) ||
		!slices.Equal(values, []string{index, it.Owner, it.Assistant, it.Reply}) {
		t.Fatalf("state %s", body.State)
	}
	q, ok := body.Questions["dup_r7"]
	if len(body.Questions) != 1 || !ok || q.Type != "noul" {
		t.Fatalf("questions %+v", body.Questions)
	}
	if q.Instructions != wantDupInstructions {
		t.Fatalf("instructions\n%s\nwant\n%s", q.Instructions, wantDupInstructions)
	}
	if want := map[string]string{"true": wantDupTrue, "false": wantDupFalse}; !reflect.DeepEqual(
		q.Criteria, want,
	) {
		t.Fatalf("criteria\n%q\nwant\n%q", q.Criteria, want)
	}
}

// stateKeys returns the keys of a JSON object in order, with their string values.
func stateKeys(t *testing.T, raw json.RawMessage) ([]string, []string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("state %s: %v", raw, err)
	}
	var keys, values []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		key, _ := k.(string)
		keys, values = append(keys, key), append(values, v)
	}
	return keys, values
}
