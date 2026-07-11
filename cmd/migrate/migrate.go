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
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ExitSchemaAhead is the distinct exit code `migrate up` returns when a
// TF_ROLE=executor process finds the connected schema AHEAD of what its
// build understands (db.ErrExecutorSchemaAhead). Unlike Behind — a sibling
// control pod still migrating, which a restart resolves — Ahead never
// self-resolves: the fix is deploying a newer executor build first (spec
// §5.5's drain-first-on-schema-change). The container entrypoint keys its
// retry loop on this code to fail fast instead of burning its retry budget
// waiting for a condition that can't clear.
const ExitSchemaAhead = 3

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
	database, dialect, err := db.OpenForCLI()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer database.Close()
	if err := db.Migrate(database, dialect); err != nil {
		fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
		os.Exit(migrateExitCode(err))
	}
	fmt.Println("migrations applied (schema at head)")
}

// migrateExitCode maps a db.Migrate error to a process exit code. A
// never-self-resolving Ahead (an executor against a newer schema — deploy a
// newer executor first) gets the distinct ExitSchemaAhead so the entrypoint
// fails fast; a transient Behind / connection failure gets the retryable 1.
func migrateExitCode(err error) int {
	if errors.Is(err, db.ErrExecutorSchemaAhead) {
		return ExitSchemaAhead
	}
	return 1
}

func runStatus() {
	database, dialect, err := db.OpenForCLI()
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
