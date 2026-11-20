package guard

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/shuymn/stack/internal/config"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

const pencilAdvice = "Pencil's Generate (AI image/SVG) is not used: the quota is small. Draw icons " +
	"as paths (Octicons). If this is a helper of your own named generate*, rename it."

// pencilInput holds the fields of the pencil MCP execute tool that run code: input is a JavaScript
// snippet, edits are find/replace patches Pencil applies to a failed snippet before rerunning it.
type pencilInput struct {
	Input string `json:"input"`
	Edits []struct {
		Find    string `json:"find"`
		Replace string `json:"replace"`
	} `json:"edits"`
}

// pencil denies executed code that calls a function or method whose name starts with `generate`
// (case-insensitive), unless `[safety].deny_pencil_generate = false`. input is parsed; on a parse
// error, and for each edits[].replace (the patched snippet is not in the payload), the best-effort
// token fallback judges it.
func pencil(raw json.RawMessage, cfg config.Config) (Decision, bool) {
	if !cfg.Safety.DenyPencilGenerate {
		return Decision{}, false
	}
	var tin pencilInput
	if json.Unmarshal(raw, &tin) != nil {
		return Decision{}, false
	}
	if name, ok := generateCall(tin.Input); ok {
		return deny(RulePencil, "the code calls %s(…). %s", name, pencilAdvice)
	}
	for _, e := range tin.Edits {
		if name, ok := generateToken(e.Replace); ok {
			return deny(RulePencil, "an edit inserts a call to %s(…). %s", name, pencilAdvice)
		}
	}
	return Decision{}, false
}

func isGenerate(name string) bool {
	return len(name) >= len("generate") && strings.EqualFold(name[:len("generate")], "generate")
}

// generateCall parses code as the body of an async function, so top-level return and await parse,
// and walks it for a call whose callee name starts with generate. On a parse error it falls back to
// generateToken.
func generateCall(code string) (string, bool) {
	if code == "" {
		return "", false
	}
	tree, err := js.Parse(parse.NewInputString("(async function(){\n"+code+"\n})"), js.Options{})
	if err != nil {
		return generateToken(code)
	}
	v := &callFinder{}
	js.Walk(v, tree)
	return v.found, v.found != ""
}

type callFinder struct {
	found string
}

func (v *callFinder) Enter(n js.INode) js.IVisitor {
	if v.found != "" {
		return nil
	}
	if c, ok := n.(*js.CallExpr); ok {
		if name := calleeName(c.X); isGenerate(name) {
			v.found = name
			return nil
		}
	}
	return v
}

func (v *callFinder) Exit(js.INode) {}

// calleeName returns the identifier or member property a call invokes, or "".
func calleeName(x js.IExpr) string {
	switch x := x.(type) {
	case *js.Var:
		return string(x.Data)
	case *js.DotExpr:
		switch y := x.Y.(type) {
		case *js.LiteralExpr:
			return string(y.Data)
		case js.LiteralExpr:
			return string(y.Data)
		case *js.Var:
			return string(y.Data)
		}
	}
	return ""
}

// generateToken is the token fallback: an identifier starting with generate followed by `(`,
// outside string, template and comment tokens. A `/` is rescanned as a regular expression only when
// the previous significant token cannot end an expression; after `)`, `]`, `}` or an identifier it
// stays division, so a regex there is read as code.
func generateToken(code string) (string, bool) {
	l := js.NewLexer(parse.NewInputString(code))
	prev := js.ErrorToken // start of input
	pending := ""
	for range len(code) + 1 {
		tt, data := l.Next()
		switch tt { //nolint:exhaustive // only skipped and slash tokens need handling
		case js.ErrorToken:
			if errors.Is(l.Err(), io.EOF) {
				return "", false
			}
			continue // the lexer skipped the offending rune
		case js.WhitespaceToken, js.LineTerminatorToken, js.CommentToken,
			js.CommentLineTerminatorToken:
			continue
		case js.DivToken, js.DivEqToken:
			if !endsExpression(prev) {
				tt, _ = l.RegExp()
			}
		}
		if pending != "" && tt == js.OpenParenToken {
			return pending, true
		}
		pending = ""
		if tt == js.IdentifierToken && isGenerate(string(data)) {
			pending = string(data)
		}
		prev = tt
	}
	return "", false
}

// endsExpression reports whether a token can end an expression, so a following `/` divides.
func endsExpression(tt js.TokenType) bool {
	switch tt { //nolint:exhaustive // tokens that can end an expression; the rest by category below
	case js.CloseParenToken, js.CloseBracketToken, js.CloseBraceToken, js.StringToken,
		js.TemplateToken, js.TemplateEndToken, js.RegExpToken, js.PrivateIdentifierToken,
		js.ThisToken, js.SuperToken, js.NullToken, js.TrueToken, js.FalseToken, js.IncrToken,
		js.DecrToken:
		return true
	}
	return js.IsNumeric(tt) || js.IsIdentifier(tt)
}
