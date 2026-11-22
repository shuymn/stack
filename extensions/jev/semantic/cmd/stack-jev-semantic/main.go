// Command stack-jev-semantic is the jev-semantic outbound extension
// (docs/design/phase4-jev.md §3.2): it answers one outbound request of the extension protocol.
package main

import (
	"context"
	"os"

	"github.com/shuymn/stack/extensions/jev/semantic"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	os.Exit(semantic.Main(context.Background(), semantic.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Home: home,
		Getenv: os.Getenv, Environ: os.Environ(),
	}))
}
