// Package instance is the CLI entrypoint for the `triagefactory instance`
// subcommand — the fleet registry's operator surface (TFAC-586):
//
//	triagefactory instance list              show every registered instance
//	triagefactory instance drain <id>        stop new claims, quiesce
//	triagefactory instance undrain <id>      resume claims
//
// Shell access is the operator boundary here, same as `jwk-init` — the
// API/UI drain button is a separate ticket (TFAC-589). Works in local mode
// too: the `instances` row exists there (N=1), and a single-user machine
// has no operator gating to speak of. The subcommand opens its own DB
// handle (db.OpenForCLI), independent of a running server process — it
// writes the same instances.draining column the running instance's own
// heartbeat reads back within one interval (see internal/delegate's
// heartbeatOnce), so no IPC to a live process is needed.
package instance

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// Handle dispatches the instance subcommand.
func Handle(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		runList()
	case "drain":
		runSetDraining(args[1:], true)
	case "undrain":
		runSetDraining(args[1:], false)
	case "placement":
		runPlacement(args[1:])
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown instance subcommand %q\n\n", args[0])
		printUsage()
		os.Exit(2)
	}
}

// openInstanceStore opens the DB the running server would use for the
// current runmode and wraps it in the narrow db.InstanceStore constructor —
// no need for the full db.Stores bundle (secrets, app pool, ...) just to
// reach the fleet registry. Callers must close the returned *sql.DB.
func openInstanceStore() (store db.InstanceStore, closeFn func() error) {
	conn, dialect, err := db.OpenForCLI()
	if err != nil {
		fail("%v", err)
	}
	switch dialect {
	case "postgres":
		return pgstore.NewInstanceStore(conn), conn.Close
	default:
		return sqlitestore.NewInstanceStore(conn), conn.Close
	}
}

func runList() {
	store, closeFn := openInstanceStore()
	defer func() { _ = closeFn() }()

	rows, err := store.List(context.Background())
	if err != nil {
		fail("list instances: %v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no registered instances")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tROLE\tVERSION\tEPOCH\tDRAINING\tRUNS\tHEARTBEAT")
	for _, r := range rows {
		runs := "-"
		if r.ActiveRuns != nil && r.MaxRuns != nil {
			runs = fmt.Sprintf("%d/%d", *r.ActiveRuns, *r.MaxRuns)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%t\t%s\t%s\n",
			r.ID, r.Role, r.Version, r.BootEpoch, r.Draining, runs, heartbeatAge(r.LastHeartbeatAt))
	}
	_ = w.Flush()
}

// heartbeatAge renders how long ago an instance last heartbeat, so an
// operator can eyeball liveness without doing timestamp math. Clamps a
// negative gap (last in the future — clock skew between the DB server and
// this CLI's host, not a real state) to "just now" rather than printing a
// confusing negative duration like "-5s ago".
func heartbeatAge(last time.Time) string {
	if last.IsZero() {
		return "never"
	}
	age := time.Since(last)
	if age < 0 {
		return "just now"
	}
	return age.Round(time.Second).String() + " ago"
}

func runSetDraining(args []string, draining bool) {
	verb := "drain"
	if !draining {
		verb = "undrain"
	}
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintf(os.Stderr, "usage: triagefactory instance %s <id>\n", verb)
		fmt.Fprintln(os.Stderr, "run `triagefactory instance list` to see registered instance ids")
		os.Exit(2)
	}
	id := args[0]

	store, closeFn := openInstanceStore()
	defer func() { _ = closeFn() }()

	matched, err := store.SetDraining(context.Background(), id, draining)
	if err != nil {
		fail("%s %s: %v", verb, id, err)
	}
	if !matched {
		fail("no registered instance with id %q — run `triagefactory instance list` to see registered ids", id)
	}

	if draining {
		fmt.Printf("instance %s is now draining: it will stop claiming new runs (existing runs finish or hibernate-on-idle); check `triagefactory instance list` for RUNS reaching 0/N before retiring it.\n", id)
	} else {
		fmt.Printf("instance %s is no longer draining: it will resume claiming new runs on its next heartbeat.\n", id)
	}
}

func printUsage() {
	fmt.Println(`triagefactory instance — fleet registry operator commands.

USAGE
  triagefactory instance list              show every registered instance
  triagefactory instance drain <id>        stop new claims, quiesce
  triagefactory instance undrain <id>      resume claims
  triagefactory instance placement ...     inspect / steer run placement
                                           (run "instance placement --help")

NOTES
  Draining takes effect within one heartbeat interval (a few seconds) of
  the running instance itself, not immediately on this command's return —
  it writes instances.draining, which the instance reads back on its next
  heartbeat. A drained instance's live runs finish (or hibernate-on-idle);
  no new claims start. Safe to retire once RUNS reaches 0.

  Local mode: the single instance's row still exists (N=1) — drain works
  the same way, useful for a controlled shutdown ahead of an upgrade.`)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory instance: "+format+"\n", args...)
	os.Exit(1)
}
