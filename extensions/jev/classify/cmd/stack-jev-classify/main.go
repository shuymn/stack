// Command stack-jev-classify is the jev-classify cycle extension (docs/design/phase4-jev.md
// §3.2): it answers one cycle request of the extension protocol.
package main

import (
	"context"
	"os"

	"github.com/shuymn/stack/extensions/jev/classify"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	os.Exit(classify.Main(context.Background(), classify.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Home: home,
		Getenv: os.Getenv, Environ: os.Environ(),
	}))
}
