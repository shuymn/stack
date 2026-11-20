package main

import (
	"context"
	"os"

	"github.com/shuymn/stack/internal/cli"
	"github.com/shuymn/stack/internal/paths"
)

var version = "dev"

func main() {
	// An unknown home stays empty; commands that need it report that themselves.
	home, _ := paths.Home()
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Home:    home,
		Version: version,
	}))
}
