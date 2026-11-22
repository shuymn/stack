package powershell

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/shell"
)

// The oracle of powershell-guard §3 and §9 W2: the installed PowerShell engines (5.1 and 7 on
// Windows, 7 elsewhere) parse the declared corpus (testdata/oracle_cases.json) with their own
// parser (testdata/oracle.ps1). Every case declares, independently of Parse, the command names
// at supported positions, the names the engine finds in opaque positions, the calls that come
// from nested scripts and the engines that reject the script. The engine's observation and the
// recognizer's result must both match the declaration, and agree on command kind and argv. Native
// calls also run against an argv-printing stub (this test binary), and the argv it receives is
// compared with ShellCall. Nothing else runs: cmdlets are parse-only.
//
// It runs only with STACK_PS_ORACLE=1 (the powershell-oracle CI job). -update-oracle rewrites the
// declarations from a Windows run with both engines; review the diff before committing it.

const (
	oracleEnv = "STACK_PS_ORACLE"      // set to 1 to run the oracle
	stubEnv   = "STACK_PS_ORACLE_STUB" // the file the stub appends its argv to
	stubIDEnv = "STACK_PS_ORACLE_ID"   // the call the stub runs for
	casesFile = "oracle_cases.json"
)

var updateOracle = flag.Bool("update-oracle", false, "rewrite testdata/"+casesFile)

func TestMain(m *testing.M) {
	if out := os.Getenv(stubEnv); out != "" {
		os.Exit(runStub(out))
	}
	os.Exit(m.Run())
}

// runStub appends {"id", "argv"} to out: the argv a native call received.
func runStub(out string) int {
	line, err := json.Marshal(stubRecord{ID: os.Getenv(stubIDEnv), Argv: os.Args[1:]})
	if err != nil {
		return 1
	}
	//nolint:gosec // G703: out is the test's own temp file, passed by the oracle run.
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 1
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr != nil || cerr != nil {
		return 1
	}
	return 0
}

type stubRecord struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
}

// oracleTimeout bounds one PowerShell process: parse the corpus, resolve names, run the stubs.
const oracleTimeout = 2 * time.Minute

// declared is one corpus case and what it must yield. Names are multisets in sorted order.
type declared struct {
	Cmd string `json:"cmd"`
	// Calls are the static command names at supported positions: the engine finds them and Parse
	// returns them.
	Calls []string `json:"calls"`
	// Skipped are the names the engine finds in positions Parse leaves opaque.
	Skipped []string `json:"skipped,omitempty"`
	// Nested are the names Parse returns from nested PowerShell scripts (string arguments to the
	// engine).
	Nested []string `json:"nested,omitempty"`
	// Rejects are the engines ("5.1", "7") that fail to parse the script.
	Rejects []string `json:"rejects,omitempty"`
}

// Oracle output (testdata/oracle.ps1).
type (
	oElem struct {
		K      string  `json:"k"`
		T      string  `json:"t"`
		V      string  `json:"v"`
		Q      string  `json:"q"`
		Prefix string  `json:"prefix"`
		Arg    *oElem  `json:"arg"`
		Items  []oElem `json:"items"`
	}
	oCmd struct {
		Name     *string `json:"name"`
		Ctx      string  `json:"ctx"`
		End      int     `json:"end"`
		Elements []oElem `json:"elements"`
		Inert    bool    `json:"inert"`
	}
	oErr struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	oCase struct {
		ID       int    `json:"id"`
		Errors   []oErr `json:"errors"`
		Commands []oCmd `json:"commands"`
	}
	oParam struct {
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
		Switch  bool     `json:"switch"`
	}
	oracleOut struct {
		Version               string              `json:"version"`
		Edition               string              `json:"edition"`
		LanguageMode          string              `json:"languageMode"`
		NativeArgumentPassing string              `json:"nativeArgumentPassing"`
		Home                  string              `json:"home"`
		Cases                 []oCase             `json:"cases"`
		Resolved              map[string]string   `json:"resolved"`
		AliasesOf             map[string][]string `json:"aliasesOf"`
		Params                map[string][]oParam `json:"params"`
		PBinding              string              `json:"pBinding"`
		StubRuns              int                 `json:"stubRuns"`
		StubSame              map[string]string   `json:"stubSame"`
	}
)

// resolve returns how the engine resolves a command name. PowerShell's hashtables fold case, so
// `gh.exe` and `GH.EXE` share one key.
func (o *oracleOut) resolve(name string) string {
	if r, ok := o.Resolved[name]; ok {
		return r
	}
	for k, r := range o.Resolved {
		if strings.EqualFold(k, name) {
			return r
		}
	}
	return ""
}

// key names the engine in declarations: "5.1" or "7", "" for any other version.
func (o *oracleOut) key() string {
	switch {
	case strings.HasPrefix(o.Version, "5.1."):
		return "5.1"
	case strings.HasPrefix(o.Version, "7."):
		return "7"
	}
	return ""
}

// Verdict is the oracle's judgement of one case.
type Verdict int

// Verdicts. A case passes only on positive evidence from the engine.
const (
	Inconclusive Verdict = iota
	Pass
	Fail
)

func (v Verdict) String() string {
	return [...]string{"inconclusive", "pass", "fail"}[v]
}

// documentedParseErrors are corpus commands the engine rejects while the recognizer returns calls:
// the recognizer does not detect every syntax error (§3), so it may judge a script that pwsh
// refuses to run, which only over-judges a command that runs nothing.
var documentedParseErrors = map[string]string{
	`$x.Invoke(gh pr view 7); [IO.File]::ReadAllText((gh pr view 1)); (Get-Item a).Delete(gh x); git status`: "method arguments are expressions: a bare command there is a syntax error",
	`$x.M($(gh pr merge 1)); Write-Output $x.Invoke(gh pr view 7)`:                                           "method arguments are expressions: a bare command there is a syntax error",
}

// evidence is what the verdict of one case on one engine is drawn from.
type evidence struct {
	text   string
	decl   *declared // nil when the case has no declaration
	engine *oracleOut
	oc     *oCase // nil when the engine reported nothing for the case
	r      Result
	stub   map[string][]string // stub argv by "<case>/<command index>"
}

// stats counts the comparisons behind the verdicts, for the log.
type stats struct {
	calls, argvWords, stubCalls, stubWords, notes int
}

// observation is what one engine and the recognizer yield for one case.
type observation struct {
	rejects                bool
	calls, skipped, nested []string
	fails, notes, missing  []string
}

// verdict judges one case: Pass only when the engine's observation and the recognizer's result
// match the declaration and agree on every supported position, Fail on a disagreement,
// Inconclusive when the engine's evidence is missing or incomplete.
func verdict(e evidence, st *stats) (Verdict, []string) {
	if e.engine == nil || e.engine.LanguageMode != "FullLanguage" || e.oc == nil {
		return Inconclusive, []string{"no engine evidence"}
	}
	ob := observe(e, st)
	if len(ob.missing) > 0 {
		return Inconclusive, ob.missing
	}
	if e.decl == nil {
		return Fail, []string{"no declaration in testdata/" + casesFile}
	}
	fails, notes := ob.fails, ob.notes
	if want := slices.Contains(e.decl.Rejects, e.engine.key()); ob.rejects != want {
		fails = append(fails, fmt.Sprintf("engine rejects = %v, declared %v", ob.rejects, want))
	}
	if !ob.rejects {
		for _, d := range []struct {
			what      string
			got, want []string
		}{
			{"calls", ob.calls, e.decl.Calls},
			{"skipped", ob.skipped, e.decl.Skipped},
			{"nested", ob.nested, e.decl.Nested},
		} {
			if !slices.Equal(d.got, d.want) && (len(d.got) > 0 || len(d.want) > 0) {
				fails = append(fails, fmt.Sprintf("%s %q, declared %q", d.what, d.got, d.want))
			}
		}
	}
	st.notes += len(notes)
	if len(fails) > 0 {
		return Fail, append(fails, notes...)
	}
	return Pass, notes
}

// observe compares the engine's commands with the recognizer's calls and collects the names on
// each side.
func observe(e evidence, st *stats) observation {
	var ob observation
	if len(e.oc.Errors) > 0 {
		ob.rejects = true
		ids := make([]string, len(e.oc.Errors))
		chain := true
		for i, er := range e.oc.Errors {
			ids[i] = er.ID + "@" + strconv.Quote(er.Text)
			chain = chain && (er.Text == "&&" || er.Text == "||" || er.Text == "&")
		}
		switch {
		case e.r.Err != nil:
			ob.notes = []string{"both reject: " + strings.Join(ids, ", ")}
		case chain && e.engine.key() == "5.1":
			ob.notes = []string{
				"documented: 5.1 rejects &&, || and a trailing & (" + strings.Join(ids, ", ") + ")",
			}
		case documentedParseErrors[e.text] != "":
			ob.notes = []string{"documented: " + documentedParseErrors[e.text]}
		default:
			ob.fails = []string{
				"engine rejects (" + strings.Join(ids, ", ") + ") but the recognizer returns calls",
			}
		}
		return ob
	}
	if e.r.Err != nil {
		ob.fails = []string{"the recognizer rejects what the engine parses: " + e.r.Err.Error()}
		return ob
	}

	nested := nestedKeys(e.r)
	cmds := e.oc.Commands
	idx := make([]int, len(cmds))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmds[a].End - cmds[b].End })
	used := make([]bool, len(e.r.Calls))
	for _, i := range idx {
		oc := cmds[i]
		if oc.Name == nil {
			continue // a dynamic name or an invoked block: no name to compare
		}
		if len(oc.Elements) == 0 {
			ob.missing = append(
				ob.missing,
				fmt.Sprintf("engine command %q without elements", *oc.Name),
			)
			continue
		}
		j := matchCall(e.r.Calls, used, *oc.Name, oc)
		switch {
		case j < 0 && oc.Ctx == "plain":
			ob.fails = append(ob.fails, fmt.Sprintf("missed call %q", *oc.Name))
		case j < 0:
			ob.skipped = append(ob.skipped, *oc.Name)
		case strings.HasPrefix(oc.Ctx, "opaque:"):
			used[j] = true
			ob.fails = append(ob.fails,
				fmt.Sprintf("call %q returned from an opaque position (%s)", *oc.Name, oc.Ctx))
		default:
			used[j] = true
			st.calls++
			ob.calls = append(ob.calls, *oc.Name)
			compareCall(e, e.r.Calls[j], oc, fmt.Sprintf("%d/%d", e.oc.ID, i), st, &ob)
		}
	}
	for j, c := range e.r.Calls {
		if used[j] || !c.Args[0].Static {
			continue
		}
		if nested[callKey(c)] {
			ob.nested = append(ob.nested, c.Args[0].Text)
			continue
		}
		ob.fails = append(ob.fails, fmt.Sprintf("call %q the engine does not find", argv(c)))
	}
	slices.Sort(ob.calls)
	slices.Sort(ob.skipped)
	slices.Sort(ob.nested)
	return ob
}

// matchCall returns the first unused call named name, preferring one whose static words agree with
// the engine's, or -1.
func matchCall(calls []Call, used []bool, name string, oc oCmd) int {
	first := -1
	for j, c := range calls {
		if used[j] || !c.Args[0].Static || c.Args[0].Text != name {
			continue
		}
		if first < 0 {
			first = j
		}
		if f, _ := compareArgv(c, oc); len(f) == 0 {
			return j
		}
	}
	return first
}

// nestedKeys returns the calls that come from nested PowerShell scripts: the engine sees those as
// string arguments, and the nested texts are corpus cases of their own.
func nestedKeys(r Result) map[string]bool {
	keys := map[string]bool{}
	for _, s := range r.Nested {
		if s.Dialect != PowerShell || s.Err != nil {
			continue
		}
		p := newParser(s.Text, "", "", HostOS(), s.Depth, knownSet{})
		if p.run() != nil {
			continue
		}
		for _, c := range p.res.Calls {
			keys[callKey(c)] = true
		}
	}
	return keys
}

func callKey(c Call) string { return strings.Join(argv(c), "\x00") }

// compareCall compares the command kind, the argv and, for a native call the engine may run, the
// argv the stub received.
func compareCall(e evidence, c Call, oc oCmd, id string, st *stats, ob *observation) {
	name := *oc.Name
	res := e.engine.resolve(name)
	judged := func(n string) bool { _, ok := canonical[strings.ToLower(n)]; return ok }
	switch {
	case res == "":
		ob.missing = append(ob.missing, fmt.Sprintf("%q: no engine resolution", name))
	case strings.HasPrefix(res, "cmdlet:"):
		target := strings.TrimPrefix(res, "cmdlet:")
		if c.Kind != Cmdlet || !strings.EqualFold(c.Name, target) {
			msg := fmt.Sprintf(
				"%q: engine %s, recognizer kind %d name %q",
				name,
				res,
				c.Kind,
				c.Name,
			)
			if judged(target) || (c.Kind == Cmdlet && judged(c.Name)) {
				ob.fails = append(ob.fails, msg)
			} else {
				ob.notes = append(ob.notes, msg)
			}
		}
	case res == "native":
		if c.Kind == Cmdlet {
			msg := fmt.Sprintf("%q: engine native, recognizer cmdlet %q", name, c.Name)
			if judged(c.Name) {
				ob.fails = append(ob.fails, msg)
			} else {
				ob.notes = append(ob.notes, msg)
			}
		}
	default:
		ob.notes = append(ob.notes, fmt.Sprintf("%q resolves to %s", name, res))
	}
	f, n := compareArgv(c, oc)
	ob.fails = append(ob.fails, f...)
	ob.notes = append(ob.notes, n...)
	st.argvWords += len(c.Args) - 1

	got, ok := e.stub[id]
	switch {
	case ok:
		st.stubCalls++
		legacy := e.engine.NativeArgumentPassing == "" ||
			e.engine.NativeArgumentPassing == "Legacy"
		f, n := compareStub(c, got, legacy, st)
		ob.fails = append(ob.fails, f...)
		ob.notes = append(ob.notes, n...)
	case res == "native" && oc.Inert:
		ob.missing = append(ob.missing, fmt.Sprintf("%q: no stub argv for a native call", name))
	}
}

// want is one argv word as the engine parses it.
type want struct {
	text            string
	static, block   bool
	skip            bool // not comparable (a native list)
	verbatimSkipped bool // the rest of argv is a `--%` tail not compared
}

// expected turns the engine's command elements into argv words for a call of kind k.
func expected(els []oElem, k Kind) []want {
	var out []want
	for i, e := range els {
		switch e.K {
		case "str":
			if e.Q == "BareWord" && e.V == "--%" {
				if i+1 < len(els) {
					tail := els[i+1].V
					if strings.ContainsAny(tail, "\"\u201c\u201d\u201e%") {
						return append(out, want{verbatimSkipped: true})
					}
					for f := range strings.FieldsSeq(tail) {
						out = append(out, want{text: f, static: true})
					}
				}
				return out
			}
			out = append(out, want{text: e.V, static: true})
		case "param":
			switch {
			case e.Arg == nil:
				out = append(out, want{text: e.T, static: true})
			case e.Arg.K == "str":
				out = append(out, want{text: e.Prefix + e.Arg.V, static: true})
			default:
				out = append(out, want{})
			}
		case "array":
			if k == Native {
				out = append(out, want{skip: true})
				continue
			}
			for _, it := range e.Items {
				out = append(out, expected([]oElem{it}, k)...)
			}
		case "block":
			out = append(out, want{block: true})
		default:
			out = append(out, want{})
		}
	}
	return out
}

// compareArgv compares the recognizer's words after the name with the engine's elements: a static
// word must equal the engine's static value, and no word the engine computes may be static.
func compareArgv(c Call, oc oCmd) ([]string, []string) {
	var fails, notes []string
	ws := expected(oc.Elements[1:], c.Kind)
	got := c.Args[1:]
	if n := len(ws); n > 0 && ws[n-1].verbatimSkipped {
		ws = ws[:n-1]
		notes = append(notes, "--% tail with quotes or %NAME% not compared")
		if len(got) > len(ws) {
			got = got[:len(ws)]
		}
	}
	if len(ws) != len(got) {
		return []string{
			fmt.Sprintf("argv %q has %d words, engine %d", argv(c), len(got), len(ws)),
		}, notes
	}
	for i, w := range ws {
		g := got[i]
		switch {
		case w.skip:
		case w.block:
			if !g.Block {
				fails = append(fails, fmt.Sprintf("word %d %q: engine block", i+1, g.Raw))
			}
		case !w.static:
			if g.Static && g.Known == NotKnown && !g.Block {
				fails = append(fails,
					fmt.Sprintf("word %d %q static, engine computes it", i+1, g.Raw))
			}
		case !g.Static || g.Known != NotKnown:
			notes = append(notes,
				fmt.Sprintf("word %d %q dynamic, engine static %q", i+1, g.Raw, w.text))
		case g.Flag+g.Text != w.text:
			fails = append(fails,
				fmt.Sprintf("word %d = %q, engine %q", i+1, g.Flag+g.Text, w.text))
		}
	}
	return fails, notes
}

// compareStub compares ShellCall's static words with the argv the stub received. A word starting
// with an unquoted `~` is not compared (W3 owns the native `~` rule). Under legacy argument
// passing (Windows PowerShell 5.1, or `$PSNativeCommandArgumentPassing = 'Legacy'`) a word holding
// `"` reaches the program split or unquoted: documented in phase2-core §6, so such a call is
// noted, not compared.
func compareStub(c Call, got []string, legacy bool, st *stats) ([]string, []string) {
	var fails, notes []string
	words := c.ShellCall().Args[1:]
	if legacy && slices.ContainsFunc(words,
		func(w shell.Word) bool { return strings.Contains(w.Text, `"`) }) {
		return nil, []string{fmt.Sprintf("documented: legacy argument passing, stub argv %q", got)}
	}
	allStatic := true
	for _, w := range words {
		allStatic = allStatic && w.Static
	}
	n := len(words)
	if n != len(got) {
		if allStatic {
			return []string{fmt.Sprintf("stub argv %q, ShellCall %q", got, shellTexts(words))}, nil
		}
		notes = append(notes,
			fmt.Sprintf("stub argv has %d words, ShellCall %d with dynamic words", len(got), n))
		n = 0
		for n < len(words) && n < len(got) && words[n].Static {
			n++
		}
	}
	for i := range n {
		w, a := words[i], c.Args[i+1]
		if !w.Static || (a.Quote == Bare && a.Flag == "" && strings.HasPrefix(a.Text, "~")) {
			continue
		}
		st.stubWords++
		if w.Text != got[i] {
			fails = append(fails,
				fmt.Sprintf("native word %d = %q, stub received %q", i+1, w.Text, got[i]))
		}
	}
	return fails, notes
}

func shellTexts(ws []shell.Word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		if w.Static {
			out[i] = w.Text
		} else {
			out[i] = "$"
		}
	}
	return out
}

// sourceCorpus returns the §7.2 commands: C1 from this package's tables, C2–C5 from the command
// lists of the guard tests, the controls, and every nested PowerShell script Parse finds in them.
func sourceCorpus(t *testing.T) []string {
	t.Helper()
	var texts []string
	for _, c := range controls {
		texts = append(texts, c.cmd)
	}
	for _, tt := range parseTests {
		texts = append(texts, tt.cmd)
	}
	for _, tt := range kindTests {
		texts = append(texts, tt.cmd)
	}
	for _, tt := range bindingTests {
		texts = append(texts, tt.cmd)
	}
	for _, tt := range nestedTests {
		texts = append(texts, tt.cmd)
	}
	texts = append(texts, parseFailures...)
	local := len(texts) // the guard lists also hold hook payloads (valid JSON): those are dropped
	for _, f := range []string{
		filepath.Join("..", "guard", "powershell_test.go"),
		filepath.Join("..", "..", "extensions", "guard", "powershell_test.go"),
	} {
		texts = append(texts, guardCommands(t, f)...)
	}
	seen := map[string]bool{}
	var out []string
	for i := 0; i < len(texts); i++ {
		s := texts[i]
		if seen[s] || strings.TrimSpace(s) == "" || (i >= local && json.Valid([]byte(s))) {
			continue
		}
		seen[s] = true
		out = append(out, s)
		for _, n := range Parse(s, "", "", Windows).Nested {
			if n.Dialect == PowerShell {
				texts = append(texts, n.Text)
			}
		}
	}
	return out
}

// guardCommands returns the string literals of the []string lists in the PowerShell tests of file.
func guardCommands(t *testing.T, file string) []string {
	t.Helper()
	f, err := goparser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("corpus source: %v", err)
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "Test") ||
			!strings.Contains(fn.Name.Name, "PowerShell") {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if at, ok := cl.Type.(*ast.ArrayType); !ok || fmt.Sprint(at.Elt) != "string" {
				return true
			}
			for _, el := range cl.Elts {
				if bl, ok := el.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					if s, err := strconv.Unquote(bl.Value); err == nil {
						out = append(out, s)
					}
				}
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("no commands in %s", file)
	}
	return out
}

// control is a positive control: a case whose engine evidence is fixed here, so a broken oracle
// (a parser that finds nothing, a stub that records nothing) is inconclusive, never a pass.
type control struct {
	cmd string
	// check returns why the engine's evidence for the case is not the known one, or "".
	check func(o *oracleOut, oc *oCase, stub map[string][]string) string
}

var controls = []control{
	{"gh pr merge 1 --help", func(o *oracleOut, oc *oCase, stub map[string][]string) string {
		if len(oc.Errors) > 0 || len(oc.Commands) != 1 {
			return "want one command"
		}
		c := oc.Commands[0]
		var vals []string
		for _, el := range c.Elements {
			vals = append(vals, el.V)
		}
		want := []string{"gh", "pr", "merge", "1", "--help"}
		if c.Name == nil || *c.Name != "gh" || c.Ctx != "plain" || !slices.Equal(vals, want) {
			return fmt.Sprintf("command %q in %s", vals, c.Ctx)
		}
		if got := stub[strconv.Itoa(oc.ID)+"/0"]; !slices.Equal(got, want[1:]) {
			return fmt.Sprintf("stub argv %q", got)
		}
		if o.resolve("gh") != "native" {
			return "gh resolves to " + o.resolve("gh")
		}
		return ""
	}},
	{"$p = { gh pr merge 7 }", func(_ *oracleOut, oc *oCase, _ map[string][]string) string {
		if len(oc.Errors) > 0 || len(oc.Commands) != 1 ||
			oc.Commands[0].Ctx != "opaque:block-value" {
			return "want one command in an assigned block"
		}
		return ""
	}},
	{
		"git status && gh pr merge 1 || gh x",
		func(o *oracleOut, oc *oCase, _ map[string][]string) string {
			if rejects := len(oc.Errors) > 0; rejects != (o.key() == "5.1") {
				return fmt.Sprintf("rejects = %v on %s", rejects, o.Version)
			}
			if o.key() == "7" && len(oc.Commands) != 3 {
				return fmt.Sprintf("%d commands, want 3", len(oc.Commands))
			}
			return ""
		},
	},
	{`Remove-Item -Path 'a,C:\'`, func(o *oracleOut, oc *oCase, _ map[string][]string) string {
		if len(oc.Commands) != 1 || o.resolve("Remove-Item") != "cmdlet:Remove-Item" {
			return "want one Remove-Item cmdlet"
		}
		return ""
	}},
}

// runOracle runs testdata/oracle.ps1 on exe over the corpus and returns its output and the stub
// records.
func runOracle(t *testing.T, exe string, corpus []string) (*oracleOut, map[string][]string, error) {
	t.Helper()
	dir := t.TempDir()
	stub, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("oracle run: %w", err)
	}
	type tcase struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
	}
	cases := make([]tcase, len(corpus))
	for i, s := range corpus {
		cases[i] = tcase{i, s}
	}
	req := map[string]any{
		"cases":   cases,
		"names":   slices.Sorted(maps.Keys(aliases)),
		"cmdlets": slices.Sorted(maps.Values(canonical)),
		"tables":  slices.Sorted(maps.Keys(tables)),
		"stub":    stub,
		"missing": filepath.Join(dir, "missing"),
	}
	in := filepath.Join(dir, "in.json")
	out := filepath.Join(dir, "out.json")
	argvFile := filepath.Join(dir, "argv.jsonl")
	b, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("oracle run: %w", err)
	}
	if err := os.WriteFile(in, b, 0o600); err != nil {
		return nil, nil, fmt.Errorf("oracle run: %w", err)
	}
	script, err := filepath.Abs(filepath.Join("testdata", "oracle.ps1"))
	if err != nil {
		return nil, nil, fmt.Errorf("oracle run: %w", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), oracleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-File", script, "-In", in, "-Out", out)
	cmd.Dir = dir // a native glob expands against an empty directory
	cmd.Env = append(os.Environ(), stubEnv+"="+argvFile)
	cmd.WaitDelay = 5 * time.Second
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w: %s", filepath.Base(exe), err, msg)
	}
	b, err = os.ReadFile(out)
	if err != nil {
		return nil, nil, fmt.Errorf("oracle run: %w", err)
	}
	var o oracleOut
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, nil, fmt.Errorf("oracle output: %w", err)
	}
	stubs := map[string][]string{}
	if f, err := os.Open(argvFile); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var r stubRecord
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				_ = f.Close()
				return nil, nil, fmt.Errorf("stub record: %w", err)
			}
			stubs[r.ID] = r.Argv
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return nil, nil, fmt.Errorf("oracle run: %w", err)
		}
	}
	if len(stubs) != o.StubRuns {
		return nil, nil, fmt.Errorf("%d stub records for %d runs", len(stubs), o.StubRuns)
	}
	for id, ran := range o.StubSame {
		argv, ok := stubs[ran]
		if !ok {
			return nil, nil, fmt.Errorf("stub call %s refers to %s, which has no record", id, ran)
		}
		stubs[id] = argv
	}
	return &o, stubs, checkEngine(&o, corpus, stubs)
}

// checkEngine returns why the engine's output is not complete evidence: a language mode other
// than FullLanguage, an unknown version, a missing or repeated case, or a failed control.
func checkEngine(o *oracleOut, corpus []string, stubs map[string][]string) error {
	if o.LanguageMode != "FullLanguage" {
		return fmt.Errorf("LanguageMode %q", o.LanguageMode)
	}
	if o.key() == "" {
		return fmt.Errorf("engine version %q", o.Version)
	}
	if len(corpus) == 0 || o.StubRuns == 0 {
		return fmt.Errorf("%d cases, %d stub runs", len(corpus), o.StubRuns)
	}
	if len(o.Cases) != len(corpus) {
		return fmt.Errorf("%d cases for a corpus of %d", len(o.Cases), len(corpus))
	}
	for i, c := range o.Cases {
		if c.ID != i {
			return fmt.Errorf("case %d reported as %d", i, c.ID)
		}
	}
	for _, c := range controls {
		i := slices.Index(corpus, c.cmd)
		if i < 0 {
			return fmt.Errorf("control %q not in the corpus", c.cmd)
		}
		if why := c.check(o, &o.Cases[i], stubs); why != "" {
			return fmt.Errorf("control %q: %s", c.cmd, why)
		}
	}
	return nil
}

// engines lists the PowerShell engines the oracle needs on this host.
func engines() []struct{ key, exe string } {
	if runtime.GOOS == "windows" {
		return []struct{ key, exe string }{{"5.1", "powershell.exe"}, {"7", "pwsh.exe"}}
	}
	return []struct{ key, exe string }{{"7", "pwsh"}}
}

func readDeclared(t *testing.T) []declared {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", casesFile))
	if *updateOracle && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("declarations: %v", err)
	}
	var ds []declared
	if err := json.Unmarshal(b, &ds); err != nil {
		t.Fatalf("declarations: %v", err)
	}
	return ds
}

// TestOracle is W2 of powershell-guard §9: the recognizer against the installed PowerShell
// engines. An engine that is missing, fails, times out or yields incomplete evidence makes the
// test fail as inconclusive.
func TestOracle(t *testing.T) {
	if os.Getenv(oracleEnv) != "1" {
		t.Skip(oracleEnv + "=1 runs the PowerShell oracle (CI job powershell-oracle)")
	}
	decls := readDeclared(t)
	byCmd := map[string]*declared{}
	corpus := make([]string, len(decls))
	for i := range decls {
		corpus[i] = decls[i].Cmd
		byCmd[decls[i].Cmd] = &decls[i]
	}
	if *updateOracle {
		corpus = sourceCorpus(t)
	} else {
		for _, s := range sourceCorpus(t) {
			if byCmd[s] == nil {
				t.Errorf("command %q is not declared: rerun with -update-oracle on Windows", s)
			}
		}
	}

	ens := engines()
	type result struct {
		o     *oracleOut
		stubs map[string][]string
		err   error
	}
	results := make([]result, len(ens))
	var wg sync.WaitGroup
	for i, en := range ens {
		exe, err := exec.LookPath(en.exe)
		if err != nil {
			results[i].err = fmt.Errorf("PowerShell %s (%s) not found: %w", en.key, en.exe, err)
			continue
		}
		wg.Go(func() {
			r := &results[i]
			r.o, r.stubs, r.err = runOracle(t, exe, corpus)
			if r.err == nil && r.o.key() != en.key {
				r.err = fmt.Errorf("%s is PowerShell %s, want %s", en.exe, r.o.Version, en.key)
			}
		})
	}
	wg.Wait()

	obs := make([][]observation, len(ens))
	for i, en := range ens {
		t.Run(en.key, func(t *testing.T) {
			res := results[i]
			if res.err != nil {
				t.Fatalf("inconclusive: all %d cases: %v", len(corpus), res.err)
			}
			o := res.o
			t.Logf("engine %s %s on %s, LanguageMode %s, PSNativeCommandArgumentPassing %q, "+
				"corpus %d, stub runs %d", o.Version, o.Edition, runtime.GOOS, o.LanguageMode,
				o.NativeArgumentPassing, len(corpus), o.StubRuns)
			var st stats
			counts := map[Verdict]int{}
			obs[i] = make([]observation, len(corpus))
			for id, text := range corpus {
				e := evidence{
					text: text, decl: byCmd[text], engine: o, oc: &o.Cases[id],
					r:    Parse(text, "", o.Home, HostOS()),
					stub: res.stubs,
				}
				obs[i][id] = observe(e, &stats{})
				if *updateOracle {
					continue
				}
				v, notes := verdict(e, &st)
				counts[v]++
				switch v {
				case Pass:
					for _, n := range notes {
						if strings.HasPrefix(n, "documented") {
							t.Logf("case %d %q: %s", id, text, n)
						}
					}
				case Fail, Inconclusive:
					t.Errorf("case %d %q: %s: %s", id, text, v, strings.Join(notes, "; "))
				}
			}
			t.Logf("verdicts on %s: %d pass, %d fail, %d inconclusive; compared %d calls, "+
				"%d argv words, %d stub calls with %d static words; %d notes",
				en.key, counts[Pass], counts[Fail], counts[Inconclusive], st.calls,
				st.argvWords, st.stubCalls, st.stubWords, st.notes)
			checkHost(t, o)
		})
	}
	if *updateOracle {
		writeDeclared(t, ens, corpus, obs)
	}
}

// writeDeclared rewrites the declarations from the observations of every engine of a Windows host.
func writeDeclared(t *testing.T, ens []struct{ key, exe string }, corpus []string,
	obs [][]observation,
) {
	t.Helper()
	if len(ens) != 2 || t.Failed() {
		t.Fatal("-update-oracle needs a clean run of both engines on Windows")
	}
	out := make([]declared, len(corpus))
	for id, text := range corpus {
		d := declared{Cmd: text}
		for i, en := range ens {
			ob := obs[i][id]
			if len(ob.fails) > 0 || len(ob.missing) > 0 {
				t.Fatalf("case %q on %s: %q %q", text, en.key, ob.fails, ob.missing)
			}
			if ob.rejects {
				d.Rejects = append(d.Rejects, en.key)
				continue
			}
			if d.Calls != nil && (!slices.Equal(d.Calls, ob.calls) ||
				!slices.Equal(d.Skipped, ob.skipped) || !slices.Equal(d.Nested, ob.nested)) {
				t.Fatalf("case %q: the engines disagree", text)
			}
			d.Calls, d.Skipped, d.Nested = ob.calls, ob.skipped, ob.nested
			if d.Calls == nil {
				d.Calls = []string{}
			}
		}
		if d.Calls == nil {
			d.Calls = []string{}
		}
		out[id] = d
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join("testdata", casesFile),
		append(b, '\n'),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

// checkHost checks the alias table of §4 and the binding tables against the engine's metadata,
// and the `-p` ambiguity of Remove-Item.
func checkHost(t *testing.T, o *oracleOut) {
	t.Helper()
	host := HostOS()
	for _, n := range slices.Sorted(maps.Keys(aliases)) {
		a := aliases[n]
		got := o.resolve(n)
		if a.windows && host != Windows {
			if strings.HasPrefix(got, "cmdlet:") || strings.HasPrefix(got, "alias:") {
				t.Errorf(
					"%q is an alias (%s) on this host; the recognizer reads it as native",
					n,
					got,
				)
			}
			continue
		}
		if got != "cmdlet:"+a.name {
			t.Errorf("alias %q: engine %q, recognizer %s", n, got, a.name)
		}
	}
	for cmdlet, names := range o.AliasesOf {
		for _, n := range names {
			if a, ok := aliases[strings.ToLower(n)]; !ok || a.name != cmdlet ||
				(a.windows && host != Windows) {
				t.Errorf("alias %q of %s is not in the recognizer's table for this host", n, cmdlet)
			}
		}
	}
	rm := make([]string, 0, 6)
	for _, n := range []string{"rm", "rmdir", "ri", "del", "erase", "rd"} {
		rm = append(rm, n+"="+o.resolve(n))
	}
	t.Logf("Remove-Item names on %s: %s; aliases of Remove-Item: %v; Remove-Item -p: %s",
		runtime.GOOS, strings.Join(rm, " "), o.AliasesOf["Remove-Item"], o.PBinding)
	if !strings.HasPrefix(o.PBinding, "AmbiguousParameter,") {
		t.Errorf("Remove-Item -p: %q, want AmbiguousParameter", o.PBinding)
	}
	var pNames []string
	for _, p := range o.Params["Remove-Item"] {
		for _, n := range append([]string{p.Name}, p.Aliases...) {
			if strings.HasPrefix(strings.ToLower(n), "p") {
				pNames = append(pNames, p.Name)
				break
			}
		}
	}
	if _, ok := tables["Remove-Item"].lookup("p"); ok || len(pNames) < 2 {
		t.Errorf("Remove-Item -p: engine parameters %q, recognizer binds it", pNames)
	}
	t.Logf("Remove-Item parameters matching -p in the engine's metadata: %v", pNames)
	checkTables(t, o)
}

// checkTables checks the binding tables against the engine's parameter metadata.
func checkTables(t *testing.T, o *oracleOut) {
	t.Helper()
	gaps := map[string][]string{}
	var extra []string
	for _, name := range slices.Sorted(maps.Keys(tables)) {
		tb := tables[name]
		ps, ok := o.Params[name]
		if !ok {
			t.Errorf("no parameter metadata for %s", name)
			continue
		}
		engine := map[string]bool{}
		for _, p := range ps {
			engine[strings.ToLower(p.Name)] = true
			q, ok := tb.lookup(p.Name)
			if !ok || !strings.EqualFold(q.name, p.Name) {
				if why := untabled(name, p.Name); why != "" {
					gaps[why] = append(gaps[why], name+" -"+p.Name)
				} else {
					t.Errorf("%s -%s: not in the recognizer's table", name, p.Name)
				}
				continue
			}
			if q.isSwitch != p.Switch {
				t.Errorf("%s -%s: switch %v, engine %v", name, p.Name, q.isSwitch, p.Switch)
			}
			for _, a := range p.Aliases {
				if q, ok := tb.lookup(a); !ok || !strings.EqualFold(q.name, p.Name) {
					t.Errorf("%s -%s alias %s: not in the recognizer's table", name, p.Name, a)
				}
			}
		}
		for _, p := range tb.params {
			if !engine[strings.ToLower(p.name)] {
				extra = append(extra, name+" -"+p.name)
			}
		}
	}
	for _, why := range slices.Sorted(maps.Keys(gaps)) {
		t.Logf("not in the binding tables (documented: %s): %s", why, strings.Join(gaps[why], ", "))
	}
	t.Logf("in the binding tables, not in this engine: %s", strings.Join(extra, ", "))
}

// whereOperators are Where-Object's comparison switches. Each belongs to a parameter set without
// -FilterScript, so a call that uses one runs no script block, and the recognizer, which refuses
// an unknown parameter, walks none either.
var whereOperators = func() map[string]bool {
	m := map[string]bool{}
	for op := range strings.FieldsSeq(`EQ NE GT GE LT LE Like NotLike Match NotMatch Contains
		NotContains In NotIn Is IsNot Not`) {
		m[strings.ToLower(op)] = true
		m["c"+strings.ToLower(op)] = true
	}
	return m
}()

// untabled says why an engine parameter is deliberately missing from a binding table, or "".
func untabled(cmdlet, param string) string {
	switch {
	case strings.EqualFold(param, "UseTransaction"):
		return "5.1 only; the FileSystem provider refuses transactions, so the call changes nothing"
	case cmdlet == "Where-Object" && whereOperators[strings.ToLower(param)]:
		return "a comparison parameter set without -FilterScript"
	}
	return ""
}
