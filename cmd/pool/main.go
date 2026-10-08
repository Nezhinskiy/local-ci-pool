// Command pool runs ephemeral GitHub Actions runners for the private
// repositories of one account on this Mac. This file holds only what a process
// owns: build identity, signals and the exit code. The behaviour is in run.go.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// version and commit are set at build time with
// -ldflags "-X main.version=... -X main.commit=..."; buildinfo.go falls back
// to the module's build information when they are empty.
var (
	version string
	commit  string
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	go func() {
		// After the first signal the pool drains; a second one must kill it
		// the usual way.
		<-ctx.Done()
		stop()
	}()
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, realServices())
	stop()
	os.Exit(code)
}
