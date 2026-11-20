package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/shuymn/stack/internal/cli"
	"github.com/shuymn/stack/internal/paths"
)

var version = "dev"

func main() {
	// SIGINT and SIGTERM cancel the context, so the runner kills the running child; commands that
	// must finish a step (pr merge's read-back) do so on a deadline of their own.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// An unknown home stays empty; commands that need it report that themselves.
	home, _ := paths.Home()
	code := cli.Run(ctx, os.Args[1:], cli.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Home:    home,
		Version: version,
	})
	stop()
	os.Exit(code)
}
