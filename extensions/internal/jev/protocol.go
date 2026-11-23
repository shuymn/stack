package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Statuses an extension answers; jev extensions never block.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
)

// Finding kinds shared by the extensions.
const (
	KindError      = "jev-error"
	KindUnassessed = "jev-unassessed"
	KindUnrecorded = "jev-unrecorded"
	KindOmitted    = "jev-omitted"
)

// Response bounds (§3.1): half core's 1 MiB stdout limit.
const (
	MaxResponse  = 512 << 10
	maxContext   = 64 << 10
	maxLine      = 1000
	findingSlack = 4 << 10
)

var errRequest = errors.New("invalid request")

// Request is the part of a protocol request the extensions read.
type Request struct {
	Stage  string `json:"stage"`
	Repo   string `json:"repo"`
	Range  string `json:"range"`
	Prompt string `json:"prompt"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Branch string `json:"branch"`
	// Jev is nil when the field is omitted; an explicit null is kept, an invalid replay (§3.4).
	Jev json.RawMessage `json:"jev"`
}

// ParseRequest decodes stdin, which must be one JSON object with a string stage.
func ParseRequest(data []byte) (Request, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return Request{}, fmt.Errorf("%w: %w", errRequest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Request{}, fmt.Errorf("%w: trailing data", errRequest)
	}
	if len(raw) == 0 || raw[0] != '{' {
		return Request{}, fmt.Errorf("%w: not an object", errRequest)
	}
	var probe struct {
		Stage *string `json:"stage"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Stage == nil {
		return Request{}, fmt.Errorf("%w: no string stage", errRequest)
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return Request{}, fmt.Errorf("%w: %w", errRequest, err)
	}
	return req, nil
}

// Finding is one finding of a response.
type Finding struct {
	Kind   string `json:"kind"`
	Where  string `json:"where,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Response is a protocol response.
type Response struct {
	Status   string    `json:"status"`
	Context  string    `json:"context"`
	Findings []Finding `json:"findings"`
}

// Reply builds a response: findings and context lines in order, warn once any warning is added.
type Reply struct {
	warn     bool
	lines    []string
	findings []Finding
}

// Warn adds a finding that makes the status warn, with its context line.
func (a *Reply) Warn(f Finding, line string) {
	a.warn = true
	a.Note(f, line)
}

// Note adds a finding that leaves the status, with its context line ("" for none).
func (a *Reply) Note(f Finding, line string) {
	a.findings = append(a.findings, f)
	a.Line(line)
}

// Line adds a context line alone.
func (a *Reply) Line(line string) {
	if line != "" {
		a.lines = append(a.lines, line)
	}
}

// Response is the bounded response: context lines cut at 1,000 bytes and 64 KiB with a marker,
// findings past the 512 KiB bound counted in one jev-omitted.
func (a *Reply) Response() Response {
	r := Response{Status: StatusPass, Findings: []Finding{}}
	if a.warn {
		r.Status = StatusWarn
	}
	var ctx strings.Builder
	for i, l := range a.lines {
		l = cut(l, maxLine)
		if ctx.Len()+len(l)+1 > maxContext-maxLine {
			if i > 0 {
				ctx.WriteByte('\n')
			}
			_, _ = fmt.Fprintf(&ctx, "[jev: %d more lines omitted]", len(a.lines)-i)
			break
		}
		if i > 0 {
			ctx.WriteByte('\n')
		}
		ctx.WriteString(l)
	}
	r.Context = ctx.String()
	size := encodedSize(r)
	for i, f := range a.findings {
		n := encodedSize(f) + 1
		if size+n > MaxResponse-findingSlack {
			r.Findings = append(r.Findings, Finding{
				Kind: KindOmitted, Detail: fmt.Sprintf("%d findings omitted", len(a.findings)-i),
			})
			break
		}
		size += n
		r.Findings = append(r.Findings, f)
	}
	return r
}

// Write encodes r as one line.
func Write(w io.Writer, r Response) error {
	data, err := Marshal(r)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

func encodedSize(v any) int {
	data, _ := Marshal(v)
	return len(data)
}

// cut keeps at most n bytes of s, on a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
