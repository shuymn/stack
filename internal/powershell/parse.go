package powershell

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/shuymn/stack/internal/paths"
	"github.com/shuymn/stack/internal/shell"
)

// maxNest bounds bracket nesting, so hostile input cannot exhaust the stack.
const maxNest = 200

// maxCdSteps and maxCwdLen bound the known cwd: past either, the cwd is unknown, so a long
// `cd x &&` chain cannot build ever longer strings.
const (
	maxCdSteps = 32
	maxCwdLen  = 4096
)

// verbatimMark is the stop-parsing token.
const verbatimMark = "--%"

// mergeRe matches the rest of a merging redirection after its stream number (`2>&1`).
var mergeRe = regexp.MustCompile(`^>&[1-6]`)

type syntaxError struct {
	pos int
	msg string
}

func (e *syntaxError) Error() string {
	return fmt.Sprintf("powershell: %s at offset %d", e.msg, e.pos)
}

type parser struct {
	src   string
	pos   int
	os    OS
	home  string
	depth int
	// quiet counts the enclosing opaque blocks: inside one nothing is recorded.
	quiet int
	nest  int
	cwd   string
	// moved reports a location change in the current statement.
	moved bool
	// assigned holds the known variables the script has assigned so far.
	assigned map[Known]bool
	groups   int
	// cdSteps counts the `Set-Location` targets resolved so far.
	cdSteps int
	res     Result
}

func newParser(src, cwd, home string, os OS, depth int, assigned map[Known]bool) *parser {
	return &parser{src: src, os: os, home: home, depth: depth, cwd: cwd, assigned: assigned}
}

func (p *parser) run() (err error) {
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(*syntaxError)
			if !ok {
				panic(r)
			}
			err = se
		}
	}()
	p.statements(0)
	return nil
}

func (p *parser) fail(msg string) {
	panic(&syntaxError{pos: p.pos, msg: msg})
}

// enter spends one level of the nesting budget; at the cap the whole command fails open.
func (p *parser) enter() {
	if p.nest++; p.nest > maxNest {
		p.fail("nesting too deep")
	}
}

func (p *parser) leave() { p.nest-- }

// statements parses statements up to end (`)` or `}`, left unconsumed) or EOF when end is 0.
func (p *parser) statements(end byte) {
	for {
		p.skipLines()
		if p.peek() == ';' {
			p.pos++
			continue
		}
		if p.eof() {
			if end != 0 {
				p.fail("unclosed bracket")
			}
			return
		}
		if c := p.peek(); end != 0 && c == end {
			return
		} else if c == ')' || c == '}' {
			p.fail("unexpected closing bracket")
		}
		before := p.pos
		p.statement()
		if p.pos == before {
			p.fail("unexpected token")
		}
	}
}

// statement parses one statement. A location change in it makes the cwd unknown afterwards.
// Every recursion (brackets, blocks, hash values, assignment right sides) passes through here or
// bracket, so the nesting budget bounds the stack.
func (p *parser) statement() {
	p.enter()
	defer p.leave()
	saved, start := p.moved, p.cwd
	p.moved = false
	if !p.keyword() {
		p.chain()
	}
	if p.moved || p.cwd != start {
		p.cwd, p.moved = "", true
	}
	p.moved = p.moved || saved
}

// keyword parses a statement that starts with a language keyword and reports whether it did.
func (p *parser) keyword() bool {
	if p.peek() == ':' && isLetter(p.at(p.pos+1)) { // a loop label
		p.pos++
		for isIdent(p.peek()) {
			p.pos++
		}
		p.skipBlank()
	}
	kw := p.keywordAhead()
	switch kw {
	case "if":
		p.pos += len(kw)
		p.ifStatement()
	case "while", "for", "foreach":
		p.pos += len(kw)
		p.skipBlank()
		if p.peek() == '-' { // `foreach -parallel` in a workflow
			p.argWord(Native)
		}
		p.cond()
		p.body(true)
	case "do":
		p.pos += len(kw)
		p.doStatement()
	case "switch":
		p.pos += len(kw)
		p.switchStatement()
	case "try":
		p.pos += len(kw)
		p.body(false)
		p.handlers()
	case "trap":
		p.pos += len(kw)
		p.skipBlank()
		if p.peek() == '[' {
			p.bracket()
		}
		p.body(false)
	case "function", "filter", "workflow", "configuration", "class", "enum", "data":
		p.pos += len(kw)
		p.opaqueDefinition()
	case "param":
		p.pos += len(kw)
		p.skipLines()
		if p.peek() != '(' {
			p.fail("param without (")
		}
		p.quietly(p.paren)
	case "begin", "process", "end", "dynamicparam", "clean":
		save := p.pos
		p.pos += len(kw)
		p.skipLines()
		if p.peek() != '{' {
			p.pos = save
			return false
		}
		p.block()
	case "return", "throw", "exit":
		p.pos += len(kw)
		p.skipBlank()
		if !p.atEnd() {
			p.chain()
		}
	case "break", "continue":
		p.pos += len(kw)
		p.skipBlank()
		if !p.atEnd() {
			p.argWord(Native) // the label
		}
	case "using":
		p.pos += len(kw)
		p.quietly(p.using)
	case "else", "elseif", "catch", "finally", "until":
		p.fail("unexpected " + kw)
	default:
		return false
	}
	return true
}

// using consumes the words of a `using` directive, quotes and brackets included, up to the end of
// its statement.
func (p *parser) using() {
	for {
		p.skipBlank()
		if p.atEnd() {
			return
		}
		p.argWord(Native)
	}
}

// ifStatement parses the rest of an `if` with its `elseif` and `else` clauses.
func (p *parser) ifStatement() {
	p.cond()
	p.body(false)
	for {
		save := p.pos
		p.skipLines()
		switch next := p.keywordAhead(); next {
		case "elseif":
			p.pos += len(next)
			p.cond()
			p.body(false)
		case "else":
			p.pos += len(next)
			p.body(false)
		default:
			p.pos = save
			return
		}
	}
}

// doStatement parses the rest of a `do … while|until (…)` loop.
func (p *parser) doStatement() {
	p.body(true)
	p.skipLines()
	next := p.keywordAhead()
	if next != "while" && next != "until" {
		p.fail("do without while or until")
	}
	p.pos += len(next)
	p.cond()
}

// cond parses the `(…)` of a control-flow statement.
func (p *parser) cond() {
	p.skipLines()
	if p.peek() != '(' {
		p.fail("missing (")
	}
	p.paren()
}

// body parses and walks a control-flow body. A loop body that changes location runs with an
// unknown cwd, since a later iteration starts where the previous one ended.
func (p *parser) body(loop bool) {
	p.skipLines()
	start, end, moved := p.block()
	cwd := p.cwd
	if loop && moved {
		cwd = ""
	}
	p.walk(start, end, cwd)
}

// handlers parses the catch and finally clauses after a try body.
func (p *parser) handlers() {
	for {
		save := p.pos
		p.skipLines()
		switch kw := p.keywordAhead(); kw {
		case "catch":
			p.pos += len(kw)
			for {
				p.skipLines()
				if p.peek() != '[' {
					break
				}
				p.bracket()
				p.skipBlank()
				if p.peek() == ',' {
					p.pos++
				}
			}
			p.body(false)
		case "finally":
			p.pos += len(kw)
			p.body(false)
		default:
			p.pos = save
			return
		}
	}
}

func (p *parser) switchStatement() {
	for {
		p.skipBlank()
		if p.peek() != '-' {
			break
		}
		opt := p.argWord(Native)
		if strings.EqualFold(opt.Text, "-file") {
			p.skipBlank()
			p.argWord(Native)
		}
	}
	p.skipLines()
	if p.peek() == '(' {
		p.paren()
	}
	p.skipLines()
	if p.peek() != '{' {
		p.fail("switch without body")
	}
	p.pos++
	for {
		p.skipLines()
		if p.peek() == ';' {
			p.pos++
			continue
		}
		if p.eof() {
			p.fail("unclosed switch")
		}
		if p.peek() == '}' {
			p.pos++
			return
		}
		if p.peek() == '{' {
			p.block() // a condition block is not a body
		} else {
			p.argWord(Native)
		}
		p.body(true)
	}
}

// opaqueDefinition skips a function, class, enum or data definition up to and including its body.
func (p *parser) opaqueDefinition() {
	for {
		p.skipLines()
		switch {
		case p.eof():
			p.fail("definition without body")
		case p.peek() == '{':
			p.block()
			return
		case p.peek() == '(':
			p.quietly(p.paren)
		default:
			p.argWord(Native)
		}
	}
}

// quietly runs f inside an opaque context. f gets a cd-step budget of its own, and the caller's
// cwd state is restored after it.
func (p *parser) quietly(f func()) {
	moved, cwd, steps := p.moved, p.cwd, p.cdSteps
	p.cdSteps = 0
	p.quiet++
	f()
	p.quiet--
	p.moved, p.cwd, p.cdSteps = moved, cwd, steps
}

// block lexes a `{…}` at pos without recording anything and returns its body range and whether it
// holds a location change.
func (p *parser) block() (int, int, bool) {
	if p.peek() != '{' {
		p.fail("missing {")
	}
	p.pos++
	start := p.pos
	moved := false
	p.quietly(func() {
		p.moved = false
		p.statements('}')
		moved = p.moved
	})
	end := p.pos
	p.pos++
	return start, end, moved
}

// walk parses a block body recorded by block, running in cwd.
func (p *parser) walk(start, end int, cwd string) {
	if p.quiet > 0 {
		return
	}
	pos, saved := p.pos, p.cwd
	p.pos, p.cwd = start, cwd
	p.statements('}')
	if p.pos != end {
		p.fail("block mismatch")
	}
	p.pos, p.cwd = pos, saved
}

// bracket skips a `[…]` type literal, attribute or index, walking the subexpressions inside.
func (p *parser) bracket() {
	p.enter()
	defer p.leave()
	p.pos++
	var b wordBuilder
	for {
		p.skipLines()
		switch c := p.peek(); {
		case p.eof():
			p.fail("unclosed [")
		case c == ']':
			p.pos++
			return
		case c == '[':
			p.bracket()
		case c == '(':
			p.paren()
		case c == '{':
			p.block()
		case c == '$':
			p.dollar(&b, false)
		case p.quoteAt(singleQuotes) > 0:
			p.single(&b, p.quoteAt(singleQuotes))
		case p.quoteAt(doubleQuotes) > 0:
			p.double(&b, p.quoteAt(doubleQuotes))
		default:
			p.pos++
		}
	}
}

// hash parses a `@{…}` literal: each key's value is a statement.
func (p *parser) hash() {
	p.pos += 2
	for {
		p.skipLines()
		if p.peek() == ';' {
			p.pos++
			continue
		}
		if p.eof() {
			p.fail("unclosed @{")
		}
		if p.peek() == '}' {
			p.pos++
			return
		}
		p.hashKey()
		p.skipBlank()
		if p.peek() != '=' {
			p.fail("missing = in @{")
		}
		p.pos++
		p.skipLines()
		p.statement()
	}
}

func (p *parser) hashKey() {
	var b wordBuilder
	start := p.pos
	switch c := p.peek(); {
	case p.quoteAt(singleQuotes) > 0:
		p.single(&b, p.quoteAt(singleQuotes))
	case p.quoteAt(doubleQuotes) > 0:
		p.double(&b, p.quoteAt(doubleQuotes))
	case c == '(':
		p.paren()
	case c == '$':
		p.dollar(&b, false)
	default:
		for !p.eof() && !isBlank(p.peek()) && strings.IndexByte("=\n;}", p.peek()) < 0 {
			p.pos++
		}
	}
	if p.pos == start {
		p.fail("missing key in @{")
	}
}

// elem is what a pipeline element tells its pipeline.
type elem struct {
	// text is the value of an element that is exactly one static string.
	text *string
	// cd reports a `Set-Location X` whose target is cdDir ("" when unknown).
	cd    bool
	cdDir string
}

// chain parses pipelines joined by `&&` and `||`, with an optional trailing `&`. Only `cd X &&`
// carries a known cwd to what follows.
func (p *parser) chain() {
	for {
		e := p.pipeline()
		p.skipBlank()
		switch {
		case p.has("&&"):
			p.pos += 2
			if e.cd {
				p.cwd, p.moved = e.cdDir, false
			} else if p.moved {
				p.cwd = ""
			}
		case p.has("||"):
			p.pos += 2
			if p.moved || e.cd {
				p.cwd = ""
			}
		default:
			if p.peek() == '&' {
				p.pos++
			}
			return
		}
		p.skipLines()
	}
}

func (p *parser) pipeline() elem {
	stdin := shell.Stdin{}
	for n := 0; ; n++ {
		p.skipBlank()
		e := p.element(stdin)
		p.skipBlank()
		if p.peek() != '|' || p.has("||") {
			if n == 0 {
				return e
			}
			return elem{}
		}
		p.pos++
		p.skipLines()
		stdin = shell.Stdin{Kind: shell.StdinDynamic}
		if e.text != nil {
			stdin = shell.Stdin{Kind: shell.StdinText, Text: *e.text + "\n"}
		}
	}
}

func (p *parser) element(stdin shell.Stdin) elem {
	c := p.peek()
	switch {
	case c == '&' && !p.has("&&"):
		p.pos++
		return p.invoke(stdin, false)
	case c == '.' && (p.boundary(p.pos+1) || strings.IndexByte(`'"{$(`, p.at(p.pos+1)) >= 0):
		p.pos++
		return p.invoke(stdin, true)
	case p.atEnd():
		return elem{}
	case p.exprStart():
		return p.expression()
	}
	return p.command(p.argWord(Native), stdin, false)
}

// exprStart reports whether the element at pos is an expression rather than a command.
func (p *parser) exprStart() bool {
	c := p.peek()
	switch {
	case strings.IndexByte("$([@-+!,{><", c) >= 0:
		return true
	case p.quoteAt(singleQuotes) > 0, p.quoteAt(doubleQuotes) > 0:
		return true
	case isDigit(c):
		return p.numberAhead()
	default:
		return false
	}
}

// invoke parses the operand of the `&` or `.` call operator.
func (p *parser) invoke(stdin shell.Stdin, dot bool) elem {
	p.skipBlank()
	if p.peek() == '{' {
		start, end, _ := p.block()
		p.walk(start, end, p.cwd)
		p.args(Native) // the block's arguments: walked for subexpressions only
		return elem{}
	}
	if p.atEnd() {
		p.fail("missing command after call operator")
	}
	return p.command(p.argWord(Native), stdin, dot)
}

// expression parses an expression element: no command of its own, but its subexpressions are
// walked, and in assignment position the right side is a statement.
func (p *parser) expression() elem {
	st := exprState{lvalue: p.peek() == '$' || p.peek() == '['}
	ntok := 0
	for {
		p.skipBlank()
		if p.atEnd() {
			break
		}
		if st.lvalue && ntok > 0 {
			if n := assignOp(p.src[p.pos:]); n > 0 {
				p.pos += n
				p.skipLines()
				p.statement()
				if st.lvars == 1 && st.lv != NotKnown && p.quiet == 0 {
					p.assigned[st.lv] = true
				}
				return elem{}
			}
		}
		ntok++
		p.exprToken(&st)
	}
	if ntok == 1 {
		return elem{text: st.text}
	}
	return elem{}
}

// exprState tracks an expression element while its tokens are read.
type exprState struct {
	// lvalue reports that the tokens so far can be an assignment target.
	lvalue bool
	// lv is the known variable of the first variable token; lvars counts variable and member
	// tokens, so only a plain `$X =` assigns X.
	lv    Known
	lvars int
	// text is the value of the last static string token.
	text *string
}

// exprToken reads one expression token, walking the subexpressions in it.
func (p *parser) exprToken(st *exprState) {
	var b wordBuilder
	c := p.peek()
	keepsLvalue := false
	switch {
	case c == '>' || ((isDigit(c) || c == '*') && p.at(p.pos+1) == '>'):
		p.redirection()
	case c == '(':
		p.paren()
	case p.has("@{"):
		p.hash()
	case p.has("@("):
		p.pos++
		p.paren()
	case p.has("@'"), p.has(`@"`):
		p.hereString(&b)
		st.text = staticText(b.word(""))
	case c == '{':
		p.block()
	case c == '[':
		p.bracket()
		keepsLvalue = true
	case c == '$':
		start := p.pos
		p.dollar(&b, false)
		if st.lvars++; st.lvars == 1 {
			st.lv = knownName(strings.Trim(p.src[start+1:p.pos], "{}"))
		}
		keepsLvalue = p.at(start+1) != '('
	case p.quoteAt(singleQuotes) > 0:
		p.single(&b, p.quoteAt(singleQuotes))
		st.text = staticText(b.word(""))
	case p.quoteAt(doubleQuotes) > 0:
		p.double(&b, p.quoteAt(doubleQuotes))
		st.text = staticText(b.word(""))
	case c == ',':
		p.pos++
		keepsLvalue = true
	case c == '`':
		p.pos++
		if p.eof() {
			p.fail("trailing backtick")
		}
		p.char(&b)
	default:
		if tok := p.genericToken(); strings.HasPrefix(tok, ".") || strings.HasPrefix(tok, "::") {
			st.lvars++ // a member: the target is no plain variable
			keepsLvalue = true
			if p.peek() == '(' {
				p.methodArgs()
				keepsLvalue = false
			}
		}
	}
	st.lvalue = st.lvalue && keepsLvalue
}

// genericToken reads an operator, number, bareword or member token of an expression.
func (p *parser) genericToken() string {
	start := p.pos
	for !p.eof() && !isBlank(p.peek()) &&
		strings.IndexByte("\n;|&(){}[]'\"$@,=>`", p.peek()) < 0 &&
		p.quoteAt(singleQuotes[1:]) == 0 && p.quoteAt(doubleQuotes[1:]) == 0 {
		p.pos++
	}
	if p.pos == start {
		p.pos++
	}
	return p.src[start:p.pos]
}

// assignOp returns the length of an assignment operator at the start of s, or 0.
func assignOp(s string) int {
	for _, op := range []string{"??=", "+=", "-=", "*=", "/=", "%=", "="} {
		if strings.HasPrefix(s, op) {
			return len(op)
		}
	}
	return 0
}

func staticText(w Word) *string {
	if !w.Static {
		return nil
	}
	return &w.Text
}

// redirection consumes a redirection at pos and reports whether there was one. A file target is
// walked for subexpressions and dropped; a merging redirection (`2>&1`) has no target.
func (p *parser) redirection() bool {
	i := p.pos
	switch c := p.peek(); {
	case (isDigit(c) || c == '*') && p.at(i+1) == '>':
		i++
	case c == '<':
		p.fail("the < operator is reserved")
	case c != '>':
		return false
	}
	if loc := mergeRe.FindStringIndex(p.src[i:]); loc != nil {
		p.pos = i + loc[1]
		return true
	}
	i++
	if p.at(i) == '>' {
		i++
	}
	p.pos = i
	p.skipBlank()
	if p.atEnd() {
		p.fail("missing redirection target")
	}
	p.argWord(Native)
	return true
}

// args lexes a command's arguments up to the end of its pipeline element.
func (p *parser) args(kind Kind) []item {
	var items []item
	for {
		p.skipBlank()
		if p.atEnd() {
			return items
		}
		if p.redirection() {
			continue
		}
		if p.has(verbatimMark) && p.boundary(p.pos+len(verbatimMark)) {
			p.pos += len(verbatimMark)
			for _, w := range p.verbatim() {
				items = append(items, item{words: []Word{w}})
			}
			return items
		}
		if kind == Cmdlet {
			if it, ok := p.paramItem(); ok {
				items = append(items, it)
				continue
			}
		}
		items = append(items, p.valueItem(kind))
	}
}

// paramItem lexes a cmdlet parameter token at pos: a dash and a name, with an optional `:`-bound
// value, or the `--` that ends parameters.
func (p *parser) paramItem() (item, bool) {
	start := p.pos
	dash := 0
	for _, d := range dashes {
		if p.has(d) {
			dash = len(d)
			break
		}
	}
	if dash == 0 {
		return item{}, false
	}
	if p.has("--") && p.boundaryOrEnd(p.pos+len("--")) {
		p.pos += len("--")
		return item{
			param: true,
			words: []Word{{Raw: "--", Text: "--", Static: true, IsParam: true}},
		}, true
	}
	if c := p.at(p.pos + dash); !isLetter(c) && c != '_' && c != '?' {
		return item{}, false
	}
	p.pos += dash
	for !p.eof() && !isBlank(p.peek()) && strings.IndexByte("\n;|&)}{(,:'\"$>", p.peek()) < 0 {
		p.pos++
	}
	if p.peek() != ':' {
		name := p.src[start:p.pos]
		return item{
			param: true,
			words: []Word{{Raw: name, Text: name, Static: true, IsParam: true}},
		}, true
	}
	p.pos++
	flag := p.src[start:p.pos]
	p.skipBlank()
	if p.atEnd() {
		p.fail("missing value after " + flag)
	}
	it := p.valueItem(Cmdlet)
	it.param, it.colon = true, true
	it.words[0].Flag = flag
	it.words[0].Raw = flag + it.words[0].Raw
	return it, true
}

// boundaryOrEnd reports whether i ends a token or an element.
func (p *parser) boundaryOrEnd(i int) bool {
	return p.boundary(i) || strings.IndexByte(";|&)}", p.src[i]) >= 0
}

// valueItem lexes one argument; for a cmdlet an unquoted `,` list is one argument of elements.
func (p *parser) valueItem(kind Kind) item {
	if kind == Cmdlet && p.peek() == ',' { // unary comma: a one-element list
		p.pos++
		p.skipBlank()
	}
	words := []Word{p.argWord(kind)}
	if kind != Cmdlet {
		return item{words: words}
	}
	for {
		save := p.pos
		p.skipBlank()
		if p.peek() != ',' {
			p.pos = save
			break
		}
		p.pos++
		p.skipLines()
		words = append(words, p.argWord(kind))
	}
	if len(words) > 1 {
		p.groups++
		for i := range words {
			words[i].Group = p.groups
		}
	}
	return item{words: words}
}

// command builds and records a call from its name word and the arguments at pos.
func (p *parser) command(name Word, stdin shell.Stdin, dot bool) elem {
	kind, cname := classify(name, p.os)
	items := p.args(kind)
	c := Call{Kind: kind, Name: cname, Stdin: stdin, Cwd: p.cwd, home: p.home, os: p.os}
	if t := tables[cname]; kind == Cmdlet && t != nil {
		c.Bound = true
		c.Refused = !t.bind(items)
	}
	c.Args = append(c.Args, name)
	for _, it := range items {
		c.Args = append(c.Args, it.words...)
	}
	if kind == Native && p.os != Windows {
		for i := 1; i < len(c.Args); i++ {
			if w := c.Args[i]; w.glob {
				// PowerShell expands it against the file system for a native command.
				c.Args[i] = Word{Raw: w.Raw, Quote: w.Quote, Group: w.Group}
			}
		}
	}
	if p.quiet == 0 {
		p.res.Calls = append(p.res.Calls, c)
	}
	return p.after(c, dot)
}

// after applies a call's effects: location changes, variable assignments, the script blocks it
// runs and its nested scripts.
func (p *parser) after(c Call, dot bool) elem {
	if c.Name == "" || dot || strings.HasSuffix(strings.ToLower(c.Name), ".ps1") {
		p.moved = true // a script or dynamic command may change location
	}
	if c.Kind == Native {
		switch {
		case c.Is("pwsh"), c.Is("powershell"):
			p.pwsh(c)
		case c.Is("bash"), c.Is("sh"), c.Is("zsh"):
			p.bash(c)
		}
		return elem{}
	}
	switch c.Name {
	case "Set-Location":
		p.moved = true
		if dir, ok := p.cdTarget(c); ok {
			return elem{cd: true, cdDir: dir}
		}
	case "Push-Location", "Pop-Location":
		p.moved = true
	case "Set-Variable":
		p.markAssigned(c, func(t string) Known {
			if strings.EqualFold(t, "HOME") {
				return KnownHome
			}
			return NotKnown
		})
	case "Set-Item":
		p.markAssigned(c, func(t string) Known {
			t = strings.ToLower(t)
			if rest, ok := strings.CutPrefix(t, "env:"); ok {
				return knownName("env:" + strings.TrimLeft(rest, `\/`))
			}
			return NotKnown
		})
	case "ForEach-Object":
		p.walkParams(c, true, "Process", "Begin", "End")
	case "Where-Object":
		p.walkParams(c, true, "FilterScript")
	case "Invoke-Command":
		p.walkParams(c, false, "ScriptBlock")
	case "Invoke-Expression":
		p.moved = true
		p.invokeExpression(c)
	}
	return elem{}
}

func (p *parser) markAssigned(c Call, known func(string) Known) {
	if p.quiet > 0 {
		return
	}
	for _, w := range c.Args[1:] {
		if k := known(w.Text); w.Static && k != NotKnown {
			p.assigned[k] = true
		}
	}
}

// cdTarget returns the directory a `Set-Location X` moves to ("" when unknown) and whether the
// call has exactly one static target.
func (p *parser) cdTarget(c Call) (string, bool) {
	if !c.Bound || c.Refused {
		return "", false
	}
	var target []Word
	for _, w := range c.Args[1:] {
		if !w.IsParam && (w.Param == "Path" || w.Param == "LiteralPath") {
			target = append(target, w)
		}
	}
	if len(target) != 1 || !target[0].Static || target[0].Group != 0 || target[0].Text == "" {
		return "", false
	}
	if p.cdSteps >= maxCdSteps {
		return "", true // bounds the cwd strings a long `cd x &&` chain builds
	}
	t, ok := fileSystemPath(target[0].Text, p.os)
	// The result is at most the target joined to the longer of home and cwd: past the cap the
	// candidate is never built.
	if !ok || max(len(p.home), len(p.cwd))+len(`\`)+len(t) > maxCwdLen {
		return "", true
	}
	dir, ok := paths.ResolvePowerShell(p.home, p.cwd, t, p.os == Windows)
	if !ok {
		return "", true
	}
	p.cdSteps++
	return dir, true
}

// fileSystemPath returns a Set-Location target as a FileSystem path of the host, where `\` and `/`
// both separate. ok is false for a target whose location is not known statically: the history
// forms `-` and `+`, a wildcard, or a provider-qualified or drive-qualified path other than a
// Windows drive letter.
func fileSystemPath(t string, os OS) (string, bool) {
	if t == "-" || t == "+" || strings.ContainsAny(t, "*?[") || strings.Contains(t, "::") {
		return "", false
	}
	if os == Windows {
		rest := t
		if len(t) >= 2 && t[1] == ':' && isLetter(t[0]) {
			rest = t[2:]
		}
		return t, !strings.Contains(rest, ":")
	}
	return strings.ReplaceAll(t, `\`, "/"), !strings.Contains(t, ":")
}

// walkParams walks the script blocks bound to names, when every bound parameter belongs to the
// parameter set that runs them.
func (p *parser) walkParams(c Call, loop bool, names ...string) {
	if !fitsBlockSet(c) {
		return
	}
	for _, w := range c.Args[1:] {
		if !w.Block || !slices.Contains(names, w.Param) {
			continue
		}
		cwd := c.Cwd
		if loop && w.blockMoved {
			cwd = ""
		}
		p.walk(w.blockStart, w.blockEnd, cwd)
	}
}

// blockSets are, per executing cmdlet, the parameters of the parameter set that runs its blocks
// (common parameters included). A local Invoke-Command takes only -ArgumentList and -NoNewScope
// beside its block.
var blockSets = map[string]map[string]bool{
	"ForEach-Object": paramSet(
		"Process", "Begin", "End", "RemainingScripts", "InputObject", "WhatIf", "Confirm",
	),
	"Where-Object":   paramSet("FilterScript", "InputObject"),
	"Invoke-Command": paramSet("ScriptBlock", "ArgumentList", "NoNewScope"),
}

func paramSet(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	for _, c := range commonParams {
		m[c.name] = true
	}
	return m
}

// fitsBlockSet reports whether every word of c binds to the parameter set that runs its blocks.
func fitsBlockSet(c Call) bool {
	set := blockSets[c.Name]
	if !c.Bound || c.Refused || set == nil {
		return false
	}
	for _, w := range c.Args[1:] {
		if !set[w.Param] {
			return false
		}
	}
	return true
}

// invokeExpression parses the one static string of an Invoke-Expression as a nested script.
func (p *parser) invokeExpression(c Call) {
	if !c.Bound || c.Refused {
		return
	}
	var cmd []Word
	for _, w := range c.Args[1:] {
		if w.Param == "Command" && !w.IsParam {
			cmd = append(cmd, w)
		}
	}
	if len(cmd) == 1 && cmd[0].Static {
		p.nestedPowerShell(cmd[0].Text, c.Cwd, true)
	}
}

var (
	pwshSwitches = map[string]bool{
		"-noprofile": true, "-nop": true, "-noninteractive": true, "-noni": true,
		"-nologo": true, "-nol": true, "-noexit": true, "-noe": true, "-mta": true, "-sta": true,
		"-login": true, "-l": true,
	}
	pwshValued = map[string]bool{
		"-executionpolicy": true, "-ex": true, "-ep": true, "-windowstyle": true, "-w": true,
		"-outputformat": true, "-o": true, "-of": true, "-inputformat": true, "-if": true,
		"-workingdirectory": true, "-wd": true,
	}
)

// pwsh parses the script of `pwsh|powershell [options] -Command '<one static string>'`.
func (p *parser) pwsh(c Call) {
	args, cwd := c.Args[1:], c.Cwd
	for i := 0; i < len(args); i++ {
		if !args[i].Static {
			return
		}
		switch t := strings.ToLower(args[i].Text); {
		case t == "-c" || t == "-command":
			if i+2 == len(args) && args[i+1].Static {
				p.nestedPowerShell(args[i+1].Text, cwd, false)
			}
			return
		case pwshSwitches[t]:
		case pwshValued[t]:
			if t == "-workingdirectory" || t == "-wd" {
				cwd = ""
			}
			i++
		default:
			return
		}
	}
}

// nestedPowerShell parses text as a nested PowerShell script and adds its calls. An
// Invoke-Expression (shared) runs in the caller's scope, so its assignments carry over.
func (p *parser) nestedPowerShell(text, cwd string, shared bool) {
	if p.quiet > 0 || p.depth >= maxDepth {
		return
	}
	assigned := maps.Clone(p.assigned)
	q := newParser(text, cwd, p.home, p.os, p.depth+1, assigned)
	s := Script{Dialect: PowerShell, Text: text, Cwd: cwd, Depth: p.depth + 1}
	if err := q.run(); err != nil {
		s.Err = err
		p.res.Nested = append(p.res.Nested, s)
		return
	}
	p.res.Nested = append(append(p.res.Nested, s), q.res.Nested...)
	p.res.Calls = append(p.res.Calls, q.res.Calls...)
	if shared {
		maps.Copy(p.assigned, assigned)
	}
}

// bash records the script of `bash|sh|zsh … -c '<one static string>'` for the Bash path.
func (p *parser) bash(c Call) {
	if p.quiet > 0 || p.depth >= maxDepth {
		return
	}
	cflag := false
	script := func(i int) {
		if cflag && i < len(c.Args) && c.Args[i].Static {
			p.res.Nested = append(p.res.Nested, Script{
				Dialect: Bash, Text: c.Args[i].Text, Cwd: c.Cwd, Depth: p.depth + 1,
			})
		}
	}
	for i := 1; i < len(c.Args); i++ {
		a := c.Args[i]
		if !a.Static {
			return
		}
		t := a.Text
		switch {
		case t == "--" || t == "-":
			script(i + 1)
			return
		case t == "--rcfile" || t == "--init-file" || t == "-o" || t == "+o" || t == "-O" ||
			t == "+O":
			i++
		case strings.HasPrefix(t, "--"):
		case len(t) > 1 && (t[0] == '-' || t[0] == '+'):
			cflag = cflag || (t[0] == '-' && strings.Contains(t[1:], "c"))
		default:
			script(i)
			return
		}
	}
}
