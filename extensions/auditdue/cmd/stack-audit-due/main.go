// Command stack-audit-due is the audit-due extension of the merged stage
// (docs/design/core-stages.md §4.7). It reads the extension protocol request from stdin and
// writes one response to stdout; it never answers block.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/shuymn/stack/extensions/auditdue"
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

func run(stdin io.Reader, stdout, stderr io.Writer) int {
	home, _ := os.UserHomeDir()
	resp := auditdue.Respond(stdin, home)
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		_, _ = fmt.Fprintf(stderr, "write response: %v\n", err)
		return 1
	}
	return 0
}
