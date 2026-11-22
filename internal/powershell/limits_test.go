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
	sep := string(filepath.Separator)
	// parse parses cmd and checks that it allocated a few KB per call at most: quadratic cwd
	// copies are gigabytes.
	parse := func(cmd, cwd, home string, calls int) Result {
		t.Helper()
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		r := Parse(cmd, cwd, home, Unix)
		runtime.ReadMemStats(&after)
		if r.Err != nil || len(r.Calls) != calls {
			t.Fatalf("err %v, %d calls; want %d", r.Err, len(r.Calls), calls)
		}
		//nolint:gosec // G115: a call count is never negative.
		budget := 4096*uint64(calls) + 64<<10
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > budget {
			t.Errorf("Parse allocated %d bytes for %d bytes of input", alloc, len(cmd))
		}
		return r
	}

	t.Run("step cap", func(t *testing.T) {
		const n = 50000
		r := parse(strings.Repeat("cd x && ", n)+"git status", "/w", "", n+1)
		if want := "/w" + strings.Repeat(sep+"x", maxCdSteps); r.Calls[maxCdSteps].Cwd != want {
			t.Errorf("cwd after %d steps = %q, want %q", maxCdSteps, r.Calls[maxCdSteps].Cwd, want)
		}
		if got := r.Calls[maxCdSteps+1].Cwd; got != "" {
			t.Errorf("cwd after %d steps = %q, want unknown", maxCdSteps+1, got)
		}
		if last := r.Calls[n]; last.Cwd != "" {
			t.Errorf("cwd after %d steps = %.40q…; want unknown", n, last.Cwd)
		}
	})

	t.Run("length cap", func(t *testing.T) {
		short, long := strings.Repeat("a", 100), strings.Repeat("b", maxCwdLen)
		// `&&` keeps the cwd known up to the long cd, so only the byte cap can make it unknown.
		r := parse("cd "+short+" && git a && cd "+long+" && git b", "/w", "", 4)
		if want := "/w" + sep + short; r.Calls[1].Cwd != want {
			t.Errorf("cwd after a short cd = %q, want %q", r.Calls[1].Cwd, want)
		}
		if got := r.Calls[3].Cwd; got != "" {
			t.Errorf("cwd past %d bytes = %.40q…; want unknown", maxCwdLen, got)
		}
	})

	t.Run("long home", func(t *testing.T) {
		const n = 20000
		home := "/" + strings.Repeat("x/", n) + "root"
		r := parse(strings.Repeat("cd ~/x && ", n)+"git status", "/w", home, n+1)
		if last := r.Calls[n]; last.Cwd != "" {
			t.Errorf("cwd under a %d-byte home = %.40q…; want unknown", len(home), last.Cwd)
		}
	})
}

// TestParseCwdBudgetInBlocks checks that the lookahead over a block uses a cd-step budget of its
// own and restores the caller's: an inert block leaves the budget whole, and an executed one counts
// its steps once.
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
