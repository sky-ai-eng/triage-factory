//go:build linux

package capbroker

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// shutdownDrain bounds how long runBroker waits for in-flight RPCs to
// finish once a shutdown signal arrives.
const shutdownDrain = 5 * time.Second

// runBroker is the `triagefactory cap-broker` subcommand body: create the
// socket, serve sandbox.NewHostOps() over it — "executing the same
// hostOps implementation from P0" — until SIGTERM/SIGINT, then drain and
// clean up. Blocks for the process lifetime; the orchestrator (Start in
// orchestrator.go) owns killing it.
func runBroker(args []string) error {
	fs := flag.NewFlagSet("cap-broker", flag.ContinueOnError)
	socketPath := fs.String("socket", DefaultSocketPath, "unix socket path to serve on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	l, err := listen(*socketPath)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(*socketPath) }()

	srv := NewServer(sandbox.NewHostOps())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		_ = l.Close()
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			return fmt.Errorf("capbroker: shutdown: %w", err)
		}
		<-serveErr
		return nil
	case err := <-serveErr:
		// Accept loop died on its own (not via our shutdown) — surface it.
		return err
	}
}
