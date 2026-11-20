// Package shell turns a Bash tool command into the simple commands it statically runs.
//
// Parsing follows the shell's input unit: bash and zsh read and run a whole input line (with the
// heredoc bodies it opens) before reading the next, and a syntax error stops its line from running.
// So the command is parsed whole, as bash and, if that fails, as zsh (the Bash tool runs the user's
// shell); when both fail it is parsed again up to the end of the last line before the error line,
// and only those statements are returned.
//
// For each simple command Parse returns its argv after stripping the `env`, `command`, `nohup`,
// `time` and `mise exec --` wrappers and leading assignments; each word marked static or dynamic; its
// stdin source; and its known cwd. It recurses into `bash|sh|zsh -c '<literal>'`, `eval '<literal>'`,
// a bash/sh/zsh call whose stdin is a literal heredoc or here-string, and `$(…)`/backticks, to depth
// 2. Quoted text and heredoc bodies of other commands never become calls.
//
// Parse also returns the source ranges of handoff substitutions: a `$(…)` whose whole body is exactly
// one `cat F` or `< F` with one static operand and no other redirect, which is (part of) the value
// word of a variable assignment or of a flag, the whole word of a here-string, or an argument of an
// `echo`/`printf` without redirects whose output feeds a pipe.
package shell

import (
	"errors"
	"path"
	"regexp"
	"strings"

	"github.com/shuymn/stack/internal/paths"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// maxDepth bounds the recursion into nested scripts and command substitutions.
const maxDepth = 2

// Word is one argv word.
type Word struct {
	// Text is the word's value when Static; empty otherwise.
	Text string
	// Static reports that the word consists only of literal, single-quoted and double-quoted text
	// without expansions, so Text is exactly what the command receives.
	Static bool
	// Tilde reports that the word is static and starts with an unquoted `~`, which bash expands.
	Tilde bool
}

// StdinKind classifies a call's stdin.
type StdinKind int

// Stdin kinds.
const (
	// StdinNone is the inherited stdin: nothing static is known.
	StdinNone StdinKind = iota
	// StdinText is a bound heredoc or here-string with static text.
	StdinText
	// StdinFile is a literal `< F`.
	StdinFile
	// StdinDynamic is a pipe or a redirect whose content or file is not static.
	StdinDynamic
)

// Stdin is a call's stdin source.
type Stdin struct {
	Kind StdinKind
	Text string // StdinText
	File Word   // StdinFile
}

// Call is one simple command.
type Call struct {
	Args  []Word
	Stdin Stdin
	// Cwd is the directory the call runs in, or "" when it is unknown.
	Cwd string
}

// Name returns the base name of argv0 when it is static, else "".
func (c Call) Name() string {
	if len(c.Args) == 0 || !c.Args[0].Static {
		return ""
	}
	return baseName(c.Args[0].Text)
}

// Range is a half-open byte range of the parsed command.
type Range struct {
	Start, End int
}

// Result is what Parse returns.
type Result struct {
	Calls    []Call
	Handoffs []Range
}

// Parse parses command, run in cwd ("" when unknown) by a user whose home is home ("" when
// unknown).
func Parse(command, cwd, home string) Result {
	w := &walker{home: home}
	w.script(scope{src: command, base: 0}, cwd, 0)
	return w.res
}

type walker struct {
	home string
	res  Result
}

// scope is one parsed text: the top-level command or a nested script. base is its offset in the
// top-level command, or -1 when its offsets do not map there (the literal had escapes).
type scope struct {
	src  string
	base int
}

// script parses sc by input lines and walks the statements.
func (w *walker) script(sc scope, cwd string, depth int) {
	w.stmts(sc, parseLines(sc.src), cwd, depth, noInput)
}

// parseLines parses src as bash and, when that fails, as zsh: the Bash tool runs the user's shell,
// which may be zsh, so a line only zsh accepts (`${(f)x}`) still runs. When both fail it parses again
// up to the end of the line before the later error line, until a prefix parses or nothing is left.
func parseLines(src string) []*syntax.Stmt {
	for src != "" {
		line := 0
		for _, lang := range []syntax.LangVariant{syntax.LangBash, syntax.LangZsh} {
			f, err := syntax.NewParser(syntax.Variant(lang)).Parse(strings.NewReader(src), "")
			if err == nil {
				return f.Stmts
			}
			line = max(line, errLine(err))
		}
		if line <= 1 {
			return nil
		}
		src = src[:lineEnd(src, line-1)]
	}
	return nil
}

// errLine returns the 1-based line of a parse error, or 0 when it has none.
func errLine(err error) int {
	if pe, ok := errors.AsType[syntax.ParseError](err); ok {
		return int(pe.Pos.Line())
	}
	if le, ok := errors.AsType[syntax.LangError](err); ok {
		return int(le.Pos.Line())
	}
	return 0
}

// lineEnd returns the offset just after the newline that ends line n (1-based), or len(src).
func lineEnd(src string, n int) int {
	off := 0
	for range n {
		i := strings.IndexByte(src[off:], '\n')
		if i < 0 {
			return len(src)
		}
		off += i + 1
	}
	return off
}

// input is the stdin a statement inherits from its enclosing command, with the offset of a heredoc
// body in the top-level command (-1 when it does not map there).
type input struct {
	stdin Stdin
	base  int
}

var (
	noInput    = input{base: -1}
	pipedInput = input{stdin: Stdin{Kind: StdinDynamic}, base: -1}
)

// stmts walks a statement list with the inherited stdin in. A statement that changes the cwd makes it
// unknown for the next one: only `cd X && …` carries a known cwd forward.
func (w *walker) stmts(sc scope, list []*syntax.Stmt, cwd string, depth int, in input) string {
	for _, s := range list {
		if after := w.stmt(sc, s, cwd, depth, in); after != cwd {
			cwd = ""
		}
	}
	return cwd
}

// stmt walks one statement and returns the cwd after it succeeds ("" when unknown). in is the stdin
// it inherits; its own redirects override it, and the result reaches the commands inside it.
func (w *walker) stmt(sc scope, s *syntax.Stmt, cwd string, depth int, in input) string {
	in = w.redirs(sc, s.Redirs, cwd, depth, in)
	switch c := s.Cmd.(type) {
	case nil:
		return cwd
	case *syntax.CallExpr:
		return w.call(sc, c, in, cwd, depth)
	case *syntax.BinaryCmd:
		switch c.Op { //nolint:exhaustive // Pipe and PipeAll share the default
		case syntax.AndStmt:
			return w.stmt(sc, c.Y, w.stmt(sc, c.X, cwd, depth, in), depth, in)
		case syntax.OrStmt:
			x := w.stmt(sc, c.X, cwd, depth, in)
			ycwd := cwd
			if x != cwd {
				ycwd = "" // `cd X || …`: unknown by design
			}
			y := w.stmt(sc, c.Y, ycwd, depth, in)
			if x != cwd || y != cwd {
				return ""
			}
			return cwd
		default: // pipes: each side runs in a subshell
			if args, ok := pipeFeeder(c.X); ok {
				for _, a := range args {
					w.handoffs(sc, a)
				}
			}
			w.stmt(sc, c.X, cwd, depth, in)
			w.stmt(sc, c.Y, cwd, depth, pipedInput)
			return cwd
		}
	case *syntax.Block:
		return w.stmts(sc, c.Stmts, cwd, depth, in)
	case *syntax.Subshell:
		w.stmts(sc, c.Stmts, cwd, depth, in)
		return cwd
	case *syntax.IfClause:
		return w.ifClause(sc, c, cwd, depth, in)
	case *syntax.WhileClause:
		body := loopCwd(c, cwd)
		w.stmts(sc, c.Cond, body, depth, in)
		w.stmts(sc, c.Do, body, depth, in)
		return unchanged(body, cwd)
	case *syntax.ForClause:
		w.substs(sc, c.Loop, cwd, depth)
		body := loopCwd(c, cwd)
		w.stmts(sc, c.Do, body, depth, in)
		return unchanged(body, cwd)
	case *syntax.CaseClause:
		w.substs(sc, c.Word, cwd, depth)
		after := cwd
		for _, item := range c.Items {
			for _, p := range item.Patterns {
				w.substs(sc, p, cwd, depth)
			}
			if w.stmts(sc, item.Stmts, cwd, depth, in) != cwd {
				after = ""
			}
		}
		return after
	case *syntax.FuncDecl:
		w.stmt(sc, c.Body, "", depth, noInput) // runs later, wherever it is called
		return cwd
	case *syntax.DeclClause:
		for _, a := range c.Args {
			w.assign(sc, a, cwd, depth)
		}
		return cwd
	case *syntax.TimeClause:
		if c.Stmt == nil {
			return cwd
		}
		return w.stmt(sc, c.Stmt, cwd, depth, in)
	case *syntax.CoprocClause:
		w.stmt(sc, c.Stmt, cwd, depth, noInput)
		return cwd
	default: // let, [[ ]], (( )), …
		w.substs(sc, c, cwd, depth)
		return cwd
	}
}

func (w *walker) ifClause(sc scope, c *syntax.IfClause, cwd string, depth int, in input) string {
	after := cwd
	for ; c != nil; c = c.Else {
		cond := w.stmts(sc, c.Cond, cwd, depth, in)
		then := w.stmts(
			sc,
			c.Then,
			cond,
			depth,
			in,
		) // walked even when the condition changed the cwd
		if cond != cwd || then != cwd {
			after = ""
		}
	}
	return after
}

func unchanged(body, cwd string) string {
	if body != cwd {
		return ""
	}
	return cwd
}

// loopCwd is the cwd a loop body starts with: unknown when the loop changes directory, since a later
// iteration starts where the previous one ended.
func loopCwd(n syntax.Node, cwd string) string {
	changes := false
	syntax.Walk(n, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			switch c.Args[0].Lit() {
			case "cd", "pushd", "popd":
				changes = true
			}
		}
		return !changes
	})
	if changes {
		return ""
	}
	return cwd
}

// hdocBase returns the offset of a heredoc body in the top-level command when the body is one
// literal whose source text is exactly in.Text, else -1.
func hdocBase(sc scope, r *syntax.Redirect, in Stdin) int {
	if in.Kind != StdinText || r.Hdoc == nil || sc.base < 0 || len(r.Hdoc.Parts) != 1 {
		return -1
	}
	lit, ok := r.Hdoc.Parts[0].(*syntax.Lit)
	if !ok {
		return -1
	}
	off := int(lit.ValuePos.Offset())
	if off+len(in.Text) > len(sc.src) || sc.src[off:off+len(in.Text)] != in.Text {
		return -1
	}
	return sc.base + off
}

// redirStdin reports the stdin a redirect binds, if it binds fd 0.
func redirStdin(r *syntax.Redirect) (Stdin, bool) {
	if r.N != nil && r.N.Value != "0" {
		return Stdin{}, false
	}
	switch r.Op { //nolint:exhaustive // only redirects that bind stdin matter
	case syntax.RdrIn:
		if wd := word(r.Word); wd.Static {
			return Stdin{Kind: StdinFile, File: wd}, true
		}
		return Stdin{Kind: StdinDynamic}, true
	case syntax.Hdoc, syntax.DashHdoc:
		if text, ok := hdocText(r.Hdoc); ok {
			return Stdin{Kind: StdinText, Text: text}, true
		}
		return Stdin{Kind: StdinDynamic}, true
	case syntax.WordHdoc:
		if wd := word(r.Word); wd.Static {
			return Stdin{Kind: StdinText, Text: wd.Text + "\n"}, true
		}
		return Stdin{Kind: StdinDynamic}, true
	default:
		return Stdin{}, false
	}
}

// hdocText returns a heredoc body that has no expansions, as written.
func hdocText(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", true
	}
	var b strings.Builder
	for _, p := range w.Parts {
		lit, ok := p.(*syntax.Lit)
		if !ok {
			return "", false
		}
		b.WriteString(lit.Value)
	}
	return b.String(), true
}

// substs walks the command substitutions inside n, which run in cwd.
func (w *walker) substs(sc scope, n syntax.Node, cwd string, depth int) {
	if n == nil {
		return
	}
	if wd, ok := n.(*syntax.Word); ok && wd == nil {
		return
	}
	syntax.Walk(n, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst:
			if depth < maxDepth {
				w.stmts(sc, x.Stmts, cwd, depth+1, noInput)
			}
			return false
		case *syntax.ProcSubst:
			if depth < maxDepth {
				w.stmts(sc, x.Stmts, cwd, depth+1, noInput)
			}
			return false
		}
		return true
	})
}

// assign walks an assignment; its value word may hold a handoff.
func (w *walker) assign(sc scope, a *syntax.Assign, cwd string, depth int) {
	if a.Value != nil {
		w.handoffs(sc, a.Value)
	}
	w.substs(sc, a, cwd, depth)
}

// handoffs records the handoff substitutions among wd's parts.
func (w *walker) handoffs(sc scope, wd *syntax.Word) {
	if sc.base < 0 {
		return
	}
	var visit func(parts []syntax.WordPart)
	visit = func(parts []syntax.WordPart) {
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.CmdSubst:
				if isHandoff(p) {
					w.res.Handoffs = append(w.res.Handoffs, Range{
						Start: sc.base + int(
							p.Pos().Offset(),
						),
						End: sc.base + int(
							p.End().Offset(),
						),
					})
				}
			case *syntax.DblQuoted:
				visit(p.Parts)
			}
		}
	}
	visit(wd.Parts)
}

// redirs walks a statement's redirects and returns its stdin: the inherited in, unless a redirect
// binds fd 0, with the offset of a heredoc body that maps to the top-level command (else -1).
func (w *walker) redirs(
	sc scope,
	list []*syntax.Redirect,
	cwd string,
	depth int,
	in input,
) input {
	for _, r := range list {
		w.substs(sc, r.Word, cwd, depth)
		w.substs(sc, r.Hdoc, cwd, depth)
		if stdin, ok := redirStdin(r); ok {
			in = input{stdin: stdin, base: hdocBase(sc, r, stdin)}
		}
		if r.Op == syntax.WordHdoc && soleSubst(r.Word) {
			w.handoffs(sc, r.Word) // `cmd <<< "$(cat F)"`: the value goes to cmd's stdin only
		}
	}
	return in
}

// soleSubst reports whether wd is exactly one command substitution, bare or double-quoted.
func soleSubst(wd *syntax.Word) bool {
	if wd == nil || len(wd.Parts) != 1 {
		return false
	}
	switch p := wd.Parts[0].(type) {
	case *syntax.CmdSubst:
		return true
	case *syntax.DblQuoted:
		if len(p.Parts) != 1 { // `""` has no parts
			return false
		}
		_, ok := p.Parts[0].(*syntax.CmdSubst)
		return ok
	default:
		return false
	}
}

// pipeFeeder returns the arguments of s when s is an `echo` or `printf` call without redirects, so
// its only output goes into the pipe it feeds (`printf '%s\n' "$(cat F)" | sudo -S …`).
func pipeFeeder(s *syntax.Stmt) ([]*syntax.Word, bool) {
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(s.Redirs) > 0 || len(c.Assigns) > 0 || len(c.Args) < 2 {
		return nil, false
	}
	switch c.Args[0].Lit() {
	case "echo", "printf":
		return c.Args[1:], true
	default:
		return nil, false
	}
}

// isHandoff reports whether c is `$(cat F)` or `$(< F)` with one static operand and nothing else.
func isHandoff(c *syntax.CmdSubst) bool {
	if c.Backquotes || len(c.Stmts) != 1 {
		return false
	}
	s := c.Stmts[0]
	if s.Negated || s.Background || s.Coprocess || s.Disown {
		return false
	}
	switch cmd := s.Cmd.(type) {
	case nil:
		return len(s.Redirs) == 1 && s.Redirs[0].Op == syntax.RdrIn && s.Redirs[0].N == nil &&
			word(s.Redirs[0].Word).Static
	case *syntax.CallExpr:
		return len(s.Redirs) == 0 && len(cmd.Assigns) == 0 && len(cmd.Args) == 2 &&
			cmd.Args[0].Lit() == "cat" && word(cmd.Args[1]).Static
	default:
		return false
	}
}

var assignWordRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)

// call walks a simple command reading in and returns the cwd after it succeeds.
func (w *walker) call(sc scope, c *syntax.CallExpr, in input, cwd string, depth int) string {
	for _, a := range c.Assigns {
		w.assign(sc, a, cwd, depth)
	}
	words := make([]Word, len(c.Args))
	assignLike := make([]bool, len(c.Args))
	for i, a := range c.Args {
		w.substs(sc, a, cwd, depth)
		words[i] = word(a)
		lit, ok := a.Parts[0].(*syntax.Lit)
		assignLike[i] = ok && assignWordRe.MatchString(lit.Value)
	}
	start, envAssigns, chdir := stripWrappers(words, assignLike)
	for _, i := range envAssigns {
		w.handoffs(sc, c.Args[i])
	}
	argv := words[start:]
	if len(argv) == 0 {
		return cwd
	}
	if chdir {
		cwd = ""
	}
	call := Call{Args: argv, Stdin: in.stdin, Cwd: cwd}
	name := call.Name()
	if name != "echo" && name != "printf" && name != "cat" {
		w.flagHandoffs(sc, c.Args[start:], argv)
	}
	w.res.Calls = append(w.res.Calls, call)

	if depth < maxDepth {
		w.nested(sc, c.Args[start:], call, in.base, depth)
	}
	switch name {
	case "cd":
		return w.cd(argv, cwd)
	case "pushd", "popd":
		return ""
	default:
		return cwd
	}
}

// flagHandoffs records handoffs in flag values: `--flag=$(cat F)`, and the word right after a word
// starting with `-` other than `--`, before any `--`.
func (w *walker) flagHandoffs(sc scope, raw []*syntax.Word, argv []Word) {
	for i := 1; i < len(argv); i++ {
		if argv[i].Static && argv[i].Text == "--" {
			return
		}
		prev := argv[i-1]
		if prev.Static && strings.HasPrefix(prev.Text, "-") && prev.Text != "--" {
			w.handoffs(sc, raw[i])
			continue
		}
		if lit, ok := raw[i].Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "-") &&
			lit.Value != "--" && strings.Contains(lit.Value, "=") {
			w.handoffs(sc, raw[i])
		}
	}
}

// nested walks the script a call runs from a literal: `bash -c '<s>'`, `eval '<s>'`, or a shell
// reading a literal heredoc or here-string on stdin. stdinBase is the offset of that heredoc body in
// the top-level command, or -1.
func (w *walker) nested(sc scope, raw []*syntax.Word, call Call, stdinBase, depth int) {
	switch call.Name() {
	case "eval":
		texts := make([]string, 0, len(call.Args)-1)
		for _, a := range call.Args[1:] {
			if !a.Static {
				return
			}
			texts = append(texts, a.Text)
		}
		src := strings.Join(texts, " ")
		base := -1
		if script := raw[1:]; len(script) == 1 { // one word: its offsets map to the command
			base = literalBase(sc, script[0], src)
		}
		w.script(scope{src: src, base: base}, call.Cwd, depth+1)
	case "bash", "sh", "zsh":
		i, script, ok := shellScriptArg(call.Args)
		switch {
		case !ok:
		case script:
			if i < len(call.Args) && call.Args[i].Static {
				src := call.Args[i].Text
				w.script(scope{src: src, base: literalBase(sc, raw[i], src)}, call.Cwd, depth+1)
			}
		case i >= len(call.Args) && call.Stdin.Kind == StdinText:
			// stdinBase is -1 for a body with expansions; handoffs in it are then dropped and the
			// secrets rule scans it whole.
			w.script(scope{src: call.Stdin.Text, base: stdinBase}, call.Cwd, depth+1)
		}
	}
}

// shellScriptArg finds the script argument of a bash/sh/zsh call. With -c it returns the index of the
// command string and true; otherwise the index of the first operand (len(args) when the script comes
// from stdin) and false. ok is false when an option word is dynamic.
func shellScriptArg(args []Word) (int, bool, bool) {
	cflag := false
	i := 1
	for ; i < len(args); i++ {
		a := args[i]
		if !a.Static {
			return 0, false, false
		}
		t := a.Text
		switch {
		case t == "--" || t == "-":
			return i + 1, cflag, true
		case t == "--rcfile" || t == "--init-file" || t == "-o" || t == "+o" || t == "-O" ||
			t == "+O":
			i++
		case strings.HasPrefix(t, "--"):
		case len(t) > 1 && (t[0] == '-' || t[0] == '+'):
			if t[0] == '-' && strings.Contains(t[1:], "c") {
				cflag = true
			}
		default:
			return i, cflag, true
		}
	}
	return i, cflag, true
}

// literalBase returns the offset of src in the top-level command when wd is one quoted or literal
// part whose source text is exactly src, else -1.
func literalBase(sc scope, wd *syntax.Word, src string) int {
	if sc.base < 0 || len(wd.Parts) != 1 {
		return -1
	}
	var off int
	switch p := wd.Parts[0].(type) {
	case *syntax.SglQuoted:
		if p.Dollar {
			return -1
		}
		off = int(p.Left.Offset()) + 1
	case *syntax.DblQuoted:
		off = int(p.Left.Offset()) + 1
	case *syntax.Lit:
		off = int(p.ValuePos.Offset())
	default:
		return -1
	}
	if off+len(src) > len(sc.src) || sc.src[off:off+len(src)] != src {
		return -1
	}
	return sc.base + off
}

// cd returns the cwd after a successful `cd` with argv.
func (w *walker) cd(argv []Word, cwd string) string {
	var operands []Word
	opts := true
	for _, a := range argv[1:] {
		switch {
		case !a.Static:
			return ""
		case opts && a.Text == "--":
			opts = false
		case opts && len(a.Text) > 1 && a.Text[0] == '-':
		default:
			operands = append(operands, a)
		}
	}
	switch {
	case len(operands) == 0:
		return w.home
	case len(operands) > 1 || operands[0].Text == "-" || operands[0].Text == "":
		return ""
	}
	dir, ok := paths.Resolve(w.home, cwd, operands[0].Text, operands[0].Tilde)
	if !ok || dir == paths.ShellRoot {
		return ""
	}
	return dir
}

// stripWrappers returns the index of the real argv0 in words, the indexes of `env NAME=value`
// assignment words, and whether a wrapper changed directory (`env -C`). assignLike marks the words
// that start with `NAME=`.
func stripWrappers(words []Word, assignLike []bool) (int, []int, bool) {
	var assigns []int
	chdir := false
	i := 0
	for i < len(words) && words[i].Static {
		switch baseName(words[i].Text) {
		case "env":
			var n int
			var a []int
			n, a, chdir = skipEnv(words, assignLike, i+1, chdir)
			assigns = append(assigns, a...)
			i = n
		case "command":
			i = skipOpts(words, i+1, nil)
		case "nohup":
			i = skipOpts(words, i+1, nil)
		case "time":
			i = skipOpts(words, i+1, map[string]bool{"-o": true, "-f": true})
		case "mise":
			n, ok := skipMiseExec(words, i)
			if !ok {
				return i, assigns, chdir
			}
			i = n
		default:
			return i, assigns, chdir
		}
	}
	return i, assigns, chdir
}

// skipOpts skips option words starting at i; withValue lists options that take the next word.
func skipOpts(words []Word, i int, withValue map[string]bool) int {
	for i < len(words) && words[i].Static && strings.HasPrefix(words[i].Text, "-") {
		t := words[i].Text
		i++
		if t == "--" {
			break
		}
		if withValue[t] {
			i++
		}
	}
	return i
}

// skipEnv skips env's options and NAME=value words starting at i.
func skipEnv(words []Word, assignLike []bool, i int, chdir bool) (int, []int, bool) {
	for i < len(words) && words[i].Static && strings.HasPrefix(words[i].Text, "-") {
		t := words[i].Text
		i++
		if t == "--" {
			break
		}
		switch {
		case t == "-u" || t == "-S" || t == "--unset" || t == "--split-string":
			i++
		case t == "-C" || t == "--chdir":
			i++
			chdir = true
		case strings.HasPrefix(t, "--chdir=") || (strings.HasPrefix(t, "-C") && len(t) > 2):
			chdir = true
		}
	}
	var assigns []int
	for i < len(words) && assignLike[i] {
		assigns = append(assigns, i)
		i++
	}
	return i, assigns, chdir
}

// skipMiseExec skips `mise exec … --` / `mise x … --` at i.
func skipMiseExec(words []Word, i int) (int, bool) {
	if i+1 >= len(words) || !words[i+1].Static ||
		(words[i+1].Text != "exec" && words[i+1].Text != "x") {
		return i, false
	}
	for j, w := range words[i+1:] {
		if w.Static && w.Text == "--" {
			return i + 1 + j + 1, true
		}
	}
	return i, false
}

// word evaluates a static word.
func word(wd *syntax.Word) Word {
	if wd == nil || len(wd.Parts) == 0 {
		return Word{Static: true}
	}
	var b strings.Builder
	static := true
	for _, p := range wd.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			b.WriteString(unescape(p.Value, false))
		case *syntax.SglQuoted:
			if !p.Dollar {
				b.WriteString(p.Value)
				break
			}
			// ANSI-C `$'…'`: decoded by mvdan's expander, which needs no environment for it.
			s, err := expand.Literal(nil, &syntax.Word{Parts: []syntax.WordPart{p}})
			if err != nil {
				static = false
			}
			b.WriteString(s)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					static = false
					break
				}
				b.WriteString(unescape(lit.Value, true))
			}
		default:
			static = false
		}
	}
	if !static {
		return Word{}
	}
	lit, firstLit := wd.Parts[0].(*syntax.Lit)
	return Word{
		Text:   b.String(),
		Static: true,
		Tilde:  firstLit && strings.HasPrefix(lit.Value, "~"),
	}
}

// unescape removes the backslashes bash removes: in unquoted text every `\x` becomes `x`; inside
// double quotes only before `$`, backtick, `"`, `\` and newline. A backslash-newline is removed.
func unescape(s string, dquoted bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		next := s[i+1]
		switch {
		case next == '\n':
			i++
		case !dquoted || strings.IndexByte("$`\"\\", next) >= 0:
			b.WriteByte(next)
			i++
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}

// baseName returns the last component of a command name, without `.exe`.
func baseName(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
		name = name[:len(name)-4]
	}
	return name
}
