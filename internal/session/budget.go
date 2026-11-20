package session

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	// Budget is the most characters compact restore emits, tags, path headers, markers and the
	// final newline included (`orchestration-context.py:43`). Characters are UTF-16 code units, the
	// string length Claude Code compares with its 10,000-character hook output limit.
	Budget = 9500
	// blockLimit caps the body of each kit state and items block, as legacy did.
	blockLimit = 4500
	// reserveChars is the body length of each state file reserved per kit before any kit gets more.
	reserveChars = 600
	// sep separates emitted pieces.
	sep = "\n\n"
)

// piece is one emitted unit of compact context: a body between tags, or a bare marker line when
// head and tail are empty. out is what the budget lets it emit, "" for nothing.
type piece struct {
	head, tail string
	body       string
	kind       string // names the piece in its omitted marker
	path       string // names the piece's file in its omitted marker, when set
	out        string
}

// chars is the length of s in UTF-16 code units; an invalid byte counts as the U+FFFD it encodes to.
func chars(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// cost is what emitting s takes from the budget: its characters and one separator. Joining n
// pieces adds n-1 separators and a final newline, so a budget of Budget+1 for the costs of all
// emitted pieces keeps the joined text within Budget.
func cost(s string) int {
	if s == "" {
		return 0
	}
	return chars(s) + len(sep)
}

// cutMarker ends a body cut by n characters.
func cutMarker(n int) string {
	return fmt.Sprintf("\n[...cut %d chars]", n)
}

// minimal is the least p emits once it is due: a bare marker whole, a tagged piece its omitted
// marker, which names its file when it has one.
func (p *piece) minimal() string {
	switch {
	case p.head == "":
		return p.body
	case p.path != "":
		return "[" + p.kind + " omitted: context limit: " + p.path + "]"
	default:
		return "[" + p.kind + " omitted: context limit]"
	}
}

// fit returns the longest rendering of p whose cost is at most room: whole when its body is within
// limit (0 = no limit) and it fits; else its body cut at the tail to at most limit characters, at a
// code-point boundary, and ended with a cut marker; else its minimal form; else "". A bare marker
// is never cut.
func (p *piece) fit(room, limit int) string {
	total := chars(p.body)
	keep := total
	if limit > 0 && limit < total {
		keep = limit
	}
	if keep == total {
		if whole := p.head + p.body + p.tail; cost(whole) <= room {
			return whole
		}
	}
	if p.head != "" {
		avail := room - len(sep) - chars(p.head) - chars(p.tail)
		if keep+chars(cutMarker(total-keep)) > avail {
			// The marker for any cut of this body is at most as long as for total.
			keep = avail - len(cutMarker(total))
		}
		if kept := prefix(p.body, keep); kept != "" {
			return p.head + kept + cutMarker(total-chars(kept)) + p.tail
		}
	}
	if m := p.minimal(); cost(m) <= room {
		return m
	}
	return ""
}

// prefix returns the longest prefix of s within n characters that ends at a code-point boundary.
func prefix(s string, n int) string {
	for i, r := range s {
		n -= utf16.RuneLen(r)
		if n < 0 {
			return s[:i]
		}
	}
	return s
}

// allocator hands out the budget in the order reserve and place are called.
type allocator struct{ room int }

func newAllocator() *allocator { return &allocator{room: Budget + 1} }

// reserve sets aside p's minimal form, when it fits, so later text cannot take its place. A nil
// piece is ignored.
func (a *allocator) reserve(p *piece) {
	if p == nil {
		return
	}
	if m := p.minimal(); cost(m) <= a.room {
		p.out = m
		a.room -= cost(m)
	}
}

// place gives p the longest rendering that fits the remaining budget plus what p already holds,
// with its body capped at limit (0 = none). It never shrinks what p already holds. A nil piece is
// ignored.
func (a *allocator) place(p *piece, limit int) {
	if p == nil {
		return
	}
	if out := p.fit(a.room+cost(p.out), limit); cost(out) > cost(p.out) {
		a.room += cost(p.out) - cost(out)
		p.out = out
	}
}

// join renders the emitted pieces in display order, separated by blank lines and ending with a
// newline; nil pieces are skipped and nothing emitted yields "".
func join(pieces []*piece) string {
	var outs []string
	for _, p := range pieces {
		if p != nil && p.out != "" {
			outs = append(outs, p.out)
		}
	}
	if len(outs) == 0 {
		return ""
	}
	return strings.Join(outs, sep) + "\n"
}

// tagged makes a piece of body between `<tag>` tags, with a quoted path attribute when path is
// set, in the format of kit.Block.
func tagged(tag, path, body, kind string) *piece {
	head := "<" + tag + ">\n"
	if path != "" {
		head = "<" + tag + " path=" + strconv.Quote(path) + ">\n"
	}
	return &piece{head: head, tail: "\n</" + tag + ">", body: body, kind: kind, path: path}
}
