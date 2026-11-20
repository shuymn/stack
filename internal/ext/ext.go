// Package ext runs optional extensions over the process protocol of docs/architecture.md §3.
//
// An extension `<name>` is the executable `stack-<name>` on PATH. It receives one JSON request on
// stdin and answers one JSON response on stdout. An extension that is not installed is skipped; one
// that cannot run or answers badly yields `warn`, never `block`, so a broken extension cannot stop
// a stage. Core never looks inside `extensions/`.
package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shuymn/stack/internal/config"
	"github.com/shuymn/stack/internal/proc"
)

// Stages, in the order `stack run` lists them.
const (
	StageEntry    = "entry"
	StageOutbound = "outbound"
	StageCycle    = "cycle"
)

// Statuses of a response and of an aggregate.
const (
	StatusPass  = "pass"
	StatusWarn  = "warn"
	StatusBlock = "block"
)

// Prefix is prepended to an extension name to form its executable name.
const Prefix = "stack-"

// Per-run timeouts of each stage.
const (
	EntryTimeout    = 10 * time.Second
	OutboundTimeout = 120 * time.Second
	CycleTimeout    = 30 * time.Minute
)

// Timeouts bound each stage: one deadline shared by all its extensions.
var Timeouts = map[string]time.Duration{
	StageEntry:    EntryTimeout,
	StageOutbound: OutboundTimeout,
	StageCycle:    CycleTimeout,
}

// severity orders statuses for aggregation: block > warn > pass.
var severity = []string{StatusPass, StatusWarn, StatusBlock}

var (
	// ErrInvalidName reports an extension name that is not a single lower-case file-name component.
	ErrInvalidName = errors.New("invalid extension name")
	// ErrUnknownStage reports a stage other than entry, outbound or cycle.
	ErrUnknownStage = errors.New("unknown stage")

	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// ValidName reports whether name may name an extension: `^[a-z0-9][a-z0-9-]*$`, so it is one
// file-name component on every OS (`../x`, `a/b` and `C:x` are not).
func ValidName(name string) bool {
	return nameRe.MatchString(name)
}

// CheckNames returns an ErrInvalidName error for the first invalid name.
func CheckNames(names []string) error {
	for _, n := range names {
		if !ValidName(n) {
			return fmt.Errorf("%w: %q (want ^[a-z0-9][a-z0-9-]*$)", ErrInvalidName, n)
		}
	}
	return nil
}

// Names returns the configured extension names of stage.
func Names(s config.Stages, stage string) ([]string, error) {
	switch stage {
	case StageEntry:
		return s.Entry, nil
	case StageOutbound:
		return s.Outbound, nil
	case StageCycle:
		return s.Cycle, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStage, stage)
	}
}

// Plan is one selected extension and how it resolved on PATH.
type Plan struct {
	Name string
	// Path is the executable; empty when skipped.
	Path string
	// Err is nil when the extension runs, wraps proc.ErrNotFound when it is skipped, and is any
	// other lookup error when it is found but not runnable or cannot be verified (a warn).
	Err error
}

// Skipped reports an extension that is not installed.
func (p Plan) Skipped() bool {
	return errors.Is(p.Err, proc.ErrNotFound)
}

// Runnable reports an extension that will be started.
func (p Plan) Runnable() bool {
	return p.Err == nil
}

func (p Plan) String() string {
	switch {
	case p.Runnable():
		return p.Name + " -> " + p.Path
	case p.Skipped():
		return p.Name + " -> skipped"
	default:
		return p.Name + " -> not runnable: " + p.Err.Error()
	}
}

// Resolve looks up `stack-<name>` on PATH for each name. Names must already be valid.
func Resolve(names []string) []Plan {
	plans := make([]Plan, 0, len(names))
	for _, n := range names {
		path, err := proc.LookPathExe(Prefix + n)
		plans = append(plans, Plan{Name: n, Path: path, Err: err})
	}
	return plans
}

// AnyToRun reports whether any plan needs a request: a runnable one.
func AnyToRun(plans []Plan) bool {
	for _, p := range plans {
		if p.Runnable() {
			return true
		}
	}
	return false
}

// Result is one extension's evaluated response.
type Result struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Context string `json:"context"`
	// Findings is the response's findings array, kept as raw JSON; a warn for a broken extension
	// holds one synthetic finding `{"extension":<name>,"error":<reason>}`.
	Findings json.RawMessage `json:"findings"`
	// Error is the reason for a synthetic warn, empty otherwise.
	Error string `json:"error"`
	// Path is the executable that ran, empty when none did.
	Path string `json:"-"`
}

// Aggregate is the outcome of a stage.
type Aggregate struct {
	Stage   string   `json:"stage"`
	Status  string   `json:"status"`
	Results []Result `json:"results"`
	Skipped []string `json:"skipped"`
}

// Options configures Run.
type Options struct {
	Runner proc.Runner
	// Dir is the extensions' working directory; empty means the current one.
	Dir string
	// Env is the base environment of every extension; nil means os.Environ(). STACK_STAGE and,
	// when SessionID is set, STACK_SESSION_ID replace any inherited value.
	Env       []string
	SessionID string
	// Stderr receives each extension's stderr, every line prefixed `[<name>] `; nil discards it.
	// It must not block.
	Stderr io.Writer
	// Timeout is the stage's one deadline, shared by all its extensions; zero means the stage's
	// entry in Timeouts.
	Timeout time.Duration
	// MaxStdout caps each response; zero means proc's 1 MiB.
	MaxStdout int64
}

// Run runs the runnable plans of stage in order, each with stdin as its request, and aggregates
// the statuses as block > warn > pass. No extension run yields pass.
func Run(ctx context.Context, o Options, stage string, stdin []byte, plans []Plan) Aggregate {
	agg := Aggregate{Stage: stage, Status: StatusPass, Results: []Result{}, Skipped: []string{}}
	for _, p := range plans {
		if p.Skipped() {
			agg.Skipped = append(agg.Skipped, p.Name)
		}
	}
	Stream(ctx, o, stage, stdin, plans, func(r Result) {
		agg.Results = append(agg.Results, r)
		if slices.Index(severity, r.Status) > slices.Index(severity, agg.Status) {
			agg.Status = r.Status
		}
	})
	return agg
}

// Stream runs the runnable plans of stage in order under one stage deadline and passes each
// result to each as soon as it is known; skipped plans yield nothing. Once the deadline has
// passed no extension starts: each remaining one warns, so a stage takes at most its timeout
// plus one cleanup grace period of the runner.
func Stream(ctx context.Context, o Options, stage string, stdin []byte, plans []Plan,
	each func(Result),
) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = Timeouts[stage]
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, p := range plans {
		switch {
		case p.Skipped():
			continue
		case !p.Runnable():
			each(warn(p.Name, p.Err.Error()))
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			each(warn(p.Name, fmt.Sprintf("not started: stage deadline of %s exceeded", timeout)))
		case ctx.Err() != nil:
			each(warn(p.Name, "not started: "+ctx.Err().Error()))
		default:
			each(o.runOne(ctx, stage, stdin, p, timeout))
		}
	}
}

func (o Options) runOne(ctx context.Context, stage string, stdin []byte, p Plan,
	timeout time.Duration,
) Result {
	stderr := io.Discard
	if o.Stderr != nil {
		stderr = o.Stderr
	}
	pw := &prefixWriter{w: stderr, prefix: "[" + p.Name + "] "}
	res, err := o.Runner.Run(ctx, proc.Cmd{
		Name:      p.Path,
		Dir:       o.Dir,
		Env:       o.childEnv(stage),
		Stdin:     stdin,
		Stderr:    pw,
		MaxStdout: o.MaxStdout,
	})
	pw.finish()

	r := evaluate(p.Name, res, err, timeout)
	r.Path = p.Path
	return r
}

// evaluate turns a run into a result. A run that timed out, outlived its pipes or overflowed the
// stdout cap warns even when the bytes received so far are a valid response; only then is stdout
// parsed. The exit code is ignored when stdout is a valid response.
func evaluate(name string, res proc.Result, runErr error, timeout time.Duration) Result {
	switch {
	case runErr != nil:
		return warn(name, runErr.Error())
	case res.TimedOut:
		return warn(name, fmt.Sprintf("timed out: stage deadline of %s exceeded", timeout))
	case res.WaitDelayExceeded:
		return warn(name, "stdout or stderr stayed open after the extension exited")
	case res.Truncated:
		return warn(name, "stdout over the response size limit")
	}
	r, err := parse(res.Stdout)
	if err != nil {
		reason := "invalid response: " + err.Error()
		if res.ExitCode != 0 {
			reason = fmt.Sprintf("exit %d; %s", res.ExitCode, reason)
		}
		return warn(name, reason)
	}
	r.Name = name
	return r
}

var errResponse = errors.New("malformed")

// parse decodes one response object followed by nothing but white space. stdout must be UTF-8;
// context, when present, must be a string and findings, when present, an array (null is neither).
// An absent context is "" and absent findings are [].
func parse(stdout []byte) (Result, error) {
	if !utf8.Valid(stdout) {
		return Result{}, fmt.Errorf("%w: not UTF-8", errResponse)
	}
	var resp struct {
		Status   string          `json:"status"`
		Context  json.RawMessage `json:"context"`
		Findings json.RawMessage `json:"findings"`
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&resp); errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("%w: no output", errResponse)
	} else if err != nil {
		return Result{}, fmt.Errorf("%w: %w", errResponse, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("%w: trailing data after the response", errResponse)
	}
	switch resp.Status {
	case StatusPass, StatusWarn, StatusBlock:
	default:
		return Result{}, fmt.Errorf("%w: unknown status %q", errResponse, resp.Status)
	}
	r := Result{Status: resp.Status, Findings: json.RawMessage("[]")}
	if resp.Context != nil {
		if bytes.TrimSpace(resp.Context)[0] != '"' {
			return Result{}, fmt.Errorf("%w: context is not a string", errResponse)
		}
		if err := json.Unmarshal(resp.Context, &r.Context); err != nil {
			return Result{}, fmt.Errorf("%w: context: %w", errResponse, err)
		}
	}
	if resp.Findings != nil {
		if r.Findings = bytes.TrimSpace(resp.Findings); r.Findings[0] != '[' {
			return Result{}, fmt.Errorf("%w: findings is not an array", errResponse)
		}
	}
	return r, nil
}

func warn(name, reason string) Result {
	finding, err := json.Marshal([]map[string]string{{"extension": name, "error": reason}})
	if err != nil { // a map of strings always encodes
		finding = []byte("[]")
	}
	return Result{Name: name, Status: StatusWarn, Findings: finding, Error: reason}
}

func (o Options) childEnv(stage string) []string {
	base := o.Env
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(name, "STACK_STAGE") ||
			(o.SessionID != "" && strings.EqualFold(name, "STACK_SESSION_ID")) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "STACK_STAGE="+stage)
	if o.SessionID != "" {
		env = append(env, "STACK_SESSION_ID="+o.SessionID)
	}
	return env
}

// prefixWriter starts every line with prefix. A partial line is written at once, so the writer
// never holds output back; its continuation is not prefixed again.
type prefixWriter struct {
	w      io.Writer
	prefix string
	mid    bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	n := len(b)
	var buf bytes.Buffer
	for len(b) > 0 {
		if !p.mid {
			buf.WriteString(p.prefix)
			p.mid = true
		}
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			buf.Write(b)
			break
		}
		buf.Write(b[:i+1])
		b = b[i+1:]
		p.mid = false
	}
	if _, err := p.w.Write(buf.Bytes()); err != nil {
		return 0, fmt.Errorf("forward stderr: %w", err)
	}
	return n, nil
}

// finish ends an unterminated last line.
func (p *prefixWriter) finish() {
	if p.mid {
		_, _ = io.WriteString(p.w, "\n")
		p.mid = false
	}
}
