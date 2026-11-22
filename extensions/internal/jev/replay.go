package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// ErrRecords reports a request's jev object that is not a valid set of replay records.
var ErrRecords = errors.New("invalid jev replay records")

var hashRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Record is the recorded outcome of one body: its 200 answer, or its non-200 status (with
// error "max_tokens_exceeded" for that 400).
type Record struct {
	Answer json.RawMessage `json:"answer,omitempty"`
	Status int             `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Replay answers each batch from the records keyed by its body hash and never calls the network.
type Replay struct {
	Records map[string]Record
}

// ParseReplay decodes a request's jev object, `{"records": {"sha256:<hex>": Record}}`. Each record
// holds an answer or a non-200 status, not both.
func ParseReplay(raw json.RawMessage) (Replay, error) {
	var doc struct {
		Records map[string]Record `json:"records"`
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Replay{}, fmt.Errorf("%w: not an object", ErrRecords)
	}
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return Replay{}, fmt.Errorf("%w: %w", ErrRecords, err)
	}
	if doc.Records == nil {
		return Replay{}, fmt.Errorf("%w: no records object", ErrRecords)
	}
	for k, r := range doc.Records {
		if !hashRe.MatchString(k) {
			return Replay{}, fmt.Errorf("%w: key %q", ErrRecords, k)
		}
		hasAnswer := len(bytes.TrimSpace(r.Answer)) > 0 && !bytes.Equal(r.Answer, []byte("null"))
		if hasAnswer == (r.Status != 0) || r.Status == http.StatusOK {
			return Replay{}, fmt.Errorf("%w: %s: want an answer or a non-200 status", ErrRecords, k)
		}
	}
	return Replay{Records: doc.Records}, nil
}

// Send implements Transport.
func (r Replay) Send(_ context.Context, b Batch) Outcome {
	rec, ok := r.Records[b.Hash]
	if !ok {
		return Outcome{Class: Unrecorded, Cause: CauseUnrecorded}
	}
	return rec.Outcome(b.Questions)
}

// Outcome is the outcome the record replays for questions.
func (rec Record) Outcome(qs []Question) Outcome {
	if rec.Status != 0 {
		return statusOutcome(rec.Status, rec.Error == CauseMaxTokens)
	}
	if len(rec.Answer) > MaxAnswer {
		return Outcome{Class: Persistent, Status: http.StatusOK, Cause: CauseAnswerTooLong}
	}
	o := Validate(rec.Answer, qs)
	o.Status, o.Body = http.StatusOK, rec.Answer
	return o
}
