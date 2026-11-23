package powershell

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuymn/stack/internal/shell"
)

// The oracle of powershell-guard §3 (C11): the installed PowerShell engines (5.1 and 7 on
// Windows, 7 elsewhere) parse the declared corpus (testdata/oracle_cases.json) with their own
// parser (testdata/oracle.ps1), which classifies each command's position by §3 from the engine's
// AST and static parameter binding and parses the static scripts of Invoke-Expression and
// pwsh -Command. Each case declares what the engine finds: the command names at supported
// positions, the names in opaque positions, the names from nested scripts and the engines that
// reject the script. The engine must match the declaration, and Parse must return exactly those
// calls, with the engine's command kind and argv. Native calls also run against an argv-printing
// stub (this test binary), and the argv it receives is compared with ShellCall. Nothing else runs:
// cmdlets are parse-only.
//
// It runs only with STACK_PS_ORACLE=1 (the powershell-oracle CI job). -update-oracle writes the
// declarations of new commands from a Windows run of both engines, never from Parse; a change to
// an existing declaration or a removed command also needs -update-oracle-changes, and the test
// prints the difference.

const (
	oracleEnv  = "STACK_PS_ORACLE"      // set to 1 to run the oracle
	stubEnv    = "STACK_PS_ORACLE_STUB" // the file the stub appends its argv to
	stubIDEnv  = "STACK_PS_ORACLE_ID"   // the call the stub runs for
	stubMarker = "--stack-ps-oracle-stub"
	casesFile  = "oracle_cases.json"
)

var (
	updateOracle  = flag.Bool("update-oracle", false, "declare new commands in testdata/"+casesFile)
	updateChanges = flag.Bool("update-oracle-changes", false,
		"with -update-oracle, also rewrite changed declarations and drop removed commands")
)

func TestMain(m *testing.M) {
	if out := os.Getenv(stubEnv); out != "" && len(os.Args) > 1 && os.Args[1] == stubMarker {
		os.Exit(runStub(out, os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runStub appends {"id", "argv"} to out: the argv a native call received.
func runStub(out string, argv []string) int {
	line, err := json.Marshal(stubRecord{ID: os.Getenv(stubIDEnv), Argv: argv})
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

// declared is one corpus case and what the engines yield for it. Names are sorted multisets.
type declared struct {
	Cmd string `json:"cmd"`
	// Calls are the static command names the engine finds at §3 positions.
	Calls []string `json:"calls"`
	// Skipped are the static names the engine finds in opaque positions.
	Skipped []string `json:"skipped,omitempty"`
	// Nested are the static names at §3 positions of the nested PowerShell scripts.
	Nested []string `json:"nested,omitempty"`
	// Rejects are the engines ("5.1", "7") that fail to parse the script.
	Rejects []string `json:"rejects,omitempty"`
	// DynamicUnix lists argument values the engine parses as static that Parse treats as dynamic
	// on non-Windows hosts by design: an unquoted wildcard, which pwsh globs there (§3 Words).
	// Written by hand; the writer keeps it.
	DynamicUnix []string `json:"dynamicUnix,omitempty"`
}

// Oracle output (testdata/oracle.ps1).
type (
	oElem struct {
		K      string   `json:"k"`
		T      string   `json:"t"`
		AST    string   `json:"ast"`    // the element's AST type
		Parts  []string `json:"nested"` // the AST types inside an expandable string
		V      string   `json:"v"`
		Q      string   `json:"q"`
		Prefix string   `json:"prefix"`
		Arg    *oElem   `json:"arg"`
		Items  []oElem  `json:"items"`
	}
	oCmd struct {
		Name     *string `json:"name"`
		Ctx      string  `json:"ctx"`
		End      int     `json:"end"`
		Elements []oElem `json:"elements"`
		Eligible bool    `json:"eligible"`
	}
	oErr struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
		Text   string `json:"text"`
	}
	oCase struct {
		ID       int       `json:"id"`
		Errors   []oErr    `json:"errors"`
		Commands []oCmd    `json:"commands"`
		Nested   []oNested `json:"nested"`
	}
	// oNested is a command of a nested script: at a §3 position (ctx "plain"), or one whose
	// position cannot be decided ("unknown:<why>").
	oNested struct {
		Name     *string `json:"name"`
		Ctx      string  `json:"ctx"`
		Elements []oElem `json:"elements"`
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
		Modules               []string            `json:"modules"`
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

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(\.\d+)?(-[0-9A-Za-z.-]+)?$`)

// key names the engine in declarations: "5.1" or "7", "" for any other or malformed version.
func (o *oracleOut) key() string {
	m := versionRe.FindStringSubmatch(o.Version)
	switch {
	case m == nil:
		return ""
	case m[1] == "5" && m[2] == "1":
		return "5.1"
	case m[1] == "7":
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
// refuses to run, which only over-judges a command that runs nothing. The engine's error-recovered
// AST is still compared.
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

// observation is what one engine yields for one case, and how Parse compares with it.
type observation struct {
	rejects, compared      bool
	calls, skipped, nested []string // the engine's, sorted
	fails, notes, missing  []string
}

// declaredBy returns the declaration the engine's observation implies.
func (ob observation) declaredBy(text string) declared {
	d := declared{Cmd: text, Calls: ob.calls, Skipped: ob.skipped, Nested: ob.nested}
	if d.Calls == nil {
		d.Calls = []string{}
	}
	return d
}

// verdict judges one case: Pass only when the engine's observation matches the declaration and
// Parse agrees with it on every supported position, Fail on a disagreement, Inconclusive when the
// engine's evidence is missing or incomplete.
func verdict(e evidence, ob observation, st *stats) (Verdict, []string) {
	if e.engine == nil || e.engine.LanguageMode != "FullLanguage" || e.oc == nil {
		return Inconclusive, []string{"no engine evidence"}
	}
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
	if ob.compared {
		for _, d := range []struct {
			what      string
			got, want []string
		}{
			{"calls", ob.calls, e.decl.Calls},
			{"skipped", ob.skipped, e.decl.Skipped},
			{"nested", ob.nested, e.decl.Nested},
		} {
			if !slices.Equal(d.got, d.want) {
				fails = append(
					fails,
					fmt.Sprintf("engine %s %q, declared %q", d.what, d.got, d.want),
				)
			}
		}
	}
	st.notes += len(notes)
	if len(fails) > 0 {
		return Fail, append(fails, notes...)
	}
	return Pass, notes
}

// observe collects the engine's names for one case and compares Parse with the engine's commands.
func observe(e evidence, st *stats) observation {
	var ob observation
	oc := *e.oc
	partial := len(oc.Errors) > 0 && documentedParseErrors[e.text] != ""
	if partial {
		// Only the commands that end before the first syntax error parse as written; the
		// engine's error recovery reads the rest differently.
		first := oc.Errors[0].Offset
		for _, er := range oc.Errors {
			first = min(first, er.Offset)
		}
		oc.Commands = slices.DeleteFunc(slices.Clone(oc.Commands),
			func(c oCmd) bool { return c.End > first })
		oc.Nested = nil
	}
	e.oc = &oc
	for _, oc := range e.oc.Commands {
		if strings.HasPrefix(oc.Ctx, "unknown:") {
			ob.missing = append(ob.missing, "position not decided: "+oc.Ctx)
		}
		if oc.Name == nil {
			continue
		}
		if oc.Ctx == "plain" {
			ob.calls = append(ob.calls, *oc.Name)
		} else {
			ob.skipped = append(ob.skipped, *oc.Name)
		}
	}
	for _, n := range e.oc.Nested {
		switch {
		case n.Ctx != "plain":
			ob.missing = append(ob.missing, "nested position not decided: "+n.Ctx)
		case n.Name != nil:
			ob.nested = append(ob.nested, *n.Name)
		}
	}
	slices.Sort(ob.calls)
	slices.Sort(ob.skipped)
	slices.Sort(ob.nested)

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
			return ob
		case chain && e.engine.key() == "5.1":
			ob.notes = []string{
				"documented: 5.1 rejects &&, || and a trailing & (" + strings.Join(ids, ", ") + ")",
			}
			return ob
		case partial:
			ob.notes = []string{"documented: " + documentedParseErrors[e.text] +
				"; the commands before the first error are compared"}
		default:
			ob.fails = []string{
				"engine rejects (" + strings.Join(ids, ", ") + ") but the recognizer returns calls",
			}
			return ob
		}
	} else if e.r.Err != nil {
		ob.fails = []string{"the recognizer rejects what the engine parses: " + e.r.Err.Error()}
		return ob
	}
	ob.compared = true
	compareCalls(e, st, &ob, partial)
	return ob
}

// compareCalls pairs the engine's commands with Parse's calls: every plain engine command must be
// returned, no opaque one may be, and every other call must be a dynamic call of the engine's or
// come from a nested script.
// A partial comparison (a script the engine rejects) checks only the engine's commands.
func compareCalls(e evidence, st *stats, ob *observation, partial bool) {
	cmds := e.oc.Commands
	idx := make([]int, len(cmds))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmds[a].End - cmds[b].End })
	used := make([]bool, len(e.r.Calls))
	dynamicEngine := 0
	var opaque []int
	// Supported top-level calls first.
	for _, i := range idx {
		oc := cmds[i]
		switch {
		case len(oc.Elements) == 0:
			ob.missing = append(ob.missing, fmt.Sprintf("engine command %d without elements", i))
		case oc.Ctx != "plain":
			opaque = append(opaque, i)
		case oc.Name == nil:
			// A dynamic name; an invoked block `& {…}` is no call of its own.
			if oc.Elements[0].K != "block" {
				dynamicEngine++
			}
		default:
			j := matchCall(e.r.Calls, used, *oc.Name, oc.Elements)
			if j < 0 {
				ob.fails = append(ob.fails, fmt.Sprintf("missed call %q", *oc.Name))
				continue
			}
			used[j] = true
			st.calls++
			compareCall(e, e.r.Calls[j], oc, fmt.Sprintf("%d/%d", e.oc.ID, i), st, ob)
		}
	}
	// Then the calls of nested scripts (none in a partial comparison): each engine record pairs
	// with a call of its name, and their static words are compared.
	for _, n := range e.oc.Nested {
		if n.Ctx != "plain" || n.Name == nil {
			continue
		}
		j := matchCall(e.r.Calls, used, *n.Name, n.Elements)
		if j < 0 {
			ob.fails = append(ob.fails, fmt.Sprintf("missed nested call %q", *n.Name))
			continue
		}
		used[j] = true
		f, notes := compareArgv(e.r.Calls[j], n.Elements, dynamicWords(e))
		ob.fails = append(ob.fails, prefixed("nested: ", f)...)
		ob.notes = append(ob.notes, prefixed("nested: ", notes)...)
	}
	// A call left over that an opaque command explains was returned from an opaque position.
	for _, i := range opaque {
		oc := cmds[i]
		if oc.Name == nil {
			continue
		}
		if j := matchCall(e.r.Calls, used, *oc.Name, oc.Elements); j >= 0 {
			used[j] = true
			ob.fails = append(ob.fails,
				fmt.Sprintf("call %q returned from an opaque position (%s)", *oc.Name, oc.Ctx))
		}
	}
	if partial {
		return
	}
	dynamicParse := 0
	for j, c := range e.r.Calls {
		switch {
		case used[j]:
		case !c.Args[0].Static:
			dynamicParse++
		default:
			ob.fails = append(ob.fails, fmt.Sprintf("call %q the engine does not find", argv(c)))
		}
	}
	if dynamicParse != dynamicEngine {
		ob.fails = append(ob.fails, fmt.Sprintf("%d calls with a dynamic name, engine %d",
			dynamicParse, dynamicEngine))
	}
}

func prefixed(p string, ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = p + s
	}
	return out
}

// dynamicWords returns the words the case declares dynamic on this host.
func dynamicWords(e evidence) []string {
	if e.decl != nil && HostOS() != Windows {
		return e.decl.DynamicUnix
	}
	return nil
}

// matchCall returns the first unused call named name, preferring one whose static words agree with
// the engine's, or -1.
func matchCall(calls []Call, used []bool, name string, els []oElem) int {
	first := -1
	for j, c := range calls {
		if used[j] || !c.Args[0].Static || c.Args[0].Text != name {
			continue
		}
		if first < 0 {
			first = j
		}
		if f, _ := compareArgv(c, els, nil); len(f) == 0 {
			return j
		}
	}
	return first
}

// compareCall compares the command kind, the argv and, for a native call the stub ran, the argv
// the stub received.
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
	f, n := compareArgv(c, oc.Elements, dynamicWords(e))
	ob.fails = append(ob.fails, f...)
	ob.notes = append(ob.notes, n...)
	st.argvWords += len(c.Args) - 1

	// checkStubContract has checked that the stub ran exactly the eligible calls.
	got, ok := e.stub[id]
	if !ok {
		return
	}
	st.stubCalls++
	legacy := e.engine.NativeArgumentPassing == "" || e.engine.NativeArgumentPassing == "Legacy"
	f, n = compareStub(c, got, legacy, st)
	ob.fails = append(ob.fails, f...)
	ob.notes = append(ob.notes, n...)
}

// stubShaped reports whether the stub must receive these arguments: every one is a literal, a
// variable read, a string holding only variable reads, a parameter with such a value or a list of
// them (the allowlist of testdata/oracle.ps1, decided here from the AST types).
func stubShaped(els []oElem) bool {
	for _, e := range els {
		switch e.AST {
		case "StringConstantExpressionAst", "ConstantExpressionAst", "VariableExpressionAst":
		case "ExpandableStringExpressionAst":
			for _, p := range e.Parts {
				if p != "VariableExpressionAst" {
					return false
				}
			}
		case "CommandParameterAst":
			if e.Arg != nil && !stubShaped([]oElem{*e.Arg}) {
				return false
			}
		case "ArrayLiteralAst":
			if !stubShaped(e.Items) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// want is one argv word as the engine parses it.
type want struct {
	text          string
	static, block bool
	skip          bool   // not comparable (a native list)
	skipNote      string // when set, the rest of argv is not compared, for this reason
}

// expected turns the engine's command elements into argv words for a call of kind k, applying the
// argument-passing rules \u00a73 measured on both engines with the argv stub: the stop-parsing token's
// tail (split at whitespace for a native command, one literal argument after the kept token for a
// cmdlet), and a native quoted `--%`, which is dropped while later values are joined as written.
func expected(els []oElem, k Kind) []want {
	var out []want
	joined := false
	for i, e := range els {
		if joined {
			ws, ok := joinedWants(e)
			if !ok {
				return append(out, want{
					skipNote: "ambiguous region after a quoted --% not compared",
				})
			}
			out = append(out, ws...)
			continue
		}
		switch e.K {
		case "str":
			if e.Q == "BareWord" && e.V == "--%" {
				tail := ""
				if i+1 < len(els) {
					tail = els[i+1].V
				}
				return append(out, tailWants(tail, k)...)
			}
			if k == Native && e.V == "--%" {
				joined = true
				continue
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

// tailWants is the argv after a stop-parsing token with this tail: for a cmdlet the kept token and
// the trimmed tail as one argument, for a native command the tail split at whitespace (not compared
// when it holds a quote or `%`).
func tailWants(tail string, k Kind) []want {
	if k == Cmdlet {
		out := []want{{text: "--%", static: true}}
		if t := strings.TrimSpace(tail); t != "" {
			out = append(out, want{text: t, static: true})
		}
		return out
	}
	if strings.ContainsAny(tail, "\"“”„%") {
		return []want{
			{skipNote: "--% tail with quotes or %NAME% not compared"},
		}
	}
	var out []want
	for f := range strings.FieldsSeq(tail) {
		out = append(out, want{text: f, static: true})
	}
	return out
}

// joinedWants is the argv of an element after a quoted `--%`: a static value split at spaces, or
// false for one the engines pass differently (§3: a value holding `"`, `,`, a tab or a line
// break, a `%NAME%`, an empty value, a parameter token, an array, any computed element, and another
// `--%`), from which on nothing is compared.
func joinedWants(e oElem) ([]want, bool) {
	if e.K != "str" || e.V == "" || e.V == "--%" ||
		strings.ContainsAny(e.V, "\",\t\r\n") || percentVar.MatchString(e.V) {
		return nil, false
	}
	var out []want
	for f := range strings.SplitSeq(e.V, " ") {
		if f != "" {
			out = append(out, want{text: f, static: true})
		}
	}
	return out, true
}

// compareArgv compares the recognizer's words after the name with the engine's elements: a static
// word must equal the engine's static value, no word the engine computes may be static, and a word
// the engine reads as static may be dynamic only when the case declares its text in dynamic.
func compareArgv(c Call, els []oElem, dynamic []string) ([]string, []string) {
	var fails, notes []string
	ws := expected(els[1:], c.Kind)
	got := c.Args[1:]
	if n := len(ws); n > 0 && ws[n-1].skipNote != "" {
		notes = append(notes, ws[n-1].skipNote)
		ws = ws[:n-1]
		if len(got) > len(ws) {
			got = got[:len(ws)]
		}
	}
	// The recognizer's ambiguous region keeps the static text of what the engines may pass
	// differently (both parsers even drop a parameter token after a quoted `--%`), so it ends the
	// comparison; TestAmbiguousRegion checks where it starts.
	if k := slices.IndexFunc(got, func(w Word) bool { return w.ambiguous }); k >= 0 {
		notes = append(notes, "ambiguous region after a quoted --% not compared")
		got = got[:k]
		ws = ws[:min(k, len(ws))]
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
			msg := fmt.Sprintf("word %d %q dynamic, engine static %q", i+1, g.Raw, w.text)
			if slices.Contains(dynamic, w.text) {
				notes = append(notes, "declared: "+msg)
			} else {
				fails = append(fails, msg)
			}
		case g.Flag+g.Text != w.text:
			fails = append(fails,
				fmt.Sprintf("word %d = %q, engine %q", i+1, g.Flag+g.Text, w.text))
		}
	}
	return fails, notes
}

// compareStub compares ShellCall's static words with the argv the stub received. A word ShellCall
// marks as a home `~` (Tilde) is not compared, since the host's expansion depends on the engine
// version (powershell-guard §3 Words); a literal `~user` or `~foo/.git` is. Under legacy argument
// passing (Windows PowerShell 5.1, or `$PSNativeCommandArgumentPassing = 'Legacy'`) a word holding
// `"` reaches the program split or unquoted: documented in phase2-core §6, so such a call is
// noted, not compared. An ambiguous word (§3, after a quoted `--%`) is engine-dependent by design
// and compared like a dynamic one: not at all.
func compareStub(c Call, got []string, legacy bool, st *stats) ([]string, []string) {
	var fails, notes []string
	words := c.ShellCall().Args[1:]
	if legacy && slices.ContainsFunc(words,
		func(w shell.Word) bool { return strings.Contains(w.Text, `"`) }) {
		return nil, []string{fmt.Sprintf("documented: legacy argument passing, stub argv %q", got)}
	}
	compared := func(w shell.Word) bool { return w.Static && !w.Ambiguous }
	allStatic := true
	for _, w := range words {
		allStatic = allStatic && compared(w)
	}
	n := len(words)
	if n != len(got) {
		if allStatic {
			return []string{fmt.Sprintf("stub argv %q, ShellCall %q", got, shellTexts(words))}, nil
		}
		notes = append(notes,
			fmt.Sprintf("stub argv has %d words, ShellCall %d with dynamic words", len(got), n))
		n = 0
		for n < len(words) && n < len(got) && compared(words[n]) {
			n++
		}
	}
	for i := range n {
		w := words[i]
		if !compared(w) || w.Tilde {
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

// sourceCorpus returns the §7.2 commands: the controls, C1 from this package's tables and C2–C5
// from the command lists of the guard tests.
func sourceCorpus(t *testing.T) []string {
	t.Helper()
	texts := make(
		[]string,
		0,
		len(
			controls,
		)+len(
			parseTests,
		)+len(
			kindTests,
		)+len(
			bindingTests,
		)+len(
			nestedTests,
		)+len(
			parseFailures,
		),
	)
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
	for i, s := range texts {
		if seen[s] || strings.TrimSpace(s) == "" || (i >= local && json.Valid([]byte(s))) {
			continue
		}
		seen[s] = true
		out = append(out, s)
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
		"1..3 | ForEach-Object { gh a } { gh b }",
		func(_ *oracleOut, oc *oCase, _ map[string][]string) string {
			ctx := make([]string, 0, len(oc.Commands))
			for _, c := range oc.Commands {
				ctx = append(ctx, c.Ctx)
			}
			// ForEach-Object, the first positional block (Process) and the RemainingScripts block.
			want := []string{"plain", "plain", "opaque:block-argument:ForEach-Object"}
			if len(oc.Errors) > 0 || !slices.Equal(ctx, want) {
				return fmt.Sprintf("positions %q, want %q", ctx, want)
			}
			return ""
		},
	},
	{"iex 'gh pr merge 7'", func(_ *oracleOut, oc *oCase, _ map[string][]string) string {
		if len(oc.Errors) > 0 || len(oc.Nested) != 1 || oc.Nested[0].Name == nil ||
			*oc.Nested[0].Name != "gh" ||
			len(oc.Nested[0].Elements) != 4 {
			return fmt.Sprintf("nested %+v, want gh pr merge 7", oc.Nested)
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

// Field types of the oracle output: s string, n number, b bool, a array, o object; a trailing ?
// allows null. Every listed field is required unless it starts with ~.
var schema = map[string]map[string]string{
	"": {
		"version": "s", "edition": "s", "languageMode": "s", "nativeArgumentPassing": "s",
		"home": "s", "modules": "a", "cases": "a", "resolved": "o", "aliasesOf": "o",
		"params": "o", "pBinding": "s", "stubRuns": "n", "stubSame": "o",
	},
	"case":    {"id": "n", "errors": "a", "commands": "a", "nested": "a"},
	"command": {"name": "s?", "ctx": "s", "end": "n", "elements": "a", "eligible": "b"},
	"nested":  {"name": "s?", "ctx": "s", "elements": "a"},
	"element": {
		"k":       "s",
		"t":       "s",
		"ast":     "s",
		"~v":      "s",
		"~q":      "s",
		"~prefix": "s",
		"~arg":    "o",
		"~items":  "a",
		"~nested": "a",
	},
	"error":    {"id": "s", "offset": "n", "text": "s"},
	"param":    {"name": "s", "aliases": "a", "switch": "b"},
	"stubLine": {"id": "s", "argv": "a"},
}

func typeOf(v any) string {
	switch v.(type) {
	case string:
		return "s"
	case float64:
		return "n"
	case bool:
		return "b"
	case []any:
		return "a"
	case map[string]any:
		return "o"
	}
	return "null"
}

// checkKeys returns an error when obj lacks a required field of kind or holds one of the wrong type.
func checkKeys(obj any, kind, where string) error {
	m, ok := obj.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: not an object", where)
	}
	for k, typ := range schema[kind] {
		optional := strings.HasPrefix(k, "~")
		k = strings.TrimPrefix(k, "~")
		v, ok := m[k]
		switch got := typeOf(v); {
		case !ok && optional:
		case !ok:
			return fmt.Errorf("%s: no %q", where, k)
		case got == "null" && strings.HasSuffix(typ, "?"):
		case got != strings.TrimSuffix(typ, "?"):
			return fmt.Errorf("%s: %q is %s, want %s", where, k, got, typ)
		}
	}
	return nil
}

// checkStrings returns an error when v is not an array of strings.
func checkStrings(v any, where string) error {
	for _, x := range listOf(v) {
		if _, ok := x.(string); !ok {
			return fmt.Errorf("%s: %v is not a string", where, x)
		}
	}
	return nil
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

var astTypeRe = regexp.MustCompile(`^[A-Z][A-Za-z]*Ast$`)

// checkElements checks command elements, nested ones included.
func checkElements(v any, where string) error {
	for _, el := range listOf(v) {
		if err := checkKeys(el, "element", where); err != nil {
			return err
		}
		m, _ := el.(map[string]any)
		types := append([]any{m["ast"]}, listOf(m["nested"])...)
		for _, t := range types {
			if s, _ := t.(string); !astTypeRe.MatchString(s) {
				return fmt.Errorf("%s: AST type %q", where, s)
			}
		}
		if a, ok := m["arg"]; ok {
			if err := checkElements([]any{a}, where); err != nil {
				return err
			}
		}
		if err := checkElements(m["items"], where); err != nil {
			return err
		}
		if err := checkStrings(m["nested"], where); err != nil {
			return err
		}
	}
	return nil
}

// validateOutput checks that the raw oracle output carries every required field with its type,
// an alias list for every cmdlet and a nonempty parameter list for every table requested.
func validateOutput(b []byte, cmdlets, tables []string) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("oracle output: %w", err)
	}
	if err := checkKeys(raw, "", "output"); err != nil {
		return err
	}
	if err := checkStrings(raw["modules"], "modules"); err != nil {
		return err
	}
	for i, c := range listOf(raw["cases"]) {
		if err := checkKeys(c, "case", fmt.Sprintf("case %d", i)); err != nil {
			return err
		}
		cm, _ := c.(map[string]any)
		for _, e := range listOf(cm["errors"]) {
			if err := checkKeys(e, "error", fmt.Sprintf("case %d error", i)); err != nil {
				return err
			}
		}
		for j, cmd := range listOf(cm["commands"]) {
			where := fmt.Sprintf("case %d command %d", i, j)
			if err := checkKeys(cmd, "command", where); err != nil {
				return err
			}
			m, _ := cmd.(map[string]any)
			if err := checkElements(m["elements"], where); err != nil {
				return err
			}
		}
		for j, n := range listOf(cm["nested"]) {
			where := fmt.Sprintf("case %d nested %d", i, j)
			if err := checkKeys(n, "nested", where); err != nil {
				return err
			}
			m, _ := n.(map[string]any)
			if err := checkElements(m["elements"], where); err != nil {
				return err
			}
		}
	}
	for _, field := range []string{"resolved", "stubSame"} {
		m, _ := raw[field].(map[string]any)
		for k, v := range m {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s[%q] is %s, want s", field, k, typeOf(v))
			}
		}
	}
	aliasesOf, _ := raw["aliasesOf"].(map[string]any)
	for _, c := range cmdlets {
		if typeOf(aliasesOf[c]) != "a" {
			return fmt.Errorf("aliasesOf[%q] is %s, want a", c, typeOf(aliasesOf[c]))
		}
	}
	for k, v := range aliasesOf {
		if err := checkStrings(v, "aliasesOf "+k); err != nil {
			return err
		}
	}
	params, _ := raw["params"].(map[string]any)
	for _, tb := range tables {
		if len(listOf(params[tb])) == 0 {
			return fmt.Errorf("params[%q] is %v, want a nonempty list", tb, params[tb])
		}
	}
	for name, ps := range params {
		for _, p := range listOf(ps) {
			if err := checkKeys(p, "param", "params of "+name); err != nil {
				return err
			}
		}
	}
	return nil
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
	cmdlets, tableNames := slices.Sorted(maps.Values(canonical)), slices.Sorted(maps.Keys(tables))
	req := map[string]any{
		"cases":   cases,
		"names":   slices.Sorted(maps.Keys(aliases)),
		"cmdlets": cmdlets,
		"tables":  tableNames,
		"stub":    stub,
		"marker":  stubMarker,
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
	if err := validateOutput(b, cmdlets, tableNames); err != nil {
		return nil, nil, err
	}
	var o oracleOut
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, nil, fmt.Errorf("oracle output: %w", err)
	}
	stubs, err := readStubs(argvFile, &o)
	if err != nil {
		return nil, nil, err
	}
	return &o, stubs, checkEngine(&o, corpus, stubs)
}

// readStubs reads the stub's records and maps every deduplicated call to the record of the run.
func readStubs(file string, o *oracleOut) (map[string][]string, error) {
	stubs := map[string][]string{}
	f, err := os.Open(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stub records: %w", err)
	}
	if err == nil {
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			r, err := parseStubLine(sc.Bytes())
			if err != nil {
				return nil, err
			}
			stubs[r.ID] = r.Argv
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("stub records: %w", err)
		}
	}
	if len(stubs) != o.StubRuns {
		return nil, fmt.Errorf("%d stub records for %d runs", len(stubs), o.StubRuns)
	}
	for id, ran := range o.StubSame {
		argv, ok := stubs[ran]
		if !ok {
			return nil, fmt.Errorf("stub call %s refers to %s, which has no record", id, ran)
		}
		stubs[id] = argv
	}
	return stubs, nil
}

// parseStubLine decodes one stub record, checking its fields and their types.
func parseStubLine(b []byte) (stubRecord, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return stubRecord{}, fmt.Errorf("stub record: %w", err)
	}
	if err := checkKeys(raw, "stubLine", "stub record"); err != nil {
		return stubRecord{}, err
	}
	if err := checkStrings(raw["argv"], "stub record"); err != nil {
		return stubRecord{}, err
	}
	var r stubRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return stubRecord{}, fmt.Errorf("stub record: %w", err)
	}
	return r, nil
}

// oracleModules are the modules the oracle imports; autoloading is off, so no other module may
// be loaded after it ran.
var oracleModules = []string{"Microsoft.PowerShell.Management", "Microsoft.PowerShell.Utility"}

// checkEngine returns why the engine's output is not complete evidence: a language mode other
// than FullLanguage, an unknown version, a module loaded by a lookup, a missing or repeated case,
// no stub run, or a failed control.
func checkEngine(o *oracleOut, corpus []string, stubs map[string][]string) error {
	if o.LanguageMode != "FullLanguage" {
		return fmt.Errorf("LanguageMode %q", o.LanguageMode)
	}
	if o.key() == "" {
		return fmt.Errorf("engine version %q", o.Version)
	}
	for _, m := range o.Modules {
		if !slices.Contains(oracleModules, m) {
			return fmt.Errorf("module %s was loaded", m)
		}
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
	if err := checkStubContract(o, stubs); err != nil {
		return err
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

// checkStubContract checks that the stub ran exactly the plain native calls whose arguments it may
// receive (decided here from the AST record), in every script the engine parses.
func checkStubContract(o *oracleOut, stubs map[string][]string) error {
	want := map[string]bool{}
	for _, c := range o.Cases {
		for i, cmd := range c.Commands {
			id := fmt.Sprintf("%d/%d", c.ID, i)
			eligible := len(c.Errors) == 0 && cmd.Ctx == "plain" && cmd.Name != nil &&
				o.resolve(*cmd.Name) == "native" && stubShaped(cmd.Elements[1:])
			if cmd.Eligible != eligible {
				return fmt.Errorf(
					"call %s: eligible = %v, its record says %v",
					id,
					cmd.Eligible,
					eligible,
				)
			}
			if _, ok := stubs[id]; ok != eligible {
				return fmt.Errorf("call %s: stub record = %v, eligible = %v", id, ok, eligible)
			}
			want[id] = eligible
		}
	}
	for id := range stubs {
		if !want[id] {
			return fmt.Errorf("stub record %s for no eligible call", id)
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
	if *updateOracle && errors.Is(err, os.ErrNotExist) {
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

// TestOracle is C11 of powershell-guard §7.2: the recognizer against the installed PowerShell
// engines. An engine that is missing, fails, times out or yields incomplete evidence makes the
// test fail as inconclusive.
func TestOracle(t *testing.T) {
	if os.Getenv(oracleEnv) != "1" {
		t.Skip(oracleEnv + "=1 runs the PowerShell oracle (CI job powershell-oracle)")
	}
	decls := readDeclared(t)
	byCmd := map[string]*declared{}
	for i := range decls {
		byCmd[decls[i].Cmd] = &decls[i]
	}
	source := sourceCorpus(t)
	corpus := source
	if !*updateOracle {
		corpus = make([]string, len(decls))
		for i := range decls {
			corpus[i] = decls[i].Cmd
		}
		for _, s := range source {
			if byCmd[s] == nil {
				t.Errorf("command %q is not declared: rerun with -update-oracle on Windows", s)
			}
		}
		for _, d := range decls {
			if !slices.Contains(source, d.Cmd) {
				t.Errorf("declared command %q is no longer in the sources", d.Cmd)
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
			obs[i] = judge(t, en.key, res.o, res.stubs, corpus, byCmd)
		})
	}
	if *updateOracle {
		writeDeclared(t, ens, corpus, decls, obs)
	}
}

// judge logs the engine, judges every case and checks the host's aliases and binding tables.
func judge(t *testing.T, key string, o *oracleOut, stubs map[string][]string, corpus []string,
	byCmd map[string]*declared,
) []observation {
	t.Helper()
	t.Logf("engine %s %s on %s, LanguageMode %s, PSNativeCommandArgumentPassing %q, "+
		"corpus %d, stub runs %d", o.Version, o.Edition, runtime.GOOS, o.LanguageMode,
		o.NativeArgumentPassing, len(corpus), o.StubRuns)
	var st stats
	counts := map[Verdict]int{}
	obs := make([]observation, len(corpus))
	for id, text := range corpus {
		e := evidence{
			text: text, decl: byCmd[text], engine: o, oc: &o.Cases[id],
			r:    Parse(text, "", o.Home, HostOS()),
			stub: stubs,
		}
		if *updateOracle && byCmd[text] == nil {
			obs[id] = observe(e, &stats{})
			continue
		}
		obs[id] = observe(e, &st)
		v, notes := verdict(e, obs[id], &st)
		counts[v]++
		switch v {
		case Pass:
			for _, n := range notes {
				if strings.HasPrefix(n, "documented") || strings.HasPrefix(n, "declared") {
					t.Logf("case %d %q: %s", id, text, n)
				}
			}
		case Fail, Inconclusive:
			if *updateOracle {
				t.Logf("case %d %q: %s: %s", id, text, v, strings.Join(notes, "; "))
			} else {
				t.Errorf("case %d %q: %s: %s", id, text, v, strings.Join(notes, "; "))
			}
		}
	}
	t.Logf("verdicts on %s: %d pass, %d fail, %d inconclusive; compared %d calls, "+
		"%d argv words, %d stub calls with %d static words; %d notes",
		key, counts[Pass], counts[Fail], counts[Inconclusive], st.calls,
		st.argvWords, st.stubCalls, st.stubWords, st.notes)
	if counts[Pass] == 0 && !*updateOracle {
		t.Errorf("no case passed on %s", key)
	}
	checkHost(t, o)
	return obs
}

// writeDeclared writes the declarations from the engines' observations of a Windows host: the
// names come from the engines' ASTs, never from Parse. A changed declaration or a removed command
// is written only with -update-oracle-changes; the differences are printed either way.
func writeDeclared(t *testing.T, ens []struct{ key, exe string }, corpus []string,
	old []declared, obs [][]observation,
) {
	t.Helper()
	if len(ens) != 2 || slices.ContainsFunc(obs, func(o []observation) bool { return o == nil }) {
		t.Fatal("-update-oracle needs a run of both engines on Windows")
	}
	prev := map[string]declared{}
	for _, d := range old {
		prev[d.Cmd] = d
	}
	out := make([]declared, len(corpus))
	var changes []string
	for id, text := range corpus {
		var d declared
		for i, en := range ens {
			ob := obs[i][id]
			if len(ob.missing) > 0 {
				t.Fatalf("case %q on %s: %q", text, en.key, ob.missing)
			}
			if ob.rejects {
				d.Rejects = append(d.Rejects, en.key)
			}
			// The names of 7's AST; of a script it rejects, only a documented one's recovered AST
			// is compared.
			if en.key == "7" {
				r := d.Rejects
				d = declared{Cmd: text, Calls: []string{}}
				if !ob.rejects || documentedParseErrors[text] != "" {
					d = ob.declaredBy(text)
				}
				d.Rejects = r
			}
		}
		p, had := prev[text]
		d.DynamicUnix = p.DynamicUnix
		if had && !sameDeclared(p, d) {
			changes = append(
				changes,
				fmt.Sprintf("changed %s\n   to %s", mustJSON(t, p), mustJSON(t, d)),
			)
		}
		out[id] = d
	}
	for _, p := range old {
		if !slices.Contains(corpus, p.Cmd) {
			changes = append(changes, "removed "+mustJSON(t, p))
		}
	}
	for _, c := range changes {
		t.Log(c)
	}
	if len(changes) > 0 && !*updateChanges {
		t.Fatalf("%d declarations would change or go: review them and rerun with "+
			"-update-oracle-changes", len(changes))
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

func sameDeclared(a, b declared) bool {
	return a.Cmd == b.Cmd && slices.Equal(a.Calls, b.Calls) && slices.Equal(a.Skipped, b.Skipped) &&
		slices.Equal(a.Nested, b.Nested) && slices.Equal(a.Rejects, b.Rejects)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
