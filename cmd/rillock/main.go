// Command rillock is the Rillock control plane and CLI. All behavior lives in
// package cli; main only connects it to the real process.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jamshids/rillock/internal/cli"
)

func main() {
	os.Exit(run())
}

// run is separate from main so that deferred calls finish before os.Exit,
// which would otherwise skip them.
func run() int {
	// Ctrl-C or SIGTERM cancels ctx, which every command uses to stop cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
}
