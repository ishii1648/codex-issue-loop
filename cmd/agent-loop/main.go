package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ishii1648/codex-issue-loop/internal/app"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code := (app.App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}).Run(ctx, os.Args[1:])
	os.Exit(code)
}
