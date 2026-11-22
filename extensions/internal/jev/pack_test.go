package jev

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

var testLayout = Layout{
	UnitsKey: "diff", NameKey: "file", TextKey: "patch",
}

func testQuestions() []Question {
	return []Question{
		{Name: "local", Type: TypeNoul, Instructions: "local?"},
		{Name: "whole", Type: TypeNoul, Instructions: "whole?", WholeOnly: true},
	}
}

func withTitle(title string) Layout {
	l := testLayout
	l.Fixed = []Field{{Key: "title", Value: title}, {Key: "commits", Value: []string{"msg"}}}
	return l
}

type sentElem struct {
	File  string `json:"file"`
	Part  string `json:"part"`
	Patch string `json:"patch"`
}

type sentBody struct {
	Model     string                     `json:"model"`
	State     json.RawMessage            `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

// decode returns a batch's state size, its units and its question names, checking the body is
// the one hashed.
func decode(t *testing.T, b Batch) (int, []sentElem, []string) {
	t.Helper()
	if b.Hash != Hash(b.Body) {
		t.Fatalf("hash %s is not the body's", b.Hash)
	}
	var body sentBody
	if err := json.Unmarshal(b.Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Model != Pin {
		t.Fatalf("model %q, want %s", body.Model, Pin)
	}
	var state struct {
		Diff []sentElem `json:"diff"`
	}
	if err := json.Unmarshal(body.State, &state); err != nil {
		t.Fatal(err)
	}
	qs := make([]string, 0, len(body.Questions))
	for k := range body.Questions {
		qs = append(qs, k)
	}
	slices.Sort(qs)
	return len(body.State), state.Diff, qs
}

// reassemble joins every unit's pieces across batches (deduplicated per state) and checks the
// part framing.
func reassemble(t *testing.T, plan Plan, units []Unit) map[string]string {
	t.Helper()
	got := map[string]string{}
	seen := map[string]bool{}
	for _, b := range plan.Batches {
		_, elems, _ := decode(t, b)
		for i, e := range elems {
			key := e.File + "\x00" + e.Part
			if seen[key] {
				continue
			}
			seen[key] = true
			p := b.Pieces[i]
			if p.Parts > 0 && e.Part != fmt.Sprintf("part %d of %d", p.Part, p.Parts) {
				t.Fatalf("framing %q for piece %+v", e.Part, p)
			}
			if p.Parts == 0 && e.Part != "" {
				t.Fatalf("a whole unit carries framing %q", e.Part)
			}
			got[e.File] += e.Patch
		}
	}
	for _, u := range units {
		if !utf8Same(got[u.Name], u.Text) {
			t.Fatalf("unit %s lost content: got %d bytes, want %d", u.Name, len(got[u.Name]),
				len(u.Text))
		}
	}
	return got
}

// utf8Same compares texts after the JSON round trip, which replaces invalid bytes.
func utf8Same(got, want string) bool {
	return got == strings.ToValidUTF8(want, "\uFFFD") || got == want
}

func lines(prefix string, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

//nolint:gocyclo,maintidx // one subtest per packing claim of C3
func TestPack(t *testing.T) {
	t.Run("25 KiB texts with a small diff go whole", func(t *testing.T) {
		units := []Unit{{"a.go", "+x\n"}, {"b.go", "-y\n"}}
		plan, err := Pack(Input{
			Layout: withTitle(strings.Repeat("t", 25<<10)), Units: units,
			Questions: testQuestions(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Whole || len(plan.Batches) != 1 || len(plan.Unasked) != 0 {
			t.Fatalf("plan = whole %v, %d batches, unasked %v", plan.Whole, len(plan.Batches),
				plan.Unasked)
		}
		_, _, qs := decode(t, plan.Batches[0])
		if !slices.Equal(qs, []string{"local", "whole"}) {
			t.Fatalf("questions %v", qs)
		}
		reassemble(t, plan, units)
	})

	t.Run("a file over the budget is split into ordered parts", func(t *testing.T) {
		units := []Unit{{"a.go", "+a\n"}, {"big.go", lines("+line", 12000)}, {"c.go", "+c\n"}}
		plan, err := Pack(Input{
			Layout: withTitle("fix"), Units: units,
			Questions: testQuestions(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Whole || len(plan.Batches) < 3 || len(plan.Unpacked) != 0 {
			t.Fatalf("plan = whole %v, %d batches, unpacked %v", plan.Whole, len(plan.Batches),
				plan.Unpacked)
		}
		if !slices.Equal(plan.Unasked, []Unasked{{"whole", ReasonNotWhole}}) {
			t.Fatalf("unasked = %v", plan.Unasked)
		}
		var order []int
		for _, b := range plan.Batches {
			size, _, qs := decode(t, b)
			if size > StateBudget {
				t.Fatalf("state of %d bytes over %d", size, StateBudget)
			}
			if !slices.Equal(qs, []string{"local"}) {
				t.Fatalf("questions %v", qs)
			}
			for _, p := range b.Pieces {
				order = append(order, p.Unit*1000+p.Part)
			}
		}
		if !slices.IsSorted(order) {
			t.Fatalf("pieces out of unit order: %v", order)
		}
		reassemble(t, plan, units)
	})

	frame := func(t *testing.T, l Layout) int {
		t.Helper()
		p, err := statePrefix(l)
		if err != nil {
			t.Fatal(err)
		}
		return len(p) + len("]}")
	}
	// budgetLayout returns a layout whose unit budget under the default limit is budget.
	budgetLayout := func(t *testing.T, budget int) Layout {
		t.Helper()
		l := withTitle("")
		pad := StateBudget - budget - frame(t, l)
		l = withTitle(strings.Repeat("t", pad))
		if got := StateBudget - frame(t, l); got != budget {
			t.Fatalf("unit budget %d, want %d", got, budget)
		}
		return l
	}
	big := []Unit{{"a.go", lines("+a", 300)}, {"b.go", lines("+b", 300)}}

	t.Run("a budget just above 1 KiB splits", func(t *testing.T) {
		plan, err := Pack(Input{
			Layout: budgetLayout(t, MinUnitBudget), Units: big,
			Questions: testQuestions(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Batches) < 2 || len(plan.Unpacked) != 0 {
			t.Fatalf("%d batches, unpacked %v", len(plan.Batches), plan.Unpacked)
		}
		for _, b := range plan.Batches {
			if size, _, _ := decode(t, b); size > StateBudget {
				t.Fatalf("state of %d bytes", size)
			}
		}
		reassemble(t, plan, big)
	})

	t.Run("a budget just below 1 KiB packs nothing when splitting is needed", func(t *testing.T) {
		plan, err := Pack(Input{
			Layout: budgetLayout(t, MinUnitBudget-1), Units: big,
			Questions: testQuestions(),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []Unasked{{"local", ReasonSmallBudget}, {"whole", ReasonSmallBudget}}
		if len(plan.Batches) != 0 || !slices.Equal(plan.Unasked, want) ||
			!slices.Equal(plan.Unpacked, []int{0, 1}) {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("a small diff that fits whole goes whole under a 1 KiB budget", func(t *testing.T) {
		small := []Unit{{"a.go", "+a\n"}}
		plan, err := Pack(Input{
			Layout: budgetLayout(t, 200), Units: small,
			Questions: testQuestions(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Whole || len(plan.Batches) != 1 || len(plan.Unasked) != 0 {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("escaped runes and part framing at the boundary", func(t *testing.T) {
		//nolint:gosmopolitan // multi-byte runes at the part boundary
		chunk := "\"\\\t\x01\u2028日本語\xff<>&" + strings.Repeat("é", 7) + "\n"
		//nolint:gosmopolitan // multi-byte runes at the part boundary
		text := strings.Repeat(chunk, 3) + strings.Repeat("\"日\x02", 900) + "\n" + chunk
		units := []Unit{{"esc\"name\t.go", text}}
		const limit = 2 << 10
		plan, err := Pack(Input{
			Layout: withTitle("x"), Units: units,
			Questions: testQuestions(), StateLimit: limit,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Batches) < 2 || len(plan.Unpacked) != 0 {
			t.Fatalf("%d batches, unpacked %v", len(plan.Batches), plan.Unpacked)
		}
		full := 0
		for _, b := range plan.Batches {
			size, _, _ := decode(t, b)
			if size > limit {
				t.Fatalf("state of %d bytes over %d", size, limit)
			}
			if size > limit-64 {
				full++
			}
		}
		if full == 0 {
			t.Fatal("no part was packed close to the boundary")
		}
		reassemble(t, plan, units)
	})

	t.Run("questions over 16 KiB go to further requests on the same state", func(t *testing.T) {
		qs := make([]Question, 0, 60)
		for i := range 60 {
			qs = append(qs, Question{
				Name: fmt.Sprintf("q%02d", i), Type: TypeNoul,
				Instructions: strings.Repeat("why ", 100),
			})
		}
		units := []Unit{{"a.go", "+a\n"}}
		plan, err := Pack(Input{Layout: withTitle("x"), Units: units, Questions: qs})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Batches) < 2 {
			t.Fatalf("%d batches", len(plan.Batches))
		}
		var asked []string
		var state string
		for i, b := range plan.Batches {
			var body sentBody
			if err := json.Unmarshal(b.Body, &body); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body.Questions)
			if len(raw) > QuestionBudget {
				t.Fatalf("questions of %d bytes", len(raw))
			}
			if i > 0 && string(body.State) != state {
				t.Fatal("a question group got another state")
			}
			state = string(body.State)
			for _, q := range b.Questions {
				asked = append(asked, q.Name)
			}
		}
		if len(asked) != len(qs) {
			t.Fatalf("asked %d of %d questions", len(asked), len(qs))
		}
	})

	t.Run("a unit whose smallest part cannot fit is reported, the rest packed", func(t *testing.T) {
		units := []Unit{
			{"a.go", lines("+a", 100)},
			{strings.Repeat("n", 3000), lines("+n", 100)},
			{"c.go", lines("+c", 100)},
		}
		plan, err := Pack(Input{
			Layout: withTitle("x"), Units: units,
			Questions: testQuestions(), StateLimit: 2 << 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.Unpacked, []int{1}) {
			t.Fatalf("unpacked %v", plan.Unpacked)
		}
		got := reassemble(t, plan, []Unit{units[0], units[2]})
		if _, ok := got[units[1].Name]; ok {
			t.Fatal("the unpackable unit was sent")
		}
	})
}

func TestEncodedTextLen(t *testing.T) {
	for _, s := range []string{
		//nolint:gosmopolitan // multi-byte runes
		"", "plain", "\"\\/", "\b\f\n\r\t\x00\x1f\x7f", "<>&", "\u2028\u2029", "日本\xff\xfe",
		"é\U0001F600",
	} {
		enc, err := Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := encodedTextLen(s), len(enc)-2; got != want {
			t.Errorf("encodedTextLen(%q) = %d, want %d", s, got, want)
		}
	}
}
