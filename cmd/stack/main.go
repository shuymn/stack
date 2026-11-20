package main

import (
	"fmt"
	"io"
	"os"
)

const exitUsage = 2

var version = "dev"

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stdout, "stack: minimal orchestration CLI (see docs/architecture.md)")
		return 0
	}
	if args[0] == "--version" || args[0] == "-v" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "stack: unknown command %q\n", args[0])
	return exitUsage
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
