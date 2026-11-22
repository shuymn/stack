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
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
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

// Live sends batches to the API. The zero Client uses a plain http.Client; every request is bound
// by its context only. It never retries.
type Live struct {
	Endpoint string
	Key      string
	Client   *http.Client
	// Ledger, when not nil, gets one line per attempt.
	Ledger *Ledger
}

// Send implements Transport.
func (l Live) Send(ctx context.Context, b Batch) Outcome {
	start := time.Now()
	o := l.send(ctx, b)
	if l.Ledger != nil {
		l.Ledger.Append(ctx, o, time.Since(start))
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
	client := l.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return transportFailure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswer+1))
	if err != nil {
		return transportFailure(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return statusOutcome(resp.StatusCode, bytes.Contains(body, []byte(CauseMaxTokens)))
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

// statusOutcome classifies a non-200 status; mte reports that a 400 named max_tokens_exceeded.
func isTimeout(err error) bool {
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

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

// Validate checks a 200 body against the questions asked: the pinned model and, for each question,
// an answer of its type with finite probabilities in [0, 1]. Usage is optional.
func Validate(body []byte, qs []Question) Outcome {
	var doc struct {
		Model   *string                `json:"model"`
		Answers map[string]*answerJSON `json:"answers"`
		Usage   *Usage                 `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Model == nil || doc.Answers == nil {
		return Outcome{Class: Persistent, Cause: CauseMalformed}
	}
	if *doc.Model != Pin {
		return Outcome{Class: Persistent, Cause: CauseModel}
	}
	out := Outcome{Class: OK, Answers: map[string]Answer{}, Usage: doc.Usage}
	for _, q := range qs {
		a, cause := validAnswer(doc.Answers[q.Name], q)
		if cause != "" {
			return Outcome{Class: Persistent, Cause: cause}
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
