//go:build improvetest

package main

import (
	"os"
	"time"

	"github.com/shuymn/stack/extensions/improve"
)

// testOptions reads the test-only wall clock (built only with the improvetest tag).
func testOptions(o *improve.Options) {
	if t, err := time.Parse(time.RFC3339, os.Getenv("STACK_IMPROVE_NOW")); err == nil {
		o.Now = func() time.Time { return t }
		o.Location = time.UTC
	}
}
