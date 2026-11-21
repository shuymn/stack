//go:build observetest

package main

import (
	"os"
	"time"

	"github.com/shuymn/stack/extensions/observe"
)

// testOptions reads the test-only budget and clock (built only with the observetest tag).
func testOptions(o *observe.Options) {
	if d, err := time.ParseDuration(os.Getenv("STACK_OBSERVE_BUDGET")); err == nil {
		o.Budget = d
	}
	if d, err := time.ParseDuration(os.Getenv("STACK_OBSERVE_RESERVE")); err == nil {
		o.Reserve = d
	}
	if t, err := time.Parse(time.RFC3339, os.Getenv("STACK_OBSERVE_NOW")); err == nil {
		o.Now = func() time.Time { return t }
	}
}
