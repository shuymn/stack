// Command stack-observe is the observe extension of the cycle stage (docs/design/phase3-cycle.md
// §3.3). It reads the extension protocol request from stdin, writes one response to stdout and
// progress to stderr, and never answers block.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/shuymn/stack/extensions/observe"
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

func respond(ctx context.Context, stdin io.Reader, stderr io.Writer) observe.Response {
	var req struct {
		Stage string `json:"stage"`
	}
	data, err := io.ReadAll(stdin)
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	if err != nil {
		return observe.Response{
			Status: observe.StatusWarn,
			Findings: []observe.Finding{
				{
					Kind:   "internal-error",
					Where:  "stdin",
					Detail: "cannot read the request: " + err.Error(),
				},
			},
		}
	}
	if req.Stage != "cycle" {
		return observe.Response{
			Status: observe.StatusPass, Context: "stack-observe runs in the cycle stage only",
			Findings: []observe.Finding{},
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return observe.Response{
			Status: observe.StatusWarn,
			Findings: []observe.Finding{
				{
					Kind:   "latest-not-written",
					Where:  "HOME",
					Detail: "home directory unknown: " + err.Error(),
				},
			},
		}
	}
	opts := observe.Options{Home: home, Stderr: stderr}
	testOptions(&opts)
	return observe.Run(ctx, opts)
}
