package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/cmd/install"
	"github.com/sky-ai-eng/triage-factory/internal/app"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Version is the binary's release tag, set by the linker at build time
// (`-ldflags "-X main.Version=v0.1.0"`). Local builds without that flag
// see the literal "dev", so anything in the wild claiming to be "dev" is
// known to be unreleased.
var Version = "dev"

func main() {
	if err := run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "triagefactory:", err)
		os.Exit(1)
	}
}

// run is the real entrypoint. It initializes the runtime mode, dispatches
// CLI subcommands, and otherwise boots the server — returning an error
// instead of calling log.Fatal so deferred cleanup runs and the boot path
// is testable.
func run(ctx context.Context, args []string) error {
	// Initialize the runtime mode flag (TF_MODE env, default local) before
	// anything touches a path or opens a DB.
	if err := runmode.InitFromEnv(); err != nil {
		return fmt.Errorf("runmode: %w", err)
	}

	// CLI subcommands short-circuit before any server wiring: exec/status
	// are used by delegated Claude Code agents; resume/install/uninstall/
	// migrate/jwk-init are user-facing.
	if handled, err := dispatchCLI(args[1:]); handled {
		return err
	}

	// Server mode. Translate SIGINT/SIGTERM into context cancellation so the
	// HTTP server and background workers shut down gracefully.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := app.LoadConfig(args[1:])
	if err != nil {
		return err
	}
	static, err := frontendDist() // go:embed lives in package main (embed.go)
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}

	a, err := app.New(ctx, cfg, static)
	if err != nil {
		return err
	}
	defer a.Close()

	fmt.Printf("Triage Factory running at %s\n", cfg.BrowserURL)
	// One-shot PATH hint so `triagefactory resume` works from any terminal.
	install.HintIfMissing()
	if !cfg.NoBrowser {
		openBrowser(cfg.BrowserURL)
	}

	return a.Run(ctx)
}
