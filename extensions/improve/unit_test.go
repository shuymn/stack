package improve

import (
	"testing"
	"time"
)

func TestMatchPaths(t *testing.T) {
	t.Parallel()
	pats := []string{"home/dot_claude/skills/**", "docs/*.md", "a/**/b.md"}
	for p, want := range map[string]bool{
		"home/dot_claude/skills/x.md":       true,
		"home/dot_claude/skills/h/r/x.md":   true,
		"home/dot_claude/skills":            false,
		"home/dot_claude/skills2/x.md":      false,
		"home/dot_claude/agents/x.md":       false,
		"docs/x.md":                         true,
		"docs/sub/x.md":                     false,
		"a/b.md":                            true,
		"a/x/y/b.md":                        true,
		"a/x/c.md":                          false,
		"xhome/dot_claude/skills/x.md":      false,
		"home/dot_claude/skills/../../x.md": true, // git never reports such a path
	} {
		if got := matchAny(pats, p); got != want {
			t.Errorf("matchAny(%q) = %v, want %v", p, got, want)
		}
	}
	for _, bad := range []string{"", "/abs/**", "a//b", "a/[/b"} {
		if validPattern(bad) {
			t.Errorf("validPattern(%q) = true", bad)
		}
	}
}

func TestWeekName(t *testing.T) {
	t.Parallel()
	for at, want := range map[string]string{
		"2026-11-16T00:00:00Z": "2026-W47",
		"2026-11-22T23:59:59Z": "2026-W47",
		"2026-11-23T00:00:00Z": "2026-W48",
		"2027-01-01T12:00:00Z": "2026-W53",
		"2025-12-29T12:00:00Z": "2026-W01",
	} {
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		if got := weekName(ts); got != want {
			t.Errorf("weekName(%s) = %s, want %s", at, got, want)
		}
	}
	// The week is the local date's: a Sunday evening in UTC-5 is already Monday in UTC.
	sunday := time.Date(2026, 11, 22, 21, 0, 0, 0, time.FixedZone("x", -5*3600))
	if got := weekName(sunday); got != "2026-W47" {
		t.Errorf("local Sunday = %s", got)
	}
}

func TestParseResult(t *testing.T) {
	t.Parallel()
	zero := `"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":0}`
	for _, tc := range []struct {
		in          string
		known, zero bool
	}{
		{`{"type":"result","total_cost_usd":0,` + zero + `}`, true, true},
		{`{"type":"result","total_cost_usd":0.0,` + zero + `}`, true, true},
		{`{"type":"result","total_cost_usd":1e999,` + zero + `}`, false, false},
		{`{"type":"result","total_cost_usd":-1,` + zero + `}`, false, false},
		{`{"type":"result","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":1e0}}`, false, false},
		{`{"type":"result","total_cost_usd":0,"usage":null}`, false, false},
		{`[]`, false, false},
		{`{"total_cost_usd":0,` + zero + `}`, false, false},
		{`{"type":"result","total_cost_usd":0,` + zero + "}\n", true, true},
		{`{"type":"result","total_cost_usd":0,` + zero + "}\n{broken", false, false},
		{`{"type":"result","total_cost_usd":0,` + zero + `}{}`, false, false},
	} {
		r := parseResult([]byte(tc.in))
		if r.known != tc.known || r.zero != tc.zero {
			t.Errorf("%s: known %v zero %v", tc.in, r.known, r.zero)
		}
	}
}
