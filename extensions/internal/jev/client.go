package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Class is how a request ended (§3.1).
type Class int

// Classes of outcomes.
const (
	// OK carries validated answers.
	OK Class = iota
	// Transient is a connect, TLS or timeout error, 429, 529 or another 5xx: unassessed.
	Transient
	// TooLarge is a 400 max_tokens_exceeded or a 413: unassessed.
	TooLarge
	// Deadline is a request not sent, or cut, by the work deadline: unassessed.
	Deadline
	// Persistent is any other failure the owner must act on: warn jev-error.
	Persistent
	// Unrecorded is a replayed body with no record: warn jev-unrecorded.
	Unrecorded
)

// Unassessed reports a class whose questions are unassessed rather than failed.
func (c Class) Unassessed() bool {
	return c == Transient || c == TooLarge || c == Deadline
}

// Fixed causes; a cause never quotes a response body, which can quote input.
const (
	CauseTimeout       = "timeout"
	CauseNetwork       = "network error"
	CauseDeadline      = "work deadline"
	CauseMaxTokens     = "max_tokens_exceeded" //nolint:gosec // G101: an API error name, not a credential
	CauseAnswerTooLong = "answer over 1 MiB"
	CauseMalformed     = "malformed answer"
	CauseModel         = "answered by another model"
	CauseMissing       = "missing or wrongly typed answer"
	CauseProbability   = "probability not in [0, 1]"
	CauseUnrecorded    = "no recorded answer for this body"
	CauseBadRecord     = "invalid replay record"
)

// Outcome is the result of one request.
type Outcome struct {
	Class   Class
	Answers map[string]Answer
	Usage   *Usage
	// Status is the HTTP status when one was received (or recorded).
	Status int
	// Cause is a fixed string or "HTTP <status>"; empty for OK.
	Cause string
	// Invalid maps each question whose answer is missing or invalid in an otherwise valid
	// answer to its fixed cause.
	Invalid map[string]string
	// Body is the 200 body an OK or persistent outcome was validated from, for jev-calibrate's
	// records; never shown.
	Body []byte
}

// Answer is one validated answer.
type Answer struct {
	Type          string
	Noul          float64
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

// Usage is the token usage of an answer.
//
//nolint:tagliatelle // the API defines the keys in snake_case.
type Usage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

// Transport sends one batch.
type Transport interface {
	Send(ctx context.Context, b Batch) Outcome
}

// Do sends the batches, at most InFlight at once, and returns their outcomes in batch order.
// A batch not started by ctx's end is Deadline, without being sent.
func Do(ctx context.Context, t Transport, batches []Batch) []Outcome {
	out := make([]Outcome, len(batches))
	sem := make(chan struct{}, InFlight)
	var wg sync.WaitGroup
	for i, b := range batches {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			out[i] = Outcome{Class: Deadline, Cause: CauseDeadline}
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			out[i] = t.Send(ctx, b)
		})
	}
	wg.Wait()
	return out
}

// Live sends batches to the API in one RoundTrip each: redirects are answers, never followed. A nil
// Client, or one without a Transport, uses http.DefaultTransport; a Client.Timeout bounds each
// request as a transient timeout. Every request is also bound by its context. It never retries.
type Live struct {
	Endpoint string
	Key      string
	Client   *http.Client
	// Ledger, when not nil, gets one line per attempt.
	Ledger *Ledger
}

// Send implements Transport. The request ends LedgerMargin before ctx's deadline, leaving that
// margin for the ledger append of a request cut by the deadline.
func (l Live) Send(ctx context.Context, b Batch) Outcome {
	hctx := ctx
	if d, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		hctx, cancel = context.WithDeadline(ctx, d.Add(-LedgerMargin))
		defer cancel()
	}
	if hctx.Err() != nil {
		// Never sent, so not an attempt: no ledger line.
		return Outcome{Class: Deadline, Cause: CauseDeadline}
	}
	start := time.Now()
	o := l.send(hctx, b)
	if l.Ledger != nil {
		l.Ledger.Append(o, time.Since(start))
	}
	return o
}

func (l Live) send(ctx context.Context, b Batch) Outcome {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		l.Endpoint,
		bytes.NewReader(b.Body),
	)
	if err != nil {
		return Outcome{Class: Persistent, Cause: "invalid endpoint"}
	}
	req.Header.Set("Authorization", "Bearer "+l.Key)
	req.Header.Set("Content-Type", "application/json")
	// One round trip: a 3xx, even with a Location the client could not parse, is the answer.
	rt := http.DefaultTransport
	if l.Client != nil {
		if l.Client.Transport != nil {
			rt = l.Client.Transport
		}
		if l.Client.Timeout > 0 {
			// A client timeout is transient; only ctx's own end is the work deadline.
			rctx, cancel := context.WithTimeout(ctx, l.Client.Timeout)
			defer cancel()
			req = req.WithContext(rctx)
		}
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return transportFailure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		// Only a 400 needs its body: max_tokens_exceeded is too large, any other 400 persistent.
		// A read that ends early keeps the status and still checks the bytes that arrived.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return statusOutcome(resp.StatusCode, bytes.Contains(body, []byte(CauseMaxTokens)))
	default:
		// Classified at the headers: a stalled error body cannot turn it into a timeout.
		return statusOutcome(resp.StatusCode, false)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswer+1))
	if err != nil {
		return transportFailure(ctx, err)
	}
	if len(body) > MaxAnswer {
		return Outcome{Class: Persistent, Status: resp.StatusCode, Cause: CauseAnswerTooLong}
	}
	o := Validate(body, b.Questions)
	o.Status, o.Body = resp.StatusCode, body
	return o
}

func transportFailure(ctx context.Context, err error) Outcome {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Outcome{Class: Deadline, Cause: CauseDeadline}
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return Outcome{Class: Transient, Cause: CauseTimeout}
	default:
		return Outcome{Class: Transient, Cause: CauseNetwork}
	}
}

func isTimeout(err error) bool {
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

// statusOutcome classifies a non-200 status; mte reports that a 400 named max_tokens_exceeded.
func statusOutcome(status int, mte bool) Outcome {
	cause := "HTTP " + strconv.Itoa(status)
	switch {
	case status == http.StatusBadRequest && mte:
		return Outcome{Class: TooLarge, Status: status, Cause: CauseMaxTokens}
	case status == http.StatusRequestEntityTooLarge:
		return Outcome{Class: TooLarge, Status: status, Cause: cause}
	case status == http.StatusTooManyRequests || status == 529 || status >= 500:
		return Outcome{Class: Transient, Status: status, Cause: cause}
	default:
		return Outcome{Class: Persistent, Status: status, Cause: cause}
	}
}

type answerJSON struct {
	Type          *string             `json:"type"`
	Noul          *float64            `json:"noul"`
	Choice        *string             `json:"choice"`
	Confidence    *float64            `json:"confidence"`
	Probabilities map[string]*float64 `json:"probabilities"`
}

// Validate checks a 200 body against the questions asked. A body that is not the §2 envelope or
// names another model is persistent; otherwise each question's answer is checked alone: a valid
// one is kept, a missing, wrongly typed or out-of-range one goes to Invalid with its cause. Usage
// is kept whenever it parses, with an absent count unknown.
func Validate(body []byte, qs []Question) Outcome {
	var doc struct {
		Model   *string                    `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   *Usage                     `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		var u struct {
			Usage *Usage `json:"usage"`
		}
		_ = json.Unmarshal(body, &u)
		return Outcome{Class: Persistent, Cause: CauseMalformed, Usage: u.Usage}
	}
	if doc.Model == nil || doc.Answers == nil {
		return Outcome{Class: Persistent, Cause: CauseMalformed, Usage: doc.Usage}
	}
	if *doc.Model != Pin {
		return Outcome{Class: Persistent, Cause: CauseModel, Usage: doc.Usage}
	}
	out := Outcome{Class: OK, Answers: map[string]Answer{}, Usage: doc.Usage}
	for _, q := range qs {
		// Each answer is decoded alone: a wrongly typed field fails only its question.
		var raw *answerJSON
		cause := ""
		if err := json.Unmarshal(doc.Answers[q.Name], &raw); doc.Answers[q.Name] != nil &&
			err != nil {
			cause = CauseMissing
		}
		a, vcause := validAnswer(raw, q)
		if cause == "" {
			cause = vcause
		}
		if cause != "" {
			if out.Invalid == nil {
				out.Invalid = map[string]string{}
			}
			out.Invalid[q.Name] = cause
			continue
		}
		out.Answers[q.Name] = a
	}
	return out
}

func validAnswer(a *answerJSON, q Question) (Answer, string) {
	if a == nil || a.Type == nil || *a.Type != q.Type {
		return Answer{}, CauseMissing
	}
	switch q.Type {
	case TypeNoul:
		if a.Noul == nil {
			return Answer{}, CauseMissing
		}
		if !probability(*a.Noul) {
			return Answer{}, CauseProbability
		}
		return Answer{Type: q.Type, Noul: *a.Noul}, ""
	case TypeChoice:
		if a.Choice == nil || a.Confidence == nil || a.Probabilities == nil ||
			!slices.Contains(q.Labels, *a.Choice) {
			return Answer{}, CauseMissing
		}
		if !probability(*a.Confidence) {
			return Answer{}, CauseProbability
		}
		ans := Answer{
			Type: q.Type, Choice: *a.Choice, Confidence: *a.Confidence,
			Probabilities: map[string]float64{},
		}
		for _, l := range q.Labels {
			p := a.Probabilities[l]
			if p == nil {
				return Answer{}, CauseMissing
			}
			if !probability(*p) {
				return Answer{}, CauseProbability
			}
			ans.Probabilities[l] = *p
		}
		return ans, ""
	default:
		return Answer{}, CauseMissing
	}
}

func probability(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1
}

// String names the outcome for the ledger: ok, max_tokens_exceeded, transient or error.
func (o Outcome) String() string {
	switch o.Class {
	case OK:
		if len(o.Invalid) > 0 {
			return "error"
		}
		return "ok"
	case TooLarge:
		return "max_tokens_exceeded"
	case Transient, Deadline:
		return "transient"
	case Persistent, Unrecorded:
		return "error"
	default:
		return "error"
	}
}

// Describe is the outcome's fixed description for findings: the cause, with the status for a
// cause that does not name it.
func (o Outcome) Describe() string {
	if o.Status != 0 && o.Status != http.StatusOK && o.Cause != "HTTP "+strconv.Itoa(o.Status) {
		return fmt.Sprintf("HTTP %d %s", o.Status, o.Cause)
	}
	return o.Cause
}
