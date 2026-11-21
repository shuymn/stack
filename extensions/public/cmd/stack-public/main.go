// Command stack-public is the public check extension (docs/design/core-stages.md §4.4): with no
// arguments it answers one outbound request of the extension protocol; `stack-public check
// <base>..<head> [--title-file F] [--body-file F] [--branch B]` is its CLI mode.
package main

import (
	"context"
	"os"

	"github.com/shuymn/stack/extensions/public"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	os.Exit(public.Main(context.Background(), os.Args[1:], public.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Home: home,
	}))
}
