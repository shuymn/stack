package jev_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
	"github.com/shuymn/stack/extensions/internal/proc"
	"github.com/shuymn/stack/extensions/jev/calibrate"
	"github.com/shuymn/stack/internal/gittest"
)

const (
	classifyFixtures = "../../../fixtures/jev/classify"
	classifyTable    = "../../jev/classify/cutoffs.json"
	classifyCommand  = "TYPESAFE_API_KEY=<key> mise exec -- go run ./extensions/jev/cmd/jev-calibrate --ext classify"
)

// TestClassifyFixturesPinned is C11 for classify: every record names the pin, every body replay
// sends has a record and every record is a body the §3.5 union rebuilds, the clock leaves the keys
// alone, and the committed cutoffs, end-to-end figures and enabled bits are the refit of the
// recorded samples. A placeholder table or a pending case fails in CI and skips locally, after
// every other check.
func TestClassifyFixturesPinned(t *testing.T) {
	env := gittest.CleanEnv(os.Environ())
	cases, err := calibrate.LoadClassify(classifyFixtures)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(classifyTable)
	if err != nil {
		t.Fatal(err)
	}
	table, err := jev.ParseTable(data)
	if err != nil {
		t.Fatal(err)
	}
	qualCutoff := calibrate.UnionCutoff(table)
	fixed, err := filepath.Glob(filepath.Join(classifyFixtures, "fixed", "*.json"))
	if err != nil || len(fixed) == 0 {
		t.Fatalf("no fixed fixtures (%v)", err)
	}
	pins := func(path string, f jev.Fixture) {
		t.Helper()
		replay, err := f.RequestRecords()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for h, rec := range replay.Records {
			if rec.Answer == nil {
				continue
			}
			var a struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(rec.Answer, &a); err != nil || a.Model != jev.Pin {
				t.Fatalf("%s: record %s names %q (%v)", path, h, a.Model, err)
			}
		}
	}
	for _, p := range fixed {
		f, err := jev.ReadFixture(p)
		if err != nil {
			t.Fatal(err)
		}
		pins(p, f)
	}
	pending := 0
	for _, c := range cases {
		pins(c.Path, c.Fixture)
		bodies, err := calibrate.ClassifyBodies(t.Context(), proc.Exec{}, env, c, qualCutoff)
		if err != nil {
			t.Fatal(err)
		}
		if c.Fixture.Pending() {
			pending++
			if rec, _ := c.Fixture.RequestRecords(); len(rec.Records) != 0 ||
				c.Fixture.Samples != nil {
				t.Fatalf("%s: pending with records or samples", c.Path)
			}
		}
		// The clock, within observe's freshness, leaves every key alone.
		later := c
		later.Fixture.Request = withNow(t, c.Fixture.Request, "2026-01-08T12:30:00Z")
		again, err := calibrate.ClassifyBodies(t.Context(), proc.Exec{}, env, later, qualCutoff)
		if err != nil || !slices.Equal(again, bodies) {
			t.Fatalf("%s: another clock changed the bodies (%v)", c.Path, err)
		}
	}
	// Recorded cases hold exactly the rebuilt union; once none is pending, the table is the refit.
	if err := calibrate.VerifyClassify(t.Context(), proc.Exec{}, env, cases, table); err != nil {
		t.Fatal(err)
	}
	if table.Placeholder != "" || pending > 0 {
		msg := fmt.Sprintf(
			"not calibrated: the classify cutoff table is a placeholder (%v) and %d cal "+
				"cases are pending; from the repository root run\n\t%s",
			table.Placeholder != "",
			pending,
			classifyCommand,
		)
		if os.Getenv("CI") == "true" {
			t.Error(msg)
			return
		}
		t.Skip(msg)
	}
}

// withNow returns a fixture request with the clock of its jev object set.
func withNow(t *testing.T, request json.RawMessage, now string) json.RawMessage {
	t.Helper()
	var req map[string]json.RawMessage
	if err := json.Unmarshal(request, &req); err != nil {
		t.Fatal(err)
	}
	var j map[string]json.RawMessage
	if err := json.Unmarshal(req["jev"], &j); err != nil {
		t.Fatal(err)
	}
	j["now"] = json.RawMessage(`"` + now + `"`)
	var err error
	if req["jev"], err = jev.Marshal(j); err != nil {
		t.Fatal(err)
	}
	out, err := jev.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
