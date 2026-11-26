// Command platform-probe runs synthetic checks against the booking platform.
//
//	platform-probe validate -config probes.json
//	platform-probe run      -config probes.json          # once; exit 1 if any check fails
//	platform-probe serve    -config probes.json -listen :9100
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
