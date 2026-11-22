// Package jev is the shared client of the Jev extensions (docs/design/phase4-jev.md §3.3):
// transport and answer validation, packing, replay records, the usage ledger, the protocol
// response and the cutoff math. State building stays with each extension.
package jev

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Pin is the only model the extensions send; no configuration overrides it (§3.5).
const Pin = "jev-1.14.0"

// KeyEnv names the variable whose non-empty value opts an extension in (§3.1).
const KeyEnv = "TYPESAFE_API_KEY"

// Provider limits that shape batches (§3.1).
const (
	// StateBudget bounds a request's encoded state: the fixed fields plus its units.
	StateBudget = 48 << 10
	// QuestionBudget bounds a request's encoded questions; more go to another request.
	QuestionBudget = 16 << 10
	// MinUnitBudget is the smallest unit budget packing splits under.
	MinUnitBudget = 1 << 10
	// MaxAnswer bounds the answer body read; a longer one is a persistent failure.
	MaxAnswer = 1 << 20
	// InFlight is how many requests are sent at once.
	InFlight = 4
)

// Child process bounds inside a work deadline (§3.1).
const (
	// PipeWait is how long a child's pipes may outlive it.
	PipeWait = 200 * time.Millisecond
	// ChildMargin ends a child's timeout this long before the work deadline.
	ChildMargin = 200 * time.Millisecond
)

// endpoint is the API URL. Only test builds change it (`-ldflags -X`); it is never configuration.
var endpoint = "https://api.typesafe.ai/v1/systemone"

// Endpoint returns the API URL this binary was built with.
func Endpoint() string { return endpoint }

// Hash is the replay record key of a sent body: "sha256:" and its hex digest.
func Hash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
