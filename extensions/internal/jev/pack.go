package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Question types the extensions ask.
const (
	TypeNoul   = "noul"
	TypeChoice = "choice"
)

// Question is one typed question of a request.
type Question struct {
	Name         string
	Type         string
	Instructions string
	// Criteria is sent as is when not nil.
	Criteria any
	// Labels are a choice question's labels, which its answer must use.
	Labels []string
	// WholeOnly asks the question only when the input fits one request whole (§3.2).
	WholeOnly bool
}

// Field is one fixed field of a state, sent in every request.
type Field struct {
	Key   string
	Value any
}

// Unit is one input of a state: a diff file, a kit, an item.
type Unit struct {
	Name string
	Text string
	// Fields are further keys of the unit's element, encoded in order after its name (and before
	// its text, when the layout has a text key).
	Fields []Field
	// Questions are asked only on the request carrying the unit (unit-question mode, Whole).
	Questions []Question
}

// Layout names the JSON keys of a state: the fixed fields come first, then the units array.
type Layout struct {
	Fixed    []Field
	UnitsKey string
	NameKey  string
	TextKey  string
}

// partKey frames a part of a split unit: "part k of n".
const partKey = "part"

// Input is what Pack packs.
type Input struct {
	Layout

	Units     []Unit
	Questions []Question
	// Model is the model sent; empty means Pin.
	Model string
	// StateLimit bounds the encoded state; zero means StateBudget.
	StateLimit int
	// QuestionLimit bounds the encoded questions; zero means QuestionBudget.
	QuestionLimit int
	// Whole never splits a unit: one that does not fit a request whole with the fixed fields is
	// unpacked and its questions unasked (ReasonUnitTooBig). Units are packed greedily in order
	// and a request closes when its next unit would pass the state or the question budget. It is
	// required for unit questions, which an input never mixes with shared ones.
	Whole bool
}

// errMixedQuestions reports an input with both shared and unit questions, unit questions
// without Whole, or a Whole unit with no question when there are no shared ones.
var errMixedQuestions = errors.New("unit questions need Whole and no shared questions")

// Piece is a unit, or part Part of Parts of it, carried by a batch.
type Piece struct {
	Unit  int
	Part  int
	Parts int
	// Text is the part of the unit's text this piece carries.
	Text string
}

// Batch is one request: its body, sent and hashed as is, and what it carries.
type Batch struct {
	Body      []byte
	Hash      string
	Pieces    []Piece
	Questions []Question
}

// Units returns the names of the units b carries, each once, in first-occurrence order.
func (b Batch) Units(units []Unit) []string {
	var names []string
	for _, p := range b.Pieces {
		if n := units[p.Unit].Name; !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// Unasked is a question asked on no request, and why.
type Unasked struct {
	Question string
	Reason   string
}

// Plan is the packing of one input: every body is determined by the input alone.
type Plan struct {
	Batches []Batch
	// Whole reports that the input went into one state unsplit.
	Whole bool
	// Unpacked lists the units no batch carries, too large to split under the budget; the
	// questions about them are unassessed.
	Unpacked []int
	// Unasked lists the questions no batch carries.
	Unasked []Unasked
}

// Reasons for unasked questions and unpacked units.
const (
	ReasonNotWhole       = "the input does not fit one request"
	ReasonSmallBudget    = "the fixed fields leave under 1 KiB for the input"
	ReasonPartTooBig     = "a smallest part does not fit one request"
	ReasonQuestionTooBig = "the question alone is over 16 KiB"
	ReasonUnitTooBig     = "the unit does not fit one request whole"
)

// Pack packs in's units into states of at most StateLimit encoded bytes and in's questions into
// groups of at most QuestionLimit (§3.1). Input that fits one state whole is sent whole. Otherwise
// units are packed greedily in order up to the unit budget (the limit less the encoded fixed
// fields), a unit larger than the budget split by lines, a longer line at UTF-8 boundaries, into
// parts marked "part k of n". Only when splitting is needed does a budget under MinUnitBudget pack
// nothing. A question over QuestionLimit by itself stays unasked.
func Pack(in Input) (Plan, error) {
	limit := orDefault(in.StateLimit, StateBudget)
	model := in.Model
	if model == "" {
		model = Pin
	}
	prefix, err := statePrefix(in.Layout)
	if err != nil {
		return Plan{}, err
	}
	frame := len(prefix) + len("]}")
	budget := limit - frame
	elems := make([][]byte, len(in.Units))
	total := 0
	unitQuestions, bare := false, false
	for i, u := range in.Units {
		if elems[i], err = in.element(u, "", u.Text); err != nil {
			return Plan{}, err
		}
		total += len(elems[i])
		unitQuestions = unitQuestions || len(u.Questions) > 0
		bare = bare || len(u.Questions) == 0
	}
	total += max(len(elems)-1, 0)
	// A Whole unit with no question of its own and no shared one would be dropped unasked.
	if unitQuestions && (!in.Whole || len(in.Questions) > 0) ||
		in.Whole && len(in.Questions) == 0 && bare {
		return Plan{}, errMixedQuestions
	}
	if in.Whole {
		return in.packWhole(model, prefix, elems, budget)
	}

	var plan Plan
	var states [][]Piece
	switch {
	case total <= budget:
		plan.Whole = true
		pieces := make([]Piece, len(in.Units))
		for i, u := range in.Units {
			pieces[i] = Piece{Unit: i, Text: u.Text}
		}
		states = [][]Piece{pieces}
	case budget < MinUnitBudget:
		for i := range in.Units {
			plan.Unpacked = append(plan.Unpacked, i)
		}
		for _, q := range in.Questions {
			plan.Unasked = append(plan.Unasked, Unasked{q.Name, ReasonSmallBudget})
		}
		return plan, nil
	default:
		states, plan.Unpacked, err = in.greedy(elems, budget)
		if err != nil {
			return Plan{}, err
		}
	}

	var asked []Question
	for _, q := range in.Questions {
		if q.WholeOnly && !plan.Whole {
			plan.Unasked = append(plan.Unasked, Unasked{q.Name, ReasonNotWhole})
			continue
		}
		asked = append(asked, q)
	}
	groups, oversize, err := groupQuestions(asked, orDefault(in.QuestionLimit, QuestionBudget))
	if err != nil {
		return Plan{}, err
	}
	for _, name := range oversize {
		plan.Unasked = append(plan.Unasked, Unasked{name, ReasonQuestionTooBig})
	}
	for _, pieces := range states {
		state, err := in.state(prefix, pieces)
		if err != nil {
			return Plan{}, err
		}
		for _, g := range groups {
			body := requestBody(model, state, g.encoded)
			plan.Batches = append(plan.Batches, Batch{
				Body: body, Hash: Hash(body), Pieces: pieces, Questions: g.questions,
			})
		}
	}
	return plan, nil
}

func orDefault(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

// greedy packs the units in order into states whose elements fit budget, splitting a unit larger
// than budget into parts. It returns the states and the units that cannot be split to fit.
func (in Input) greedy(elems [][]byte, budget int) ([][]Piece, []int, error) {
	var states [][]Piece
	var cur []Piece
	used := 0
	place := func(p Piece, size int) {
		need := size
		if len(cur) > 0 {
			need++
		}
		if len(cur) > 0 && used+need > budget {
			states = append(states, cur)
			cur, used, need = nil, 0, size
		}
		cur = append(cur, p)
		used += need
	}
	var unpacked []int
	for i, u := range in.Units {
		if len(elems[i]) <= budget {
			place(Piece{Unit: i, Text: u.Text}, len(elems[i]))
			continue
		}
		parts, ok, err := in.split(u, budget)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			unpacked = append(unpacked, i)
			continue
		}
		for k, text := range parts {
			p := Piece{Unit: i, Part: k + 1, Parts: len(parts), Text: text}
			elem, err := in.element(u, partLabel(p.Part, p.Parts), text)
			if err != nil {
				return nil, nil, err
			}
			place(p, len(elem))
		}
	}
	if len(cur) > 0 {
		states = append(states, cur)
	}
	return states, unpacked, nil
}

func partLabel(k, n int) string {
	return "part " + strconv.Itoa(k) + " of " + strconv.Itoa(n)
}

// split cuts u's text into the fewest greedy line-wise parts whose elements, framed "part k of n",
// each fit budget. ok is false when a smallest non-empty part cannot fit.
func (in Input) split(u Unit, budget int) ([]string, bool, error) {
	for width := 1; width <= len(strconv.Itoa(len(u.Text)+1)); width++ {
		nines := strings.Repeat("9", width)
		empty, err := in.element(u, "part "+nines+" of "+nines, "")
		if err != nil {
			return nil, false, err
		}
		room := budget - len(empty)
		parts, ok := splitText(u.Text, room)
		if !ok {
			return nil, false, nil
		}
		if len(strconv.Itoa(len(parts))) <= width {
			return parts, true, nil
		}
	}
	return nil, false, nil
}

// splitText cuts text into parts of at most room encoded bytes each: whole lines where they fit,
// a longer line at UTF-8 boundaries. ok is false when one character does not fit.
func splitText(text string, room int) ([]string, bool) {
	if room <= 0 {
		return nil, false
	}
	var parts []string
	start, size := 0, 0
	flush := func(end int) {
		if end > start {
			parts = append(parts, text[start:end])
		}
		start, size = end, 0
	}
	for i := 0; i < len(text); {
		end := strings.IndexByte(text[i:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += i + 1
		}
		line := encodedTextLen(text[i:end])
		switch {
		case size+line <= room:
			size += line
		case line <= room:
			flush(i)
			size = line
		default:
			flush(i)
			for j := i; j < end; {
				_, n := utf8.DecodeRuneInString(text[j:])
				c := runeCost(text[j:])
				if c > room {
					return nil, false
				}
				if size+c > room {
					flush(j)
				}
				size += c
				j += n
			}
		}
		i = end
	}
	flush(len(text))
	return parts, true
}

// encodedTextLen is the length of s encoded as a JSON string, without the quotes.
func encodedTextLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		n += runeCost(s[i:])
		i += size
	}
	return n
}

// runeCost is the encoded length of the first character of s as encoding/json writes it with HTML
// escaping off.
func runeCost(s string) int {
	b := s[0]
	if b < utf8.RuneSelf {
		switch {
		case b == '"' || b == '\\' || b == '\b' || b == '\f' || b == '\n' || b == '\r' ||
			b == '\t':
			return len(`\n`)
		case b < ' ':
			return len(`\u0000`)
		default:
			return 1
		}
	}
	r, size := utf8.DecodeRuneInString(s)
	switch {
	case r == utf8.RuneError && size == 1:
		return utf8.RuneLen(utf8.RuneError) // written as the replacement character
	case r == 0x2028 || r == 0x2029:
		return len(`\u0000`) // escaped line separators
	}
	return size
}

// statePrefix is `{"<fixed>":<value>,…,"<units>":[`.
func statePrefix(l Layout) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for _, f := range l.Fixed {
		if err := writeKV(&b, f.Key, f.Value); err != nil {
			return nil, err
		}
		b.WriteByte(',')
	}
	key, err := Marshal(l.UnitsKey)
	if err != nil {
		return nil, err
	}
	b.Write(key)
	b.WriteString(":[")
	return b.Bytes(), nil
}

// element encodes one unit or part: `{"<name>":…,"part":"part k of n",<fields>,"<text>":…}`,
// without the text when the layout has no text key.
func (in Input) element(u Unit, part, text string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	if err := writeKV(&b, in.NameKey, u.Name); err != nil {
		return nil, err
	}
	if part != "" {
		b.WriteByte(',')
		if err := writeKV(&b, partKey, part); err != nil {
			return nil, err
		}
	}
	for _, f := range u.Fields {
		b.WriteByte(',')
		if err := writeKV(&b, f.Key, f.Value); err != nil {
			return nil, err
		}
	}
	if in.TextKey != "" {
		b.WriteByte(',')
		if err := writeKV(&b, in.TextKey, text); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// packWhole is unit-question mode (§3.1): units in order, never split, each request closed when
// its next unit would pass the state budget or the question budget; a unit's questions ride only
// on the request carrying it. Shared questions, when given instead, go on every state.
func (in Input) packWhole(model string, prefix []byte, elems [][]byte, budget int) (Plan, error) {
	qlimit := orDefault(in.QuestionLimit, QuestionBudget)
	type state struct {
		pieces    []Piece
		used      int
		questions []Question
		encoded   []byte
	}
	var plan Plan
	var states []state
	var cur state
	flush := func() {
		if len(cur.pieces) > 0 {
			states = append(states, cur)
		}
		cur = state{}
	}
	for i, u := range in.Units {
		kvs, err := questionKVs(u.Questions)
		if err != nil {
			return Plan{}, err
		}
		if len(elems[i]) > budget || len("{}")+len(kvs) > qlimit {
			plan.Unpacked = append(plan.Unpacked, i)
			for _, q := range u.Questions {
				plan.Unasked = append(plan.Unasked, Unasked{q.Name, ReasonUnitTooBig})
			}
			continue
		}
		sep := func(n int) int {
			if n > 0 {
				return 1
			}
			return 0
		}
		need := len(elems[i]) + sep(len(cur.pieces))
		qneed := len(kvs)
		if len(kvs) > 0 {
			qneed += sep(len(cur.encoded))
		}
		if len(cur.pieces) > 0 &&
			(cur.used+need > budget || len("{}")+len(cur.encoded)+qneed > qlimit) {
			flush()
			need = len(elems[i])
		}
		cur.pieces = append(cur.pieces, Piece{Unit: i, Text: u.Text})
		cur.used += need
		if len(kvs) > 0 {
			if len(cur.encoded) > 0 {
				cur.encoded = append(cur.encoded, ',')
			}
			cur.encoded = append(cur.encoded, kvs...)
		}
		cur.questions = append(cur.questions, u.Questions...)
	}
	flush()
	plan.Whole = len(states) <= 1 && len(plan.Unpacked) == 0
	var groups []questionGroup
	if len(in.Questions) > 0 {
		var oversize []string
		var err error
		groups, oversize, err = groupQuestions(in.Questions, qlimit)
		if err != nil {
			return Plan{}, err
		}
		for _, name := range oversize {
			plan.Unasked = append(plan.Unasked, Unasked{name, ReasonQuestionTooBig})
		}
	}
	for _, st := range states {
		encoded, err := in.state(prefix, st.pieces)
		if err != nil {
			return Plan{}, err
		}
		add := func(questions []byte, qs []Question) {
			body := requestBody(model, encoded, questions)
			plan.Batches = append(plan.Batches, Batch{
				Body: body, Hash: Hash(body), Pieces: st.pieces, Questions: qs,
			})
		}
		for _, g := range groups {
			add(g.encoded, g.questions)
		}
		if len(st.questions) > 0 {
			add(slices.Concat([]byte("{"), st.encoded, []byte("}")), st.questions)
		}
	}
	return plan, nil
}

// questionKVs encodes questions as the members of a questions object, without its braces.
func questionKVs(qs []Question) ([]byte, error) {
	var b bytes.Buffer
	for i, q := range qs {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := writeKV(
			&b,
			q.Name,
			questionSpec{q.Type, q.Instructions, q.Criteria},
		); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

func (in Input) state(prefix []byte, pieces []Piece) ([]byte, error) {
	b := bytes.NewBuffer(bytes.Clone(prefix))
	for i, p := range pieces {
		if i > 0 {
			b.WriteByte(',')
		}
		part := ""
		if p.Parts > 0 {
			part = partLabel(p.Part, p.Parts)
		}
		elem, err := in.element(in.Units[p.Unit], part, p.Text)
		if err != nil {
			return nil, err
		}
		b.Write(elem)
	}
	b.WriteString("]}")
	return b.Bytes(), nil
}

func writeKV(b *bytes.Buffer, key string, value any) error {
	k, err := Marshal(key)
	if err != nil {
		return err
	}
	v, err := Marshal(value)
	if err != nil {
		return fmt.Errorf("field %s: %w", key, err)
	}
	b.Write(k)
	b.WriteByte(':')
	b.Write(v)
	return nil
}

type questionGroup struct {
	questions []Question
	encoded   []byte
}

type questionSpec struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// groupQuestions packs the questions, in order, into objects of at most limit encoded bytes. A
// question that alone exceeds limit is never sent; its name is returned in oversize.
func groupQuestions(qs []Question, limit int) ([]questionGroup, []string, error) {
	var groups []questionGroup
	var oversize []string
	var cur questionGroup
	flush := func() {
		if len(cur.questions) > 0 {
			cur.encoded = append(cur.encoded, '}')
			groups = append(groups, cur)
		}
		cur = questionGroup{}
	}
	for _, q := range qs {
		var kv bytes.Buffer
		if err := writeKV(
			&kv,
			q.Name,
			questionSpec{q.Type, q.Instructions, q.Criteria},
		); err != nil {
			return nil, nil, err
		}
		if len("{}")+kv.Len() > limit {
			oversize = append(oversize, q.Name)
			continue
		}
		if len(cur.questions) > 0 && len(cur.encoded)+1+kv.Len()+1 > limit {
			flush()
		}
		if len(cur.questions) == 0 {
			cur.encoded = append(cur.encoded, '{')
		} else {
			cur.encoded = append(cur.encoded, ',')
		}
		cur.encoded = append(cur.encoded, kv.Bytes()...)
		cur.questions = append(cur.questions, q)
	}
	flush()
	return groups, oversize, nil
}

// Single is one request whose state is the object of fields, in order, asking qs: classify's
// dup_ request. ok is false when the state or the questions pass their budget; nothing is sent
// then.
func Single(fields []Field, qs []Question) (Batch, bool, error) {
	var state bytes.Buffer
	state.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			state.WriteByte(',')
		}
		if err := writeKV(&state, f.Key, f.Value); err != nil {
			return Batch{}, false, err
		}
	}
	state.WriteByte('}')
	kvs, err := questionKVs(qs)
	if err != nil {
		return Batch{}, false, err
	}
	if state.Len() > StateBudget || len("{}")+len(kvs) > QuestionBudget {
		return Batch{}, false, nil
	}
	body := requestBody(Pin, state.Bytes(), slices.Concat([]byte("{"), kvs, []byte("}")))
	return Batch{Body: body, Hash: Hash(body), Questions: qs}, true, nil
}

func requestBody(model string, state, questions []byte) []byte {
	var b bytes.Buffer
	m, _ := Marshal(model)
	b.WriteString(`{"model":`)
	b.Write(m)
	b.WriteString(`,"state":`)
	b.Write(state)
	b.WriteString(`,"questions":`)
	b.Write(questions)
	b.WriteByte('}')
	return b.Bytes()
}

// Marshal encodes v as JSON without HTML escaping and without a trailing newline.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
