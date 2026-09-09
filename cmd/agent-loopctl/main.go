package main

import (
	"context"
	"github.com/ishii1648/codex-issue-loop/internal/application/hostcli"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case sig := <-signals:
			cancel(hostcli.Interrupted{Signal: sig})
		case <-ctx.Done():
		}
	}()
	defer cancel(nil)
	os.Exit((hostcli.App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}).Run(ctx, os.Args[1:]))
}
