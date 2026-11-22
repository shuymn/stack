package powershell

import (
	"strings"
	"testing"
)

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
