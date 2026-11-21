// Command stack-guard is the bundled Claude Code PreToolUse hook of the safety rules
// (docs/design/core-stages.md §4.5), run as `stack-guard hook claude pre-tool-use`.
package main

import (
	"os"

	"github.com/shuymn/stack/extensions/guard"
	"github.com/shuymn/stack/internal/paths"
)

func main() {
	home, err := paths.Home()
	if err != nil {
		home = ""
	}
	os.Exit(guard.Main(os.Args[1:], guard.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Home: home,
	}))
}
