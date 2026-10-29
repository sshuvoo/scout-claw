package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"scoutclaw/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Execute(ctx))
}
