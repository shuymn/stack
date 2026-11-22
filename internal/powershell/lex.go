package powershell

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The quote characters PowerShell accepts besides the ASCII ones.
var (
	singleQuotes = []string{"'", "‘", "’"}
	doubleQuotes = []string{`"`, "“", "”"}
)

var (
	numberRe = regexp.MustCompile(
		`(?i)^(0x[0-9a-f]+|\d+(\.\d*)?(e[+-]?\d+)?)(ul|us|uy|u|l|d|n|y|s)?(kb|mb|gb|tb|pb)?$`,
	)
	numberOpRe = regexp.MustCompile(`(?i)^(0x[0-9a-f]+|\d+(\.\d+)?)(\.\.|[-+*/%])`)
	percentRe  = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_()]*%`)
)

func isBlank(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v'
}

func isLetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isIdent(c byte) bool { return isLetter(c) || isDigit(c) || c == '_' }

// wordBuilder assembles a word from its parts.
type wordBuilder struct {
	text     strings.Builder
	segs     int
	quote    Quote
	quoteSet bool
	known    Known
	dynamic  bool
	// glob reports an unquoted, unescaped wildcard character.
	glob bool
}

// part records the quoting of the part that starts now; the first one is the word's.
func (b *wordBuilder) part(q Quote) {
	if !b.quoteSet {
		b.quote, b.quoteSet = q, true
	}
}

func (b *wordBuilder) lit(s string) {
	b.text.WriteString(s)
	b.segs++
}

func (b *wordBuilder) dyn() {
	b.dynamic = true
	b.segs++
}

// variable adds a variable: known when it is the word's first part and not assigned.
func (b *wordBuilder) variable(k Known, assigned map[Known]bool) {
	if b.segs == 0 && k != NotKnown && !assigned[k] {
		b.known = k
	} else {
		b.dynamic = true
	}
	b.segs++
}

func (b *wordBuilder) word(raw string) Word {
	w := Word{Raw: raw, Quote: b.quote}
	switch {
	case b.dynamic:
	case b.known != NotKnown:
		w.Known, w.Text = b.known, b.text.String()
	default:
		w.Static, w.Text = true, b.text.String()
	}
	return w
}

// knownName maps a variable name (without `$` and braces) to a known variable.
func knownName(name string) Known {
	switch strings.ToLower(name) {
	case "home":
		return KnownHome
	case "env:userprofile":
		return KnownUserProfile
	case "env:systemdrive":
		return KnownSystemDrive
	default:
		return NotKnown
	}
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() byte { return p.at(p.pos) }

func (p *parser) at(i int) byte {
	if i >= len(p.src) {
		return 0
	}
	return p.src[i]
}

func (p *parser) has(s string) bool { return strings.HasPrefix(p.src[p.pos:], s) }

// quoteAt returns the byte length of a quote of set at pos, or 0.
func (p *parser) quoteAt(set []string) int {
	for _, q := range set {
		if p.has(q) {
			return len(q)
		}
	}
	return 0
}

// continuation returns the length of a backtick line continuation at pos, or 0.
func (p *parser) continuation() int {
	switch {
	case p.has("`\n"):
		return len("`\n")
	case p.has("`\r\n"):
		return len("`\r\n")
	default:
		return 0
	}
}

// skipBlank skips horizontal blanks, line continuations and comments, stopping at a newline.
func (p *parser) skipBlank() {
	for !p.eof() {
		c := p.peek()
		switch {
		case isBlank(c):
			p.pos++
		case p.continuation() > 0:
			p.pos += p.continuation()
		case p.has("<#"):
			i := strings.Index(p.src[p.pos+len("<#"):], "#>")
			if i < 0 {
				p.fail("unclosed block comment")
			}
			p.pos += len("<#") + i + len("#>")
		case c == '#':
			for !p.eof() && p.peek() != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// skipLines is skipBlank across newlines.
func (p *parser) skipLines() {
	for {
		p.skipBlank()
		if p.peek() != '\n' {
			return
		}
		p.pos++
	}
}

// atEnd reports whether pos ends a pipeline element.
func (p *parser) atEnd() bool {
	if p.eof() {
		return true
	}
	return strings.IndexByte("\n;|&)}", p.peek()) >= 0
}

// boundary reports whether i is the end of a token: EOF, a blank or a newline.
func (p *parser) boundary(i int) bool {
	return i >= len(p.src) || isBlank(p.src[i]) || p.src[i] == '\n'
}

// char appends the rune at pos to b.
func (p *parser) char(b *wordBuilder) {
	_, n := utf8.DecodeRuneInString(p.src[p.pos:])
	b.lit(p.src[p.pos : p.pos+n])
	p.pos += n
}

// single lexes a single-quoted string whose opener is n bytes long.
func (p *parser) single(b *wordBuilder, n int) {
	p.pos += n
	b.part(Single)
	var s strings.Builder
	for {
		if p.eof() {
			p.fail("unclosed string")
		}
		if m := p.quoteAt(singleQuotes); m > 0 {
			p.pos += m
			if m2 := p.quoteAt(singleQuotes); m2 > 0 {
				s.WriteString(p.src[p.pos : p.pos+m2])
				p.pos += m2
				continue
			}
			b.lit(s.String())
			return
		}
		_, sz := utf8.DecodeRuneInString(p.src[p.pos:])
		s.WriteString(p.src[p.pos : p.pos+sz])
		p.pos += sz
	}
}

// double lexes a double-quoted string whose opener is n bytes long.
func (p *parser) double(b *wordBuilder, n int) {
	p.pos += n
	b.part(Double)
	for {
		if p.eof() {
			p.fail("unclosed string")
		}
		if m := p.quoteAt(doubleQuotes); m > 0 {
			p.pos += m
			if m2 := p.quoteAt(doubleQuotes); m2 > 0 {
				b.lit(p.src[p.pos : p.pos+m2])
				p.pos += m2
				continue
			}
			return
		}
		p.expandableChar(b)
	}
}

// expandableChar lexes one character, escape or variable of an expandable string.
func (p *parser) expandableChar(b *wordBuilder) {
	switch p.peek() {
	case '`':
		p.escape(b)
	case '$':
		p.dollar(b, true)
	default:
		p.char(b)
	}
}

var escapes = map[byte]string{
	'0': "\x00", 'a': "\a", 'b': "\b", 'e': "\x1b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t",
	'v': "\v",
}

// escape lexes a backtick escape of an expandable string.
func (p *parser) escape(b *wordBuilder) {
	p.pos++
	if p.eof() {
		p.fail("unclosed string")
	}
	c := p.peek()
	if s, ok := escapes[c]; ok {
		b.lit(s)
		p.pos++
		return
	}
	if c == 'u' && p.at(p.pos+1) == '{' {
		end := strings.IndexByte(p.src[p.pos:], '}')
		if end < 0 {
			p.fail("bad unicode escape")
		}
		r, err := strconv.ParseUint(p.src[p.pos+len("u{"):p.pos+end], 16, 32)
		if err != nil || r > unicode.MaxRune {
			p.fail("bad unicode escape")
		}
		b.lit(string(rune(r)))
		p.pos += end + 1
		return
	}
	p.char(b)
}

// hereString lexes a here-string at `@'` or `@"`: the opener ends its line (after optional blanks)
// and the closer stands at column 0.
func (p *parser) hereString(b *wordBuilder) {
	expandable := p.at(p.pos+1) == '"'
	j := p.pos + len("@'")
	for j < len(p.src) && isBlank(p.src[j]) {
		j++
	}
	if j >= len(p.src) || p.src[j] != '\n' {
		p.fail("here-string header must end its line")
	}
	bodyStart := j + 1
	closer := "'@"
	if expandable {
		closer = `"@`
	}
	var at int
	for k := bodyStart; ; {
		i := strings.Index(p.src[k:], closer)
		if i < 0 {
			p.fail("unclosed here-string")
		}
		if i += k; i == bodyStart || p.src[i-1] == '\n' {
			at = i
			break
		}
		k = i + 1
	}
	bodyEnd := bodyStart
	if at > bodyStart {
		bodyEnd = at - 1
		if bodyEnd > bodyStart && p.src[bodyEnd-1] == '\r' {
			bodyEnd--
		}
	}
	b.lit("") // a here-string word is never a known-variable word
	if !expandable {
		b.part(HereSingle)
		b.lit(p.src[bodyStart:bodyEnd])
		p.pos = at + len(closer)
		return
	}
	b.part(HereDouble)
	p.pos = bodyStart
	for p.pos < bodyEnd {
		p.expandableChar(b)
	}
	if p.pos > bodyEnd {
		p.fail("subexpression crosses the here-string end")
	}
	p.pos = at + len(closer)
}

// dollar lexes a `$` part: a subexpression, a variable or a lone `$`.
func (p *parser) dollar(b *wordBuilder, inString bool) {
	n := p.at(p.pos + 1)
	switch {
	case n == '(':
		p.pos++
		p.paren()
		b.dyn()
	case n == '{':
		j := p.pos + len("${")
		var name strings.Builder
		for ; j < len(p.src) && p.src[j] != '}'; j++ {
			if p.src[j] == '`' && j+1 < len(p.src) {
				j++
			}
			name.WriteByte(p.src[j])
		}
		if j >= len(p.src) {
			p.fail("unclosed variable")
		}
		p.pos = j + 1
		if !inString && p.members() {
			b.dyn()
			return
		}
		b.variable(knownName(name.String()), p.assigned)
	case n == '$' || n == '?' || n == '^':
		p.pos += 2
		b.dyn()
	case isIdent(n):
		j := p.pos + 1
		for j < len(p.src) && isIdent(p.src[j]) {
			j++
			if j+1 < len(p.src) && p.src[j] == ':' && p.src[j+1] != ':' && isIdent(p.src[j+1]) {
				j++
			}
		}
		name := p.src[p.pos+1 : j]
		p.pos = j
		if !inString && p.members() {
			b.dyn()
			return
		}
		b.variable(knownName(name), p.assigned)
	default:
		p.pos++
		b.dyn()
	}
}

// members consumes the member accesses (`.Length`, `::Name`), method calls and indexes after a
// variable or expression outside a string, and reports whether there were any.
func (p *parser) members() bool {
	found := false
	for {
		switch {
		case p.peek() == '[':
			p.bracket()
		case p.peek() == '.' && isIdent(p.at(p.pos+1)), p.has("::") && isIdent(p.at(p.pos+2)):
			for p.peek() == '.' || p.peek() == ':' {
				p.pos++
			}
			for isIdent(p.peek()) {
				p.pos++
			}
			if p.peek() == '(' {
				p.methodArgs()
			}
		default:
			return found
		}
		found = true
	}
}

// methodArgs skips the `(…)` arguments of a method call. They are expressions, not command
// positions: only an explicit `$(…)` in them is walked.
func (p *parser) methodArgs() {
	p.enter()
	defer p.leave()
	p.pos++
	var b wordBuilder
	for {
		p.skipLines()
		switch c := p.peek(); {
		case p.eof():
			p.fail("unclosed method arguments")
		case c == ')':
			p.pos++
			return
		case c == '(':
			p.methodArgs()
		case p.has("@("):
			p.pos++
			p.methodArgs()
		case p.has("@{"):
			p.quietly(p.hash)
		case p.has("@'"), p.has(`@"`):
			p.hereString(&b)
		case c == '$':
			p.dollar(&b, false)
		case c == '{':
			p.block()
		case c == '[':
			p.bracket()
		case p.quoteAt(singleQuotes) > 0:
			p.single(&b, p.quoteAt(singleQuotes))
		case p.quoteAt(doubleQuotes) > 0:
			p.double(&b, p.quoteAt(doubleQuotes))
		default:
			p.pos++
		}
	}
}

// paren walks the statements of `(…)` at pos.
func (p *parser) paren() {
	p.pos++
	p.statements(')')
	p.pos++
}

// argWord lexes one argument-mode word at pos. In a cmdlet's arguments an unquoted `,` ends it.
func (p *parser) argWord(kind Kind) Word {
	start := p.pos
	var b wordBuilder
	switch {
	case p.peek() == '{':
		s, e, moved := p.block()
		return Word{
			Raw: p.src[start:p.pos], Block: true, blockStart: s, blockEnd: e, blockMoved: moved,
		}
	case p.has("@{"):
		p.hash()
		return Word{Raw: p.src[start:p.pos]}
	case p.has("@("), p.has("$("), p.peek() == '(':
		// A subexpression at the token start is a whole argument: `(7)--help` is two.
		if p.peek() != '(' {
			p.pos++
		}
		p.paren()
		return Word{Raw: p.src[start:p.pos]}
	case p.has("@'"), p.has(`@"`):
		p.hereString(&b)
	case p.peek() == '@':
		p.pos++
		b.part(Bare)
		b.dyn() // splatting
	}
	for !p.eof() {
		c := p.peek()
		if isBlank(c) || strings.IndexByte("\n;|&)}{>", c) >= 0 || (kind == Cmdlet && c == ',') {
			break
		}
		if n := p.quoteAt(singleQuotes); n > 0 {
			p.single(&b, n)
			continue
		}
		if n := p.quoteAt(doubleQuotes); n > 0 {
			p.double(&b, n)
			continue
		}
		b.part(Bare)
		switch c {
		case '`':
			if p.continuation() > 0 {
				return b.word(p.src[start:p.pos])
			}
			p.pos++
			if p.eof() {
				p.fail("trailing backtick")
			}
			p.char(&b)
		case '$':
			p.dollar(&b, false)
		case '(':
			p.paren()
			b.dyn()
		default:
			b.glob = b.glob || strings.IndexByte("*?[", c) >= 0
			p.char(&b)
		}
	}
	if p.pos == start {
		p.fail("unexpected character")
	}
	w := b.word(p.src[start:p.pos])
	w.glob = b.glob
	return w
}

// verbatim lexes a `--%` tail: up to a CR or LF, or an unquoted `|`, `&&` or `||`, split at blanks
// outside `"`. A word holding `"` is dynamic (its argv depends on the native-argument mode), and so
// is one holding `%NAME%`.
func (p *parser) verbatim() []Word {
	var words []Word
	var text strings.Builder
	start, have, quoted, hadQuote := p.pos, false, false, false
	flush := func() {
		if have {
			w := Word{Raw: p.src[start:p.pos], Verbatim: true}
			if t := text.String(); !hadQuote && !percentRe.MatchString(t) {
				w.Text, w.Static = t, true
			}
			words = append(words, w)
		}
		text.Reset()
		have, hadQuote = false, false
	}
	for !p.eof() {
		c := p.peek()
		if c == '\n' || c == '\r' || (!quoted && (c == '|' || p.has("&&"))) {
			break
		}
		switch {
		case c == '"':
			quoted, hadQuote = !quoted, true
		case !quoted && isBlank(c):
			flush()
			p.pos++
			start = p.pos
			continue
		default:
			text.WriteByte(c)
		}
		have = true
		p.pos++
	}
	flush()
	return words
}

// numberAhead reports whether the token at pos is a number (`7`, `0x1F`, `1kb`, `1..3`), not a
// command such as `7z`.
func (p *parser) numberAhead() bool {
	j := p.pos
	for j < len(p.src) && !isBlank(p.src[j]) && strings.IndexByte("\n;|&(){}'\"$@,=>", p.src[j]) < 0 {
		j++
	}
	tok := p.src[p.pos:j]
	return numberRe.MatchString(tok) || numberOpRe.MatchString(tok)
}

// keywordAhead returns the lower-cased keyword-shaped token at pos when a keyword boundary follows
// it, else "".
func (p *parser) keywordAhead() string {
	j := p.pos
	for j < len(p.src) && isLetter(p.src[j]) {
		j++
	}
	if j == p.pos || (j < len(p.src) && !isBlank(p.src[j]) &&
		strings.IndexByte("\n(){};", p.src[j]) < 0) {
		return ""
	}
	return strings.ToLower(p.src[p.pos:j])
}
