package powershell

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestParseCwdGrowth checks that a long `cd x &&` chain allocates in proportion to its length:
// past the cap on cd steps the cwd is unknown, and recognition goes on.
func TestParseCwdGrowth(t *testing.T) {
	const n = 50000
	cmd := strings.Repeat("cd x && ", n) + "git status"
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := Parse(cmd, "/w", "", Unix)
	runtime.ReadMemStats(&after)
	if r.Err != nil || len(r.Calls) != n+1 {
		t.Fatalf("err %v, %d calls; want %d", r.Err, len(r.Calls), n+1)
	}
	if last := r.Calls[n]; last.Cwd != "" {
		t.Errorf("cwd after %d cd steps = %.40q…; want unknown", n, last.Cwd)
	}
	if want := "/w" + string(filepath.Separator) + "x"; r.Calls[1].Cwd != want {
		t.Errorf("cwd after one cd = %q, want %q", r.Calls[1].Cwd, want)
	}
	// A few KB per call covers the calls and their slice growth; quadratic cwd copies are gigabytes.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4096*uint64(n) {
		t.Errorf("Parse allocated %d bytes for %d bytes of input", alloc, len(cmd))
	}
}

// TestParseCwdBudgetInBlocks checks that the lookahead over a block spends no cd steps of its own:
// an inert block leaves the budget whole, and an executed one counts its steps once.
func TestParseCwdBudgetInBlocks(t *testing.T) {
	sep := string(filepath.Separator)
	gitCwd := func(cmd string) string {
		t.Helper()
		r := Parse(cmd, "/w", "", Unix)
		for _, c := range r.Calls {
			if c.Is("git") {
				return c.Cwd
			}
		}
		t.Fatalf("no git call in %q (err %v)", cmd, r.Err)
		return ""
	}
	inert := "$p = { " + strings.Repeat("cd x && ", maxCdSteps) + "'done' }; cd sub && git status"
	if got, want := gitCwd(inert), "/w"+sep+"sub"; got != want {
		t.Errorf("after an inert block: git cwd = %q, want %q", got, want)
	}
	executed := "& { " + strings.Repeat("cd x && ", 20) + "git status }"
	if got, want := gitCwd(executed), "/w"+strings.Repeat(sep+"x", 20); got != want {
		t.Errorf("inside an executed block: git cwd = %q, want %q", got, want)
	}
}

// TestParseDeepNesting checks that every recursion path spends the one nesting budget, so a hostile
// command fails open (no calls) instead of growing the stack with its length.
func TestParseDeepNesting(t *testing.T) {
	const n = 100000
	for name, cmd := range map[string]string{
		"hash values":        strings.Repeat("@{x=", n) + "0" + strings.Repeat("}", n),
		"chained assignment": strings.Repeat("$x=", n) + "0",
		"parens":             strings.Repeat("(", n) + "gh x" + strings.Repeat(")", n),
		"brackets":           strings.Repeat("[", n) + strings.Repeat("]", n),
		"invoked blocks":     strings.Repeat("& { ", n) + "gh x" + strings.Repeat(" }", n),
	} {
		if r := Parse(cmd, "", "", Unix); r.Err == nil || len(r.Calls) != 0 {
			t.Errorf(
				"%s: err %v, %d calls; want the nesting cap and no calls",
				name,
				r.Err,
				len(r.Calls),
			)
		}
	}
}
