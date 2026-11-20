// Package ghargs models how gh parses the argv of the commands the guard inspects: the command path
// with its built-in aliases, the persistent -R/--repo and --help flags, and each command's flags as
// declared in gh GhVersion, parsed with spf13/pflag (gh's own parser, via cobra). It reports the
// published inputs of a call (§4.3 of docs/design/phase2-core.md) and what the raw-merge rule needs.
package ghargs

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/spf13/pflag"
)

// Arg is one argv word.
type Arg struct {
	Text   string
	Static bool
	// Tilde reports that a leading `~` is unquoted, so the shell expands it.
	Tilde bool
}

// Kind is where a published input comes from.
type Kind int

// Input kinds.
const (
	// Text is a value published as written.
	Text Kind = iota
	// File is a file whose content is published; Path names it.
	File
	// Stdin is the call's stdin.
	Stdin
)

// Input is one published input.
type Input struct {
	Flag string // e.g. "--body"
	Kind Kind
	Text string // Text
	Path string // File, as written
	// Expand reports that Path's leading `~` was unquoted, so the shell expanded it.
	Expand bool
	// JSON marks an `api --input` body: a JSON object whose "query" is a GraphQL document.
	JSON bool
}

// Call is a parsed gh call.
type Call struct {
	// Command is the canonical command path, e.g. "pr create".
	Command string
	// Published lists the published inputs the parse collected.
	Published []Input
	// Literal lists, after a parse error, the words from the failing one on. They are not parsed
	// and are scanned as text.
	Literal []string
	// Err is the parse error, if any.
	Err error

	// Help reports that -h/--help parsed as true.
	Help bool
	// DisableAuto reports that --disable-auto parsed as true (pr merge).
	DisableAuto bool
	// DryRun reports that --dry-run parsed as true (pr create): gh prints the pull request and
	// returns before creating it.
	DryRun bool
	// Method is the effective HTTP method of `gh api`, upper-cased.
	Method string
	// Endpoint is the `gh api` endpoint as gh resolves it: without scheme, host, leading `/` and
	// query string.
	Endpoint string
	// Queries lists the GraphQL query sources of `gh api graphql`.
	Queries []Input
}

const (
	// dynamic stands for a word whose value is not static. NUL cannot occur in an argv word.
	dynamic = "\x00"
	// noExpand prefixes a value whose leading `~` the shell does not expand.
	noExpand = "\x00"
)

var groups = map[string]bool{"pr": true, "issue": true, "release": true}

// Parse parses the argv of a gh call after argv0. ok is false when the command is not one the table
// models.
func Parse(args []Arg) (Call, bool) {
	tokens := make([]string, len(args))
	for i, a := range args {
		tokens[i] = mark(a)
	}
	command, rest, ok := commandPath(tokens)
	if !ok {
		return Call{}, false
	}
	c := Call{Command: command}
	fs := flagSet(command, false)
	c.Err = fs.Parse(rest)
	if c.Err != nil {
		c.Literal = literal(rest[errorIndex(command, rest):])
	}
	c.Help = boolValue(fs, "help")
	switch command {
	case "pr merge":
		c.DisableAuto = boolValue(fs, "disable-auto")
	case "pr create":
		c.DryRun = boolValue(fs, "dry-run")
		c.Published = commandPublished(command, fs)
	case "api":
		c.Method, c.Endpoint = apiTarget(fs)
		if c.Method != http.MethodGet {
			c.Published = apiPublished(fs)
		}
		if c.Err != nil {
			// The merge rule judges what a newer gh would see: skip unknown flags and read on.
			all := flagSet(command, true)
			_ = all.Parse(rest)
			fs = all
			c.Method, c.Endpoint = apiTarget(fs)
		}
		if c.Endpoint == "graphql" {
			c.Queries = graphqlQueries(fs)
		}
	default:
		c.Published = commandPublished(command, fs)
	}
	return c, true
}

// mark turns an argv word into the token passed to pflag, keeping the shell's tilde decision: a
// value whose leading `~` the shell does not expand gets the noExpand prefix.
func mark(a Arg) string {
	if !a.Static {
		return dynamic
	}
	t := a.Text
	if strings.HasPrefix(t, "~") && !a.Tilde {
		return noExpand + t
	}
	if strings.HasPrefix(t, "-") {
		if i := strings.IndexByte(t, '='); i > 0 && strings.HasPrefix(t[i+1:], "~") {
			return t[:i+1] + noExpand + t[i+1:]
		}
	}
	return t
}

// pathValue undoes mark for a value naming a file.
func pathValue(v string) (string, bool) {
	if after, ok := strings.CutPrefix(v, noExpand); ok {
		return after, false
	}
	return v, strings.HasPrefix(v, "~")
}

// textValue undoes mark for a published text; ok is false for a dynamic word.
func textValue(v string) (string, bool) {
	if v == dynamic {
		return "", false
	}
	return strings.ReplaceAll(v, "\x00", ""), true
}

func literal(tokens []string) []string {
	var out []string
	for _, t := range tokens {
		if s, ok := textValue(t); ok {
			out = append(out, s)
		}
	}
	return out
}

// commandPath finds the command the way cobra does: the first words that are not flags name it,
// skipping the value of -R/--repo, and every other token goes to the flag parser.
func commandPath(tokens []string) (string, []string, bool) {
	var path []string
	var rest []string
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		want := len(path) == 0 || (len(path) == 1 && groups[path[0]])
		switch {
		case !want:
			rest = append(rest, t)
		case t == "--":
			return "", nil, false
		case t == "-R" || t == "--repo":
			rest = append(rest, t)
			if i+1 < len(tokens) {
				i++
				rest = append(rest, tokens[i])
			}
		case strings.HasPrefix(t, "-") && t != "-":
			rest = append(rest, t)
		default:
			path = append(path, t)
		}
	}
	command := strings.Join(path, " ")
	if a, ok := aliases[command]; ok {
		command = a
	}
	if _, ok := table[command]; !ok {
		return "", nil, false
	}
	return command, rest, true
}

// flagSet builds the pflag set of command; unknownOK skips unknown flags instead of failing.
func flagSet(command string, unknownOK bool) *pflag.FlagSet {
	fs := pflag.NewFlagSet(command, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if unknownOK {
		fs.ParseErrorsAllowlist.UnknownFlagsHandling = pflag.PassUnknownFlagToArgs
	}
	fs.BoolP("help", "h", false, "")
	if command != "api" {
		fs.StringP("repo", "R", "", "")
	}
	for _, f := range table[command] {
		switch f.kind {
		case kBool:
			fs.BoolP(f.name, f.short, false, "")
		case kString:
			fs.StringP(f.name, f.short, "", "")
		case kStringArray:
			fs.StringArrayP(f.name, f.short, nil, "")
		case kStringSlice:
			fs.StringSliceP(f.name, f.short, nil, "")
		case kDuration:
			fs.DurationP(f.name, f.short, 0, "")
		}
	}
	return fs
}

// errorIndex returns the index of the token at which parsing rest fails: the shortest prefix that
// fails with an error other than a value missing at its end.
func errorIndex(command string, rest []string) int {
	for k := 1; k <= len(rest); k++ {
		err := flagSet(command, false).Parse(rest[:k])
		if err == nil {
			continue
		}
		if _, missing := errors.AsType[*pflag.ValueRequiredError](err); missing && k < len(rest) {
			continue
		}
		return k - 1
	}
	return len(rest)
}

func boolValue(fs *pflag.FlagSet, name string) bool {
	v, err := fs.GetBool(name)
	return err == nil && v
}

func stringValue(fs *pflag.FlagSet, name string) (string, bool) {
	if !fs.Changed(name) {
		return "", false
	}
	v, err := fs.GetString(name)
	return v, err == nil
}

func arrayValue(fs *pflag.FlagSet, name string) []string {
	if !fs.Changed(name) {
		return nil
	}
	v, _ := fs.GetStringArray(name)
	return v
}

// fileInput is the input a body-file style flag names: `-` is the call's stdin.
func fileInput(flag, v string) (Input, bool) {
	if v == dynamic {
		return Input{}, false
	}
	if v == "-" {
		return Input{Flag: flag, Kind: Stdin}, true
	}
	p, expand := pathValue(v)
	return Input{Flag: flag, Kind: File, Path: p, Expand: expand}, true
}

func commandPublished(command string, fs *pflag.FlagSet) []Input {
	spec, ok := published[command]
	if !ok {
		return nil
	}
	var in []Input
	for _, name := range spec.text {
		if v, ok := stringValue(fs, name); ok {
			if s, ok := textValue(v); ok {
				in = append(in, Input{Flag: "--" + name, Kind: Text, Text: s})
			}
		}
	}
	if v, ok := stringValue(fs, spec.file); ok {
		if fi, ok := fileInput("--"+spec.file, v); ok {
			in = append(in, fi)
		}
	}
	if spec.attachOK {
		// gh splits '<file>#<alt>' at the `#` after the longest existing path; the text after the
		// first `#` covers every split.
		for _, v := range arrayValue(fs, "attach") {
			if i := strings.IndexByte(v, '#'); i > 0 {
				if s, ok := textValue(v[i+1:]); ok {
					in = append(in, Input{Flag: "--attach", Kind: Text, Text: s})
				}
			}
		}
	}
	return in
}

// apiTarget returns the effective method and the normalised endpoint of `gh api`.
func apiTarget(fs *pflag.FlagSet) (string, string) {
	method := http.MethodGet
	if v, ok := stringValue(fs, "method"); ok {
		method = strings.ToUpper(strings.ReplaceAll(v, "\x00", ""))
	} else if fs.Changed("field") || fs.Changed("raw-field") || fs.Changed("input") {
		method = http.MethodPost
	}
	// After an unknown flag the flag and its value may sit among the positionals: prefer one that
	// names a guarded endpoint.
	var endpoint string
	for _, a := range fs.Args() {
		if a == dynamic || strings.HasPrefix(a, "-") {
			continue
		}
		e := normalizeEndpoint(strings.ReplaceAll(a, "\x00", ""))
		if endpoint == "" || e == "graphql" || IsPullMerge(e) {
			endpoint = e
		}
		if e == "graphql" || IsPullMerge(e) {
			break
		}
	}
	return method, endpoint
}

// normalizeEndpoint strips what gh does not treat as part of the endpoint path: scheme and host of
// an absolute URL (and the `api/v3/` prefix of an Enterprise host), the leading `/`, and the query.
func normalizeEndpoint(p string) string {
	if _, after, ok := strings.Cut(p, "://"); ok {
		_, p, _ = strings.Cut(after, "/")
		p = strings.TrimPrefix(p, "api/v3/")
		if p == "api/graphql" {
			p = "graphql"
		}
	}
	p = strings.TrimLeft(p, "/")
	p, _, _ = strings.Cut(p, "?")
	return p
}

// apiPublished returns the field values and the --input body of a non-GET `gh api` call.
func apiPublished(fs *pflag.FlagSet) []Input {
	var in []Input
	for _, v := range arrayValue(fs, "raw-field") {
		if s, ok := textValue(v); ok {
			in = append(in, Input{Flag: "--raw-field", Kind: Text, Text: s})
		}
	}
	for _, v := range arrayValue(fs, "field") {
		key, val, _ := strings.Cut(v, "=")
		if after, ok := strings.CutPrefix(val, "@"); ok {
			if s, ok := textValue(key); ok {
				in = append(in, Input{Flag: "--field", Kind: Text, Text: s})
			}
			if fi, ok := fileInput("--field", after); ok {
				fi.Expand = false // the shell does not expand `~` after `@`
				in = append(in, fi)
			}
			continue
		}
		if s, ok := textValue(v); ok {
			in = append(in, Input{Flag: "--field", Kind: Text, Text: s})
		}
	}
	if v, ok := stringValue(fs, "input"); ok {
		if fi, ok := fileInput("--input", v); ok {
			in = append(in, fi)
		}
	}
	return in
}

// graphqlQueries returns the `query` sources of `gh api graphql`: -f/-F query=…, -F query=@F, and
// the "query" of an --input JSON body.
func graphqlQueries(fs *pflag.FlagSet) []Input {
	var in []Input
	for _, name := range []string{"raw-field", "field"} {
		for _, v := range arrayValue(fs, name) {
			key, val, ok := strings.Cut(v, "=")
			if !ok || strings.ReplaceAll(key, "\x00", "") != "query" {
				continue
			}
			if after, ok := strings.CutPrefix(val, "@"); ok && name == "field" {
				if fi, ok := fileInput("--field", after); ok {
					fi.Expand = false
					in = append(in, fi)
				}
				continue
			}
			if s, ok := textValue(val); ok {
				in = append(in, Input{Flag: "--" + name, Kind: Text, Text: s})
			}
		}
	}
	if v, ok := stringValue(fs, "input"); ok {
		if fi, ok := fileInput("--input", v); ok {
			fi.JSON = true
			in = append(in, fi)
		}
	}
	return in
}

// QueryFromJSON returns the "query" string of an `api --input` JSON body.
func QueryFromJSON(data []byte) (string, bool) {
	var body struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Query == "" {
		return "", false
	}
	return body.Query, true
}

// IsPullMerge reports whether a normalised REST endpoint is repos/<o>/<r>/pulls/<n>/merge.
func IsPullMerge(endpoint string) bool {
	parts := strings.Split(strings.TrimRight(endpoint, "/"), "/")
	return len(parts) == 6 && strings.EqualFold(parts[0], "repos") &&
		strings.EqualFold(parts[3], "pulls") && strings.EqualFold(parts[5], "merge") &&
		!slices.Contains(parts[1:5], "")
}
