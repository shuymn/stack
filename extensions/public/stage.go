package public

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shuymn/stack/extensions/internal/checkpub"
	"github.com/shuymn/stack/extensions/internal/proc"
)

const (
	// maxResponse bounds the encoded response, well under the protocol's 1 MiB stdout limit, past
	// which core would record a warn and let a hit through.
	maxResponse = 512 << 10
	// lineReserve is kept free of hit lines for the error line, the trailer and the envelope.
	lineReserve = 16 << 10
	// maxLine cuts each context line, so one line always fits.
	maxLine = 1000
	// hitPrefix starts every hit line Check writes.
	hitPrefix = "check public: "

	// outboundDeadline is the protocol's outbound stage deadline, and pipeReaping the time core
	// allows for an extension's pipes after it exits (docs/architecture.md §3).
	outboundDeadline = 120 * time.Second
	pipeReaping      = 2 * time.Second
	// answerReserve is kept after the check's deadline for the killed git to exit and its pipes to
	// close (proc's 2 s wait delay, with margin) and for the response to be written.
	answerReserve = 18 * time.Second
)

// checkTimeout bounds stage mode from its start to the end of the scan, so it answers block itself
// before core's stage deadline would record a warn. A variable only for tests.
var checkTimeout = outboundDeadline - pipeReaping - answerReserve

var (
	errDeadline   = errors.New("the check did not finish by its deadline")
	errNotObject  = errors.New("the request is not an object")
	errNoStage    = errors.New("the request has no stage")
	objectOpening = []byte("{")
)

// request is the part of an outbound request stack-public reads.
type request struct {
	Stage  string `json:"stage"`
	Repo   string `json:"repo"`
	Range  string `json:"range"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Branch string `json:"branch"`
}

type response struct {
	Status   string    `json:"status"`
	Context  string    `json:"context"`
	Findings []finding `json:"findings"`
}

// finding is one hit, or the error that stopped the check.
type finding struct {
	Hit   string `json:"hit,omitempty"`
	Error string `json:"error,omitempty"`
}

// stage answers one protocol request on env.Stdin. Outside outbound it passes silently. At
// outbound it scans range (when non-empty), title, body and branch, and answers block on a hit or
// on its own error (it fails closed), pass otherwise.
func stage(ctx context.Context, env Env) int {
	resp := respond(ctx, env)
	if err := encode(env.Stdout, resp); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "stack-public: write response: %v\n", err)
		return 1
	}
	return 0
}

func respond(ctx context.Context, env Env) response {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	h := &hits{}
	data, err := io.ReadAll(env.Stdin)
	var req request
	if err == nil {
		req, err = decodeRequest(data)
	}
	if err != nil {
		return h.block(fmt.Errorf("cannot read the request: %w", err))
	}
	if req.Stage != "outbound" {
		return response{Status: "pass", Findings: []finding{}}
	}
	terms, err := privateTerms(env)
	if err != nil {
		return h.block(err)
	}
	err = checkpub.Check(ctx, checkpub.Options{
		Range:  req.Range,
		Dir:    req.Repo,
		Runner: proc.Exec{},
		Terms:  terms,
		Meta: []checkpub.Text{
			{Where: "title", Text: req.Title},
			{Where: "body", Text: req.Body},
			{Where: "branch", Text: req.Branch},
		},
		Out: h,
	})
	switch {
	case err == nil:
		return response{Status: "pass", Findings: []finding{}}
	case errors.Is(err, checkpub.ErrFound):
		return h.block(nil)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return h.block(fmt.Errorf("%w (%s): %w", errDeadline, checkTimeout, err))
	default:
		return h.block(err)
	}
}

// decodeRequest decodes a request, which must be a JSON object with a string stage.
func decodeRequest(data []byte) (request, error) {
	var req request
	if !bytes.HasPrefix(bytes.TrimSpace(data), objectOpening) {
		return req, errNotObject
	}
	var probe struct {
		Stage *string `json:"stage"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return req, fmt.Errorf("decode: %w", err)
	}
	if probe.Stage == nil {
		return req, errNoStage
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, fmt.Errorf("decode: %w", err)
	}
	return req, nil
}

// hits collects the hit lines Check writes, in scan order, while the response stays within its
// cap, and counts the rest.
type hits struct {
	partial  []byte
	lines    []string
	findings []finding
	size     int
	more     int
}

// Write receives whole hit lines from Check.
func (h *hits) Write(p []byte) (int, error) {
	h.partial = append(h.partial, p...)
	for {
		line, rest, ok := bytes.Cut(h.partial, []byte("\n"))
		if !ok {
			break
		}
		h.add(string(line))
		h.partial = rest
	}
	return len(p), nil
}

func (h *hits) add(line string) {
	// Once a hit is left out, every later one is, so the kept hits stay a prefix in scan order.
	if h.more > 0 {
		h.more++
		return
	}
	line = cutLine(line)
	f := finding{Hit: strings.TrimPrefix(line, hitPrefix)}
	cost := lineCost(line) + encodedLen(f) + 1
	if h.size+cost > maxResponse-lineReserve {
		h.more++
		return
	}
	h.size += cost
	h.lines = append(h.lines, line)
	h.findings = append(h.findings, f)
}

// block is the block response: the hit lines kept, then the error when err is not nil, then the
// trailer when hits were left out.
func (h *hits) block(err error) response {
	lines := h.lines
	findings := append([]finding{}, h.findings...)
	if err != nil {
		e := cutLine("stack-public: " + err.Error())
		lines = append(lines, e)
		findings = append(findings, finding{Error: e})
	}
	if h.more > 0 {
		lines = append(lines, fmt.Sprintf("%s%d more hits", hitPrefix, h.more))
	}
	return response{Status: "block", Context: strings.Join(lines, "\n"), Findings: findings}
}

// cutLine keeps at most maxLine bytes of line, on a UTF-8 boundary.
func cutLine(line string) string {
	if len(line) <= maxLine {
		return line
	}
	i := maxLine
	for i > 0 && !utf8.RuneStart(line[i]) {
		i--
	}
	return line[:i]
}

// lineCost is what line and its separator add to the encoded context.
func lineCost(line string) int {
	return encodedLen(line+"\n") - len(`""`)
}

// encodedLen is the length of v as encode writes it, without the newline.
func encodedLen(v any) int {
	var b bytes.Buffer
	_ = encode(&b, v)
	return b.Len() - 1
}

func encode(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return nil
}
