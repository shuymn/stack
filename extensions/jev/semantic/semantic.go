// Package semantic is the jev-semantic outbound extension (docs/design/phase4-jev.md §3.2): it asks
// Jev whether a PR's diff weakens a duty the project performs on itself without its texts saying
// so, and whether the texts claim what the diff does not show. It is advisory: hits warn, it never
// blocks, and with no TYPESAFE_API_KEY it answers pass.
package semantic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
)

// budget runs from process start; a variable only for tests (§3.1).
var budget = 10 * time.Second

// reserve is kept after the work deadline to answer (§3.1).
const reserve = 500 * time.Millisecond

// Env is everything jev-semantic takes from the process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Home is the home directory, for the usage ledger; "" when unknown.
	Home string
	// Getenv reads the key.
	Getenv func(string) string
	// Environ is git's base environment.
	Environ []string
	// Runner runs git; nil means proc.Exec with the pipe wait of §3.1.
	Runner proc.Runner
	// Git is the git executable; "" means git.
	Git string
	// Endpoint is the API URL; "" means the one this binary was built with.
	Endpoint string
	// Client supplies the live transport and timeout (jev.Live); nil means
	// http.DefaultTransport and the work deadline alone.
	Client *http.Client
	// Decisions replaces the shipped decisions when not nil.
	Decisions map[string]jev.Decision
}

// Main answers one protocol request on env.Stdin and returns the exit code.
func Main(ctx context.Context, env Env) int {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	work, cancelWork := context.WithTimeout(ctx, budget-reserve)
	defer cancelWork()
	resp := respond(work, env)
	if err := jev.Write(env.Stdout, resp); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

func pass() jev.Response {
	return jev.Response{Status: jev.StatusPass, Findings: []jev.Finding{}}
}

func warnLine(f jev.Finding, line string) jev.Response {
	r := &jev.Reply{}
	r.Warn(f, line)
	return r.Response()
}

func respond(ctx context.Context, env Env) jev.Response {
	data, err := io.ReadAll(env.Stdin)
	var req jev.Request
	if err == nil {
		req, err = jev.ParseRequest(data)
	}
	if err != nil {
		return warnLine(jev.Finding{Kind: jev.KindError, Detail: "invalid request"},
			name+": invalid request")
	}
	if req.Stage != "outbound" {
		return pass()
	}
	var transport jev.Transport
	if req.Jev != nil {
		replay, err := jev.ParseReplay(req.Jev)
		if err != nil {
			return warnLine(jev.Finding{Kind: jev.KindError, Detail: "invalid replay records"},
				name+": invalid jev replay records in the request")
		}
		transport = replay
	} else {
		key := ""
		if env.Getenv != nil {
			key = env.Getenv(jev.KeyEnv)
		}
		if key == "" {
			return pass()
		}
		transport = env.live(key)
	}
	if req.Range == "" {
		return pass()
	}
	ds := env.Decisions
	if ds == nil {
		ds = Decisions()
	}
	if AllDisabled(ds) {
		return Disabled(ds)
	}
	runner := env.Runner
	if runner == nil {
		runner = proc.Exec{WaitDelay: jev.PipeWait}
	}
	g := Git{Runner: runner, Name: env.Git, Dir: req.Repo, Env: env.Environ}
	state, err := Prepare(ctx, g, req.Range)
	if fail, ok := errors.AsType[*gitError](err); ok {
		return warnLine(jev.Finding{Kind: jev.KindError, Where: "git", Detail: fail.Error()},
			fmt.Sprintf("%s: %s: check the range %q", name, fail.Error(), req.Range))
	}
	if err != nil {
		r := &jev.Reply{}
		detail := MetaDuty + ", " + Unsupported + ": " + errPrepDeadline.Error()
		r.Note(jev.Finding{Kind: jev.KindUnassessed, Where: "range", Detail: detail},
			name+": not assessed: "+detail)
		return r.Response()
	}
	if len(state.Units) == 0 {
		return pass()
	}
	plan, err := jev.Pack(Input(Texts{req.Title, req.Body, req.Branch}, state))
	if err != nil {
		return warnLine(jev.Finding{Kind: jev.KindError, Detail: "cannot encode the request"},
			name+": cannot encode the request")
	}
	return Evaluate(ctx, plan, state.Units, transport, ds, env.Stderr)
}

func (env Env) live(key string) jev.Live {
	endpoint := env.Endpoint
	if endpoint == "" {
		endpoint = jev.Endpoint()
	}
	l := jev.Live{Endpoint: endpoint, Key: key, Client: env.Client}
	if env.Home != "" {
		l.Ledger = &jev.Ledger{
			Path: jev.LedgerPath(env.Home), Run: jev.RunID(), Extension: name, Stderr: env.Stderr,
		}
	} else {
		_, _ = fmt.Fprintf(env.Stderr, "jev: usage ledger: home directory unknown\n")
	}
	return l
}
