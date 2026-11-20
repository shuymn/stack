package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstPrompt(t *testing.T) {
	t.Parallel()
	meta := `{"type":"user","isMeta":true,"message":{"content":"meta"}}`
	summary := `{"type":"user","isCompactSummary":true,"message":{"content":"summary"}}`
	side := `{"type":"user","isSidechain":true,"message":{"content":"side"}}`
	assistant := `{"type":"assistant","message":{"content":"hi"}}`
	long := strings.Repeat("x", 200_000) // past bufio.Scanner's default line limit

	tests := []struct {
		name    string
		lines   []string
		eol     string
		want    string
		wantErr bool
	}{
		{
			name: "string after skipped records",
			lines: []string{
				meta,
				summary,
				side,
				assistant,
				`{"type":"user","message":{"content":"brief"}}`,
			},
			want: "brief",
		},
		{
			name: "block array joins text blocks",
			lines: []string{
				`{"type":"user","message":{"content":[{"type":"text","text":"a"},` +
					`{"type":"image"},{"type":"text","text":"b"}]}}`,
			},
			want: "a\n\nb",
		},
		{
			name: "tool_result first is skipped",
			lines: []string{
				`{"type":"user","message":{"content":[{"type":"tool_result","content":"r"}]}}`,
				`{"type":"user","message":{"content":"next"}}`,
			},
			want: "next",
		},
		{
			name:  "array without text or tool_result is empty",
			lines: []string{`{"type":"user","message":{"content":[{"type":"image"}]}}`},
			want:  "",
		},
		{
			name: "CRLF lines and blank lines",
			lines: []string{
				meta, "", `{"type":"user","message":{"content":"line1\nline2"}}`,
			},
			eol:  "\r\n",
			want: "line1\nline2",
		},
		{
			name:  "long line",
			lines: []string{`{"type":"user","message":{"content":"` + long + `"}}`},
			want:  long,
		},
		{
			name:    "malformed line ends the read",
			lines:   []string{meta, "{", `{"type":"user","message":{"content":"x"}}`},
			wantErr: true,
		},
		{
			name:    "null content",
			lines:   []string{`{"type":"user","message":{"content":null}}`},
			wantErr: true,
		},
		{name: "no user prompt", lines: []string{meta, assistant}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eol := tt.eol
			if eol == "" {
				eol = "\n"
			}
			path := filepath.Join(t.TempDir(), "t.jsonl")
			if err := os.WriteFile(
				path,
				[]byte(strings.Join(tt.lines, eol)+eol),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			got, err := FirstPrompt(path)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("FirstPrompt = %q, %v; want %q, error %v",
					trunc(got), err, trunc(tt.want), tt.wantErr)
			}
		})
	}
	if _, err := FirstPrompt(filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
		t.Error("absent transcript: want an error")
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
