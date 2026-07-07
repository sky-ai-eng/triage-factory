// Package migrate is the CLI entrypoint for the `triagefactory migrate`
// subcommand. It exposes the operator-facing slice of goose:
//
//	triagefactory migrate up      bring the schema to head
//	triagefactory migrate status  show applied / pending versions
//
// Down migrations are intentionally not exposed — installed user-tools
// shouldn't ship a footgun for downgrade-induced data loss.
//
// The subcommand opens the same SQLite path the server does so an
// operator can run `triagefactory migrate status` against an existing
// install without spinning up the HTTP stack.
package migrate

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// openTarget returns the (db, dialect) pair for the current runmode.
// Local mode opens the same SQLite file the server uses; multi mode
// opens TF_DATABASE_URL via pgx. This is the only path through the
// CLI that touches a Postgres handle today — the server side is
// stubbed by main.go pending the rest of the multi-mode wiring,
// but the migrate subcommand needs to work standalone so the Docker
// image's entrypoint can bring the schema to head before the server
// process starts.
func openTarget() (*sql.DB, string, error) {
	if runmode.Current() == runmode.ModeMulti {
		dsn := os.Getenv("TF_DATABASE_URL")
		if dsn == "" {
			return nil, "", fmt.Errorf("TF_MODE=multi requires TF_DATABASE_URL")
		}
		conn, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, "", fmt.Errorf("open postgres: %w", err)
		}
		if err := conn.Ping(); err != nil {
			conn.Close()
			return nil, "", fmt.Errorf("ping postgres: %w", err)
		}
		return conn, "postgres", nil
	}
	conn, err := db.Open()
	if err != nil {
		return nil, "", fmt.Errorf("open database: %w", err)
	}
	return conn, "sqlite3", nil
}

// Handle is the entrypoint dispatched from main.go on
// `triagefactory migrate ...`. The first argv after `migrate` is the
// sub-subcommand; anything else falls through to a usage print so
// operators get a quick reference rather than a silent no-op.
func Handle(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "up":
		runUp()
	case "status":
		runStatus()
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown migrate subcommand %q\n\n", args[0])
		printUsage()
		os.Exit(2)
	}
}

func runUp() {
	database, dialect, err := openTarget()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()
	if err := db.Migrate(database, dialect); err != nil {
		fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("migrations applied (schema at head)")
}

func runStatus() {
	database, dialect, err := openTarget()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()
	if err := db.MigrationStatus(database, dialect, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "migrate status: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`triagefactory migrate — schema migration ops.

USAGE
  triagefactory migrate up        bring the schema to head
  triagefactory migrate status    list applied + pending migrations

NOTES
  Down migrations are intentionally not exposed; for installed
  user-tools, downgrade-induced data loss is a footgun without a
  matching upside.`)
}
