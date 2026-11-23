package jev

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// itemLayout is classify's layout: no fixed fields, items {id, owner, assistant, reply}.
var itemLayout = Layout{UnitsKey: "items", NameKey: "id"}

// item is a unit of unit-question mode with one choice question of instruction length qpad and
// a reply of rpad bytes.
func item(n, qpad, rpad int) Unit {
	id := fmt.Sprintf("r%d", n)
	return Unit{
		Name: id,
		Fields: []Field{
			{Key: "owner", Value: "scope " + id},
			{Key: "assistant", Value: "done " + id},
			{Key: "reply", Value: strings.Repeat("x", rpad)},
		},
		Questions: []Question{{
			Name: "kind_" + id, Type: TypeChoice,
			Instructions: strings.Repeat("q", qpad),
			Criteria:     map[string]string{"a": "A", "b": "B"},
			Labels:       []string{"a", "b"},
		}},
	}
}

// wantBody builds the body of a request carrying units, written out independently of Pack: the
// state's items in order, then each unit's questions in order.
func wantBody(t *testing.T, units ...Unit) string {
	t.Helper()
	enc := func(v any) string {
		b, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	elems := make([]string, 0, len(units))
	var qs []string
	for _, u := range units {
		var e strings.Builder
		e.WriteString(`{"id":` + enc(u.Name))
		for _, f := range u.Fields {
			e.WriteString(`,` + enc(f.Key) + `:` + enc(f.Value))
		}
		elems = append(elems, e.String()+`}`)
		for _, q := range u.Questions {
			qs = append(qs, enc(q.Name)+`:{"type":`+enc(q.Type)+`,"instructions":`+
				enc(q.Instructions)+`,"criteria":`+enc(q.Criteria)+`}`)
		}
	}
	return `{"model":"` + Pin + `","state":{"items":[` + strings.Join(elems, ",") +
		`]},"questions":{` + strings.Join(qs, ",") + `}}`
}

// membership is each batch's units and questions, by name.
func membership(plan Plan, units []Unit) []string {
	out := make([]string, 0, len(plan.Batches))
	for _, b := range plan.Batches {
		qs := make([]string, 0, len(b.Questions))
		for _, q := range b.Questions {
			qs = append(qs, q.Name)
		}
		out = append(out, strings.Join(b.Units(units), ",")+" | "+strings.Join(qs, ","))
	}
	return out
}

func bodies(plan Plan) []string {
	out := make([]string, 0, len(plan.Batches))
	for _, b := range plan.Batches {
		if b.Hash != Hash(b.Body) {
			panic("hash is not the body's")
		}
		out = append(out, string(b.Body))
	}
	return out
}

// TestPackUnits is C3's unit-question mode: units never split, a unit's questions only on the
// request carrying it, a request closed at whichever budget its next unit would pass first, and
// one disposition per unit.
//
//nolint:gocyclo,maintidx // one subtest per claim of C3's unit-question mode
func TestPackUnits(t *testing.T) {
	two := []Unit{item(1, 600, 40), item(2, 600, 40)}
	qsize := func(units ...Unit) int {
		var all []Question
		for _, u := range units {
			all = append(all, u.Questions...)
		}
		kvs, err := questionKVs(all)
		if err != nil {
			t.Fatal(err)
		}
		return len("{}") + len(kvs)
	}
	ssize := func(units ...Unit) int {
		in := Input{Layout: itemLayout, Units: units}
		pieces := make([]Piece, len(units))
		for i := range units {
			pieces[i] = Piece{Unit: i}
		}
		prefix, err := statePrefix(itemLayout)
		if err != nil {
			t.Fatal(err)
		}
		s, err := in.state(prefix, pieces)
		if err != nil {
			t.Fatal(err)
		}
		return len(s)
	}
	pack := func(t *testing.T, in Input) Plan {
		t.Helper()
		in.Layout, in.Whole = itemLayout, true
		plan, err := Pack(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range plan.Batches {
			size, _, _ := decode(t, b)
			if limit := orDefault(in.StateLimit, StateBudget); size > limit {
				t.Fatalf("state of %d bytes over %d", size, limit)
			}
		}
		return plan
	}
	for _, c := range []struct {
		name   string
		in     Input
		bodies [][]Unit
	}{
		{
			"question budget first, exactly at it",
			Input{Units: two, QuestionLimit: qsize(two...)},
			[][]Unit{two},
		},
		{
			"question budget first, one byte past it",
			Input{Units: two, QuestionLimit: qsize(two...) - 1},
			[][]Unit{{two[0]}, {two[1]}},
		},
		{
			"state budget first, exactly at it",
			Input{Units: two, StateLimit: ssize(two...)},
			[][]Unit{two},
		},
		{
			"state budget first, one byte past it",
			Input{Units: two, StateLimit: ssize(two...) - 1},
			[][]Unit{{two[0]}, {two[1]}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The other budget has room to spare, so only the named one closes the request.
			if c.in.QuestionLimit != 0 && ssize(two...) >= StateBudget/2 ||
				c.in.StateLimit != 0 && qsize(two...) >= QuestionBudget/2 {
				t.Fatal("the other budget is not slack")
			}
			plan := pack(t, c.in)
			want := make([]string, 0, len(c.bodies))
			wantMembers := make([]string, 0, len(c.bodies))
			for _, us := range c.bodies {
				want = append(want, wantBody(t, us...))
				names := make([]string, 0, len(us))
				qs := make([]string, 0, len(us))
				for _, u := range us {
					names = append(names, u.Name)
					qs = append(qs, u.Questions[0].Name)
				}
				wantMembers = append(wantMembers,
					strings.Join(names, ",")+" | "+strings.Join(qs, ","))
			}
			if got := bodies(plan); !slices.Equal(got, want) {
				t.Fatalf("bodies\n%q\nwant\n%q", got, want)
			}
			if got := membership(plan, c.in.Units); !slices.Equal(got, wantMembers) {
				t.Fatalf("membership %q, want %q", got, wantMembers)
			}
			if len(plan.Unpacked) != 0 || len(plan.Unasked) != 0 {
				t.Fatalf("unpacked %v, unasked %v", plan.Unpacked, plan.Unasked)
			}
		})
	}

	t.Run(
		"questions fill their budget before the state's at the default limits",
		func(t *testing.T) {
			units := make([]Unit, 0, 40)
			for i := range 40 {
				units = append(units, item(i, 780, 200))
			}
			plan := pack(t, Input{Units: units})
			seen := map[string]int{}
			for _, b := range plan.Batches {
				size, _, qs := decode(t, b)
				if size > StateBudget/2 {
					t.Fatalf("state of %d bytes: the state budget closed the request", size)
				}
				names := b.Units(units)
				if len(qs) != len(names) {
					t.Fatalf("questions %v for units %v", qs, names)
				}
				for _, n := range names {
					seen[n]++
					if !slices.Contains(qs, "kind_"+n) {
						t.Fatalf("unit %s without its question: %v", n, qs)
					}
				}
			}
			if len(plan.Batches) < 2 || len(seen) != len(units) {
				t.Fatalf(
					"%d batches carry %d of %d units",
					len(plan.Batches),
					len(seen),
					len(units),
				)
			}
			for n, k := range seen {
				if k != 1 {
					t.Fatalf("unit %s sent %d times", n, k)
				}
			}
		},
	)

	t.Run("a 60 KiB unit is unpacked, never split, its question unasked", func(t *testing.T) {
		units := []Unit{item(1, 10, 10), item(2, 10, 60<<10), item(3, 10, 10)}
		plan := pack(t, Input{Units: units})
		if !slices.Equal(plan.Unpacked, []int{1}) ||
			!slices.Equal(plan.Unasked, []Unasked{{"kind_r2", ReasonUnitTooBig}}) {
			t.Fatalf("unpacked %v, unasked %v", plan.Unpacked, plan.Unasked)
		}
		if got, want := bodies(plan), []string{wantBody(t, units[0], units[2])}; !slices.Equal(
			got,
			want,
		) {
			t.Fatalf("bodies %q, want %q", got, want)
		}
		if plan.Whole {
			t.Fatal("a plan with an unpacked unit is whole")
		}
	})

	t.Run("a unit whose questions pass the question budget alone is unpacked", func(t *testing.T) {
		units := []Unit{item(1, QuestionBudget, 10), item(2, 10, 10)}
		plan := pack(t, Input{Units: units})
		if !slices.Equal(plan.Unpacked, []int{0}) ||
			!slices.Equal(membership(plan, units), []string{"r2 | kind_r2"}) {
			t.Fatalf("unpacked %v, membership %v", plan.Unpacked, membership(plan, units))
		}
	})

	t.Run("unit questions never mix with shared ones or go without Whole", func(t *testing.T) {
		for _, in := range []Input{
			{Layout: itemLayout, Units: two, Whole: true, Questions: testQuestions()},
			{Layout: itemLayout, Units: two},
		} {
			if _, err := Pack(in); !errors.Is(err, errMixedQuestions) {
				t.Fatalf("err %v", err)
			}
		}
	})

	t.Run("a Whole unit with no question and no shared one is refused", func(t *testing.T) {
		bare := item(2, 10, 10)
		bare.Questions = nil
		in := Input{Layout: itemLayout, Units: []Unit{bare}, Whole: true}
		if plan, err := Pack(in); !errors.Is(err, errMixedQuestions) {
			t.Fatalf("plan %+v, err %v", plan, err)
		}
	})

	t.Run("an element encodes its fields in order after its name", func(t *testing.T) {
		plan := pack(t, Input{Units: []Unit{item(7, 1, 3)}})
		if !strings.Contains(string(plan.Batches[0].Body),
			`{"id":"r7","owner":"scope r7","assistant":"done r7","reply":"xxx"}`) {
			t.Fatalf("body %s", plan.Batches[0].Body)
		}
	})
}
