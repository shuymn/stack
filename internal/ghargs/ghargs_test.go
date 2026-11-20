package ghargs

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// args builds static, unquoted argv words from a space-separated line; "$" is a dynamic word.
func args(line string) []Arg {
	var out []Arg
	for f := range strings.FieldsSeq(line) {
		if f == "$" {
			out = append(out, Arg{})
			continue
		}
		out = append(out, Arg{Text: f, Static: true, Tilde: strings.HasPrefix(f, "~")})
	}
	return out
}

func parse(t *testing.T, line string) Call {
	t.Helper()
	c, ok := Parse(args(line))
	if !ok {
		t.Fatalf("%q: not a modelled command", line)
	}
	return c
}

func TestCommandPath(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"pr create", "pr create"},
		{"pr new", "pr create"},
		{"issue new", "issue create"},
		{"release new", "release create"},
		{"-R o/r pr merge 1", "pr merge"},
		{"pr -R o/r merge 1", "pr merge"},
		{"pr merge -R o/r 1", "pr merge"},
		{"--repo=o/r pr merge 1", "pr merge"},
		{"api repos/o/r", "api"},
	}
	for _, tt := range tests {
		if c := parse(t, tt.line); c.Command != tt.want {
			t.Errorf("%q command = %q, want %q", tt.line, c.Command, tt.want)
		}
	}
	for _, line := range []string{"pr view 1", "repo clone x", "-- pr merge", "$ merge", "auth status"} {
		if _, ok := Parse(args(line)); ok {
			t.Errorf("%q parsed as a modelled command", line)
		}
	}
}

func TestMergeFlags(t *testing.T) {
	tests := []struct {
		line              string
		help, disableAuto bool
	}{
		{"pr merge 1", false, false},
		{"pr merge --help", true, false},
		{"pr merge -h", true, false},
		{"pr merge -dh", true, false},
		{"pr merge -h=false", false, false},
		{"pr merge --help=false", false, false},
		{"pr merge --subject --help", false, false},
		{"pr merge -t -h 1", false, false},
		{"pr merge 1 -- --help", false, false},
		{"pr merge --disable-auto", false, true},
		{"pr merge --disable-auto=true", false, true},
		{"pr merge --disable-auto=false", false, false},
		{"pr merge --body=--disable-auto", false, false},
	}
	for _, tt := range tests {
		c := parse(t, tt.line)
		if c.Help != tt.help || c.DisableAuto != tt.disableAuto {
			t.Errorf("%q help=%v disableAuto=%v, want %v %v", tt.line, c.Help, c.DisableAuto,
				tt.help, tt.disableAuto)
		}
	}
}

func inputs(c Call) []string {
	var out []string
	for _, in := range c.Published {
		switch in.Kind {
		case Text:
			out = append(out, in.Flag+"="+in.Text)
		case File:
			out = append(out, in.Flag+"@"+in.Path)
		case Stdin:
			out = append(out, in.Flag+"@-")
		}
	}
	return out
}

func TestPublished(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{"pr create -t T -b B", []string{"--title=T", "--body=B"}},
		{"pr create -dtT -bB", []string{"--title=T", "--body=B"}},
		{"pr create -b=x", []string{"--body=x"}},
		{"pr create -b -x", []string{"--body=-x"}},
		{"pr create --body-file f.md", []string{"--body-file@f.md"}},
		{"pr create -F -", []string{"--body-file@-"}},
		{"pr new -F ~/b.md", []string{"--body-file@~/b.md"}},
		{"pr create -H head -B base -r x -T tpl", nil},
		{"pr create --attach ./s.png#alt --attach ./t.png", []string{"--attach=alt"}},
		{"pr edit 1 -b B --add-label x", []string{"--body=B"}},
		{"pr comment 1 -b B", []string{"--body=B"}},
		{"pr review 1 -c -b B", []string{"--body=B"}},
		{"issue create -t T -F f", []string{"--title=T", "--body-file@f"}},
		{"issue comment 1 --body-file=f", []string{"--body-file@f"}},
		{"release create v1 ./dist/a.tgz -n N -t T", []string{"--title=T", "--notes=N"}},
		{"release new v1 -F notes.md", []string{"--notes-file@notes.md"}},
		{"release edit v1 --notes N", []string{"--notes=N"}},
		{"pr create -b $", nil},
		{"pr merge 1 --body B", nil},
		{"pr create -- -b x", nil},
	}
	for _, tt := range tests {
		c := parse(t, tt.line)
		if c.Err != nil {
			t.Errorf("%q: %v", tt.line, c.Err)
		}
		if got := inputs(c); !slices.Equal(got, tt.want) {
			t.Errorf("%q published = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestQuotedTildeIsNotExpanded(t *testing.T) {
	c, _ := Parse([]Arg{
		{Text: "pr", Static: true},
		{Text: "create", Static: true},
		{Text: "-F", Static: true},
		{Text: "~/b.md", Static: true},
		{Text: "--body", Static: true},
		{Text: "~/x", Static: true},
	})
	if f := c.Published[1]; f.Path != "~/b.md" || f.Expand {
		t.Errorf("quoted ~ body file = %+v, want unexpanded", f)
	}
	if b := c.Published[0]; b.Text != "~/x" {
		t.Errorf("quoted ~ body = %q", b.Text)
	}
	c = parse(t, "pr create -F ~/b.md")
	if f := c.Published[0]; !f.Expand {
		t.Errorf("unquoted ~ body file = %+v, want expanded", f)
	}
	c = parse(t, "pr create --body-file=~/b.md")
	if f := c.Published[0]; f.Expand {
		t.Errorf("--body-file=~/b.md = %+v: bash does not expand ~ after `=` in an argument", f)
	}
}

func TestAPI(t *testing.T) {
	tests := []struct {
		line      string
		method    string
		endpoint  string
		published []string
	}{
		{"api repos/o/r/pulls --jq .x", "GET", "repos/o/r/pulls", nil},
		{"api -X PUT /repos/o/r/pulls/1/merge", "PUT", "repos/o/r/pulls/1/merge", nil},
		{"api --method=put repos/o/r/pulls/1/merge", "PUT", "repos/o/r/pulls/1/merge", nil},
		{"api https://api.github.com/repos/o/r?x=1", "GET", "repos/o/r", nil},
		{
			"api repos/o/r/issues -f a=1 -f b=2",
			"POST",
			"repos/o/r/issues",
			[]string{"--raw-field=a=1", "--raw-field=b=2"},
		},
		{
			"api repos/o/r/issues -F k=@f.md",
			"POST",
			"repos/o/r/issues",
			[]string{"--field=k", "--field@f.md"},
		},
		{
			"api repos/o/r/issues -F k=@-",
			"POST",
			"repos/o/r/issues",
			[]string{"--field=k", "--field@-"},
		},
		{
			"api repos/o/r/issues --input body.json",
			"POST",
			"repos/o/r/issues",
			[]string{"--input@body.json"},
		},
		{"api -X GET search -f q=x", "GET", "search", nil},
	}
	for _, tt := range tests {
		c := parse(t, tt.line)
		if c.Method != tt.method || c.Endpoint != tt.endpoint {
			t.Errorf(
				"%q = %s %s, want %s %s",
				tt.line,
				c.Method,
				c.Endpoint,
				tt.method,
				tt.endpoint,
			)
		}
		if got := inputs(c); !slices.Equal(got, tt.published) {
			t.Errorf("%q published = %q, want %q", tt.line, got, tt.published)
		}
	}
}

func TestGraphQLQueries(t *testing.T) {
	c := parse(t, "api graphql -f query=Q1 -F query=@q.graphql --input in.json -f owner=o")
	got := make([]string, 0, len(c.Queries))
	for _, q := range c.Queries {
		got = append(got, q.Flag+":"+q.Text+q.Path)
	}
	want := []string{"--raw-field:Q1", "--field:q.graphql", "--input:in.json"}
	if !slices.Equal(got, want) {
		t.Errorf("queries = %q, want %q", got, want)
	}
	if !c.Queries[2].JSON {
		t.Errorf("--input query not marked JSON")
	}
	if q, ok := QueryFromJSON(
		[]byte(`{"query":"mutation{x}","variables":{}}`),
	); !ok ||
		q != "mutation{x}" {
		t.Errorf("QueryFromJSON = %q, %v", q, ok)
	}
}

func TestUnknownFlag(t *testing.T) {
	c := parse(t, "pr create --newflag x --body B -t T")
	if c.Err == nil {
		t.Fatal("unknown flag parsed")
	}
	if want := []string{
		"--newflag",
		"x",
		"--body",
		"B",
		"-t",
		"T",
	}; !slices.Equal(
		c.Literal,
		want,
	) {
		t.Errorf("literal = %q, want %q", c.Literal, want)
	}
	c = parse(t, "pr create -t T --newflag")
	if got := inputs(c); !slices.Equal(got, []string{"--title=T"}) {
		t.Errorf("inputs before the error = %q", got)
	}
	if !slices.Equal(c.Literal, []string{"--newflag"}) {
		t.Errorf("literal = %q", c.Literal)
	}
	c = parse(t, "api --newflag -X PUT repos/o/r/pulls/1/merge")
	if c.Method != http.MethodPut || c.Endpoint != "repos/o/r/pulls/1/merge" {
		t.Errorf("api after unknown flag = %s %s", c.Method, c.Endpoint)
	}
}

func TestIsPullMerge(t *testing.T) {
	for e, want := range map[string]bool{
		"repos/o/r/pulls/1/merge":  true,
		"repos/o/r/pulls/1/merge/": true,
		"repos/o/r/pulls/1":        false,
		"repos/o/r/pulls/1/files":  false,
		"repos//r/pulls/1/merge":   false,
		"repos/o/r/issues/1/merge": false,
	} {
		if got := IsPullMerge(e); got != want {
			t.Errorf("IsPullMerge(%q) = %v, want %v", e, got, want)
		}
	}
}
