package shell

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// argv returns the static texts of a call's words, "$" for a dynamic one.
func argv(c Call) []string {
	out := make([]string, len(c.Args))
	for i, a := range c.Args {
		if a.Static {
			out[i] = a.Text
		} else {
			out[i] = "$"
		}
	}
	return out
}

func argvs(r Result) [][]string {
	out := make([][]string, len(r.Calls))
	for i, c := range r.Calls {
		out[i] = argv(c)
	}
	return out
}

func find(t *testing.T, r Result, name string) Call {
	t.Helper()
	for _, c := range r.Calls {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("no %s call in %q", name, argvs(r))
	return Call{}
}

func TestParseCalls(t *testing.T) {
	tests := []struct {
		cmd  string
		want [][]string
	}{
		{`gh pr view "1" 'x y'`, [][]string{{"gh", "pr", "view", "1", "x y"}}},
		{`gh "a"'b'c`, [][]string{{"gh", "abc"}}},
		{`gh a\ b "q\"x" "\a"`, [][]string{{"gh", "a b", `q"x`, `\a`}}},
		{`gh $VAR "$X" "a${b}"`, [][]string{{"gh", "$", "$", "$"}}},
		{`A=1 B=2 gh x`, [][]string{{"gh", "x"}}},
		{`env -- A=1 gh x`, [][]string{{"gh", "x"}}},
		{`env -u A B=1 gh x`, [][]string{{"gh", "x"}}},
		{`command -p nohup time -p gh x`, [][]string{{"gh", "x"}}},
		{`mise exec go@1 -- gh x`, [][]string{{"gh", "x"}}},
		{`/opt/bin/gh.exe x`, [][]string{{"/opt/bin/gh.exe", "x"}}},
		{`a && gh x || b; c | d`, [][]string{{"a"}, {"gh", "x"}, {"b"}, {"c"}, {"d"}}},
		{
			`bash -c 'gh x; rm -rf y'`,
			[][]string{{"bash", "-c", "gh x; rm -rf y"}, {"gh", "x"}, {"rm", "-rf", "y"}},
		},
		{`sh -ec "gh x"`, [][]string{{"sh", "-ec", "gh x"}, {"gh", "x"}}},
		{`eval 'gh x'`, [][]string{{"eval", "gh x"}, {"gh", "x"}}},
		{"bash <<'EOF'\ngh x\nEOF\n", [][]string{{"bash"}, {"gh", "x"}}},
		{"bash <<< 'gh x'", [][]string{{"bash"}, {"gh", "x"}}},
		{`echo "$(gh x)" ` + "`gh y`", [][]string{{"gh", "x"}, {"gh", "y"}, {"echo", "$", "$"}}},
		{`bash -c "bash -c 'bash -c \"gh deep\"'"`, [][]string{
			{"bash", "-c", `bash -c 'bash -c "gh deep"'`},
			{"bash", "-c", `bash -c "gh deep"`},
			{"bash", "-c", "gh deep"},
		}},
		{
			`echo "gh pr merge 1"; echo 'rm -rf /'`,
			[][]string{{"echo", "gh pr merge 1"}, {"echo", "rm -rf /"}},
		},
		{"cat <<'EOF'\ngh pr merge 1\nEOF\n", [][]string{{"cat"}}},
		{
			"if true; then gh x; fi; for i in 1; do gh y; done",
			[][]string{{"true"}, {"gh", "x"}, {"gh", "y"}},
		},
		{"f() { gh x; }", [][]string{{"gh", "x"}}},
		{"( gh x ) && { gh y; }", [][]string{{"gh", "x"}, {"gh", "y"}}},
		{"export A=\"$(gh x)\"", [][]string{{"gh", "x"}}},
	}
	for _, tt := range tests {
		got := argvs(Parse(tt.cmd, "", ""))
		if !slices.EqualFunc(got, tt.want, slices.Equal) {
			t.Errorf("%q calls = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

func TestParseWords(t *testing.T) {
	r := Parse(`rm -rf '~' ~ ~/x "~" a$B`, "", "")
	c := find(t, r, "rm")
	want := []Word{
		{Text: "rm", Static: true},
		{Text: "-rf", Static: true},
		{Text: "~", Static: true},
		{Text: "~", Static: true, Tilde: true},
		{Text: "~/x", Static: true, Tilde: true},
		{Text: "~", Static: true},
		{},
	}
	if !slices.Equal(c.Args, want) {
		t.Errorf("words = %+v, want %+v", c.Args, want)
	}
}

func TestParseCwd(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	sub := filepath.Join(cwd, "d")
	tests := []struct {
		cmd  string
		want string
	}{
		{"gh x", cwd},
		{"cd d && gh x", sub},
		{"cd d && cd e && gh x", filepath.Join(sub, "e")},
		{"cd d; gh x", ""},
		{"cd d || gh x", ""},
		{"cd $D && gh x", ""},
		{"cd d && true; gh x", ""},
		{"cd && gh x", home},
		{"cd ~ && gh x", home},
		{"cd - && gh x", ""},
		{"(cd d && true) && gh x", cwd},
		{"true && gh x", cwd},
		{"pushd d && gh x", ""},
		{"env -C d gh x", ""},
		{"bash -c 'cd d && gh x'", sub},
	}
	for _, tt := range tests {
		c := find(t, Parse(tt.cmd, cwd, home), "gh")
		if c.Cwd != tt.want {
			t.Errorf("%q: gh cwd = %q, want %q", tt.cmd, c.Cwd, tt.want)
		}
	}
	if c := find(t, Parse("cd d && gh x", "", home), "gh"); c.Cwd != "" {
		t.Errorf("unknown cwd: gh cwd = %q, want unknown", c.Cwd)
	}
}

func TestParseStdin(t *testing.T) {
	tests := []struct {
		cmd  string
		want Stdin
	}{
		{"gh x", Stdin{}},
		{"gh x <<'EOF'\nbody\nEOF\n", Stdin{Kind: StdinText, Text: "body\n"}},
		{"gh x <<EOF\nbody $X\nEOF\n", Stdin{Kind: StdinDynamic}},
		{"gh x <<< 'line'", Stdin{Kind: StdinText, Text: "line\n"}},
		{"gh x < b.md", Stdin{Kind: StdinFile, File: Word{Text: "b.md", Static: true}}},
		{"gh x < $F", Stdin{Kind: StdinDynamic}},
		{"cat b.md | gh x", Stdin{Kind: StdinDynamic}},
		{"gh x 2< f", Stdin{}},
	}
	for _, tt := range tests {
		c := find(t, Parse(tt.cmd, "", ""), "gh")
		if c.Stdin != tt.want {
			t.Errorf("%q stdin = %+v, want %+v", tt.cmd, c.Stdin, tt.want)
		}
	}
}

func TestParseSyntaxError(t *testing.T) {
	tests := []struct {
		cmd  string
		want [][]string
	}{
		{"gh a\ngh b; if", [][]string{{"gh", "a"}}},
		{"gh a; if", nil},
		{"gh a\ngh b\n)", [][]string{{"gh", "a"}, {"gh", "b"}}},
		{"gh a <<'EOF'\nx\nEOF\ngh b 'unterminated", [][]string{{"gh", "a"}}},
	}
	for _, tt := range tests {
		got := argvs(Parse(tt.cmd, "", ""))
		if !slices.EqualFunc(got, tt.want, slices.Equal) {
			t.Errorf("%q calls = %q, want %q", tt.cmd, got, tt.want)
		}
	}
	if c := find(t, Parse("gh a <<'EOF'\nx\nEOF\n(", "", ""), "gh"); c.Stdin.Text != "x\n" {
		t.Errorf("heredoc before a later error not bound: %+v", c.Stdin)
	}
}

func TestParseHandoffs(t *testing.T) {
	const f = "F"
	tests := []struct {
		cmd  string
		want []string // the source text of each handoff range
	}{
		{`K="$(cat F)" cmd`, []string{"$(cat F)"}},
		{`K=$(cat F) cmd`, []string{"$(cat F)"}},
		{`export K="$(< F)"`, []string{"$(< F)"}},
		{`local K="$(cat 'F')"`, []string{"$(cat 'F')"}},
		{`env K="$(cat F)" cmd`, []string{"$(cat F)"}},
		{`cmd --token "$(cat F)"`, []string{"$(cat F)"}},
		{`cmd --token="$(cat F)"`, []string{"$(cat F)"}},
		{`cmd -H "Bearer $(cat F)"`, []string{"$(cat F)"}},
		{`bash -c 'K="$(cat F)" cmd'`, []string{"$(cat F)"}},
		{`cmd "$(cat F)"`, nil},
		{`cmd -- "$(cat F)"`, nil},
		{`cmd -- --token "$(cat F)"`, nil},
		{`echo -n "$(cat F)"`, nil},
		{`printf -v "$(cat F)"`, nil},
		{`cat -A "$(cat F)"`, nil},
		{`K="$(cat F | tee x)" cmd`, nil},
		{`K="$(cat F G)" cmd`, nil},
		{`K="$(cat -v F)" cmd`, nil},
		{`K="$(cat $F)" cmd`, nil},
		{`K="$(< F 2>x)" cmd`, nil},
		{"K=`cat F` cmd", nil},
		{`K="$(cat F; true)" cmd`, nil},
	}
	for _, tt := range tests {
		r := Parse(tt.cmd, "", "")
		var got []string
		for _, h := range r.Handoffs {
			got = append(got, tt.cmd[h.Start:h.End])
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%q handoffs = %q, want %q", tt.cmd, got, tt.want)
		}
		for _, h := range got {
			if !strings.Contains(h, f) {
				t.Errorf("%q: handoff %q misses the operand", tt.cmd, h)
			}
		}
	}
}
