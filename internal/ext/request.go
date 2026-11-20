package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shuymn/stack/internal/proc"
)

// gitTimeout bounds each git call that builds a request.
const gitTimeout = 30 * time.Second

var (
	// ErrRange reports an explicit --range that git cannot resolve.
	ErrRange = errors.New("cannot resolve range")
	// ErrReplay reports a replay file that cannot be used.
	ErrReplay = errors.New("invalid replay file")
)

// Request is the JSON object an extension reads from stdin.
type Request struct {
	Stage  string `json:"stage"`
	Repo   string `json:"repo"`
	Range  string `json:"range"`
	Prompt string `json:"prompt"`
}

// Git runs git to build a request.
type Git struct {
	Runner proc.Runner
	// Name is the git executable; empty means "git".
	Name string
	// Dir is where git looks for the repository; empty means the current directory.
	Dir string
	// Env is git's environment; the caller drops repository-location variables (GIT_DIR, …) so the
	// repository is found from Dir, never from variables a git hook exported.
	Env []string
}

// BuildRequest returns the request of stage: repo is the toplevel of the repository around g.Dir
// or ""; range is rng when non-empty, which git must resolve (else ErrRange), else for outbound
// `@{upstream}..HEAD` when the branch has an upstream, else "".
func BuildRequest(ctx context.Context, g Git, stage, rng, prompt string) (Request, error) {
	req := Request{Stage: stage, Range: rng, Prompt: prompt}
	if out, ok := g.run(ctx, "rev-parse", "--show-toplevel"); ok {
		// Only git's line end: a directory name may itself end in a space.
		req.Repo = strings.TrimSuffix(out, "\n")
	}
	switch {
	case rng != "":
		// rev-list resolves every revision of a range; an option-looking R stays a revision.
		if _, ok := g.run(ctx, "rev-list", "--max-count=1", "--end-of-options", rng); !ok {
			return Request{}, fmt.Errorf("%w: %q", ErrRange, rng)
		}
	case stage == StageOutbound:
		if _, ok := g.run(ctx, "rev-parse", "--verify", "--quiet", "@{upstream}"); ok {
			req.Range = "@{upstream}..HEAD"
		}
	}
	return req, nil
}

// run reports git's stdout and whether it exited 0 with complete output.
func (g Git) run(ctx context.Context, args ...string) (string, bool) {
	name := g.Name
	if name == "" {
		name = "git"
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	res, err := g.Runner.Run(ctx, proc.Cmd{Name: name, Args: args, Dir: g.Dir, Env: g.Env})
	if err != nil || res.TimedOut || res.WaitDelayExceeded || res.Truncated || res.ExitCode != 0 {
		return "", false
	}
	return string(res.Stdout), true
}

// Replay is a `--replay` fixture.
type Replay struct {
	// Request is sent verbatim.
	Request json.RawMessage
	// Stage is the request's stage.
	Stage string
	// Expect is the expected aggregate status, empty when the fixture has none.
	Expect string
}

// ParseReplay decodes `{"request":{…},"expect":{"status":…}}`; expect is optional. The request must
// be an object with a string stage, and an expected status must be pass, warn or block.
func ParseReplay(data []byte) (Replay, error) {
	var doc struct {
		Request json.RawMessage `json:"request"`
		Expect  *struct {
			Status string `json:"status"`
		} `json:"expect"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return Replay{}, fmt.Errorf("%w: %w", ErrReplay, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Replay{}, fmt.Errorf("%w: trailing data", ErrReplay)
	}
	var req struct {
		Stage *string `json:"stage"`
	}
	if trimmed := bytes.TrimSpace(doc.Request); len(trimmed) == 0 || trimmed[0] != '{' {
		return Replay{}, fmt.Errorf("%w: request is not an object", ErrReplay)
	}
	if err := json.Unmarshal(doc.Request, &req); err != nil || req.Stage == nil {
		return Replay{}, fmt.Errorf("%w: request.stage is not a string", ErrReplay)
	}
	r := Replay{Request: bytes.TrimSpace(doc.Request), Stage: *req.Stage}
	if doc.Expect != nil {
		switch doc.Expect.Status {
		case StatusPass, StatusWarn, StatusBlock:
			r.Expect = doc.Expect.Status
		default:
			return Replay{}, fmt.Errorf("%w: expect.status %q is not pass, warn or block",
				ErrReplay, doc.Expect.Status)
		}
	}
	return r, nil
}
