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
			t.Errorf("%s: err %v, %d calls; want the nesting cap and no calls", name, r.Err, len(r.Calls))
		}
	}
}
