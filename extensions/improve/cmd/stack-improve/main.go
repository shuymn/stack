// Command stack-improve is the improve extension of the cycle stage (docs/design/phase3-cycle.md
// §3.4). It reads the extension protocol request from stdin, writes one response to stdout and
// progress to stderr, and never answers block.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/shuymn/stack/extensions/improve"
)

func main() {
	os.Exit(run(context.Background(), os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	resp := respond(ctx, stdin, stderr)
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		_, _ = fmt.Fprintf(stderr, "write response: %v\n", err)
		return 1
	}
	return 0
}

func respond(ctx context.Context, stdin io.Reader, stderr io.Writer) improve.Response {
	var req improve.Request
	data, err := io.ReadAll(stdin)
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	if err != nil {
		return improve.Response{
			Status: improve.StatusWarn,
			Findings: []improve.Finding{
				{Kind: "internal-error", Where: "stdin", Detail: "cannot read the request"},
			},
		}
	}
	if req.Stage != "cycle" {
		return improve.Response{
			Status: improve.StatusPass, Context: "stack-improve runs in the cycle stage only",
			Findings: []improve.Finding{},
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return improve.Response{
			Status: improve.StatusWarn,
			Findings: []improve.Finding{
				{Kind: "internal-error", Where: "HOME", Detail: "home directory unknown"},
			},
		}
	}
	opts := improve.Options{Home: home, Stderr: stderr}
	testOptions(&opts)
	return improve.Run(ctx, opts, req)
}
