// Package instance is the CLI entrypoint for the `triagefactory instance`
// subcommand — the fleet registry's operator surface:
//
//	triagefactory instance list [--json]     show every registered instance
//	triagefactory instance drain [--until-restart] <id>
//	                                         stop new claims, quiesce
//	triagefactory instance undrain <id>      resume claims
//
// Shell access is the operator boundary here, same as `jwk-init`; the fleet
// console's drain control is the other surface. Works in local mode
// too: the `instances` row exists there (N=1), and a single-user machine
// has no operator gating to speak of. The subcommand opens its own DB
// handle (db.OpenForCLI), independent of a running server process — it
// writes the drain columns the running instance's own heartbeat reads back
// within one interval (see internal/delegate's heartbeatOnce), so no IPC to
// a live process is needed.
package instance

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Handle dispatches the instance subcommand.
func Handle(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		runList(args[1:])
	case "drain":
		runDrain(args[1:])
	case "undrain":
		runUndrain(args[1:])
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

func runList(args []string) {
	fset := flag.NewFlagSet("instance list", flag.ContinueOnError)
	asJSON := fset.Bool("json", false, "print the instances as JSON")
	parseArgs(fset, args, "usage: triagefactory instance list [--json]")
	if fset.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: triagefactory instance list [--json]")
		os.Exit(2)
	}

	store, closeFn := openInstanceStore()
	defer func() { _ = closeFn() }()

	rows, err := store.List(context.Background())
	if err != nil {
		fail("list instances: %v", err)
	}
	if *asJSON {
		if err := writeListJSON(os.Stdout, rows, time.Now()); err != nil {
			fail("write instances: %v", err)
		}
		return
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			r.ID, r.Role, r.Version, r.BootEpoch, drainingLabel(r), runs, heartbeatAge(r.LastHeartbeatAt))
	}
	_ = w.Flush()
}

// The drain values `instance list --json` reports.
const (
	drainNone         = "none"
	drainPersistent   = "persistent"
	drainUntilRestart = "until_restart"
)

// listRow is one instance in `instance list --json`. `deploy upgrade` reads
// this shape, from whatever version of the binary the control container
// runs, to find the executors to drain and wait on, so its field names are
// a contract.
type listRow struct {
	ID         string `json:"id"`
	Role       string `json:"role"`
	Version    string `json:"version"`
	Drain      string `json:"drain"`
	ActiveRuns *int   `json:"active_runs"`
	// HeartbeatAgeSeconds is how long ago the instance last heartbeat, on
	// this host's clock against the database's timestamp, never negative.
	HeartbeatAgeSeconds float64 `json:"heartbeat_age_seconds"`
}

// writeListJSON writes rows as a JSON array, one listRow each.
func writeListJSON(w io.Writer, rows []domain.Instance, now time.Time) error {
	out := make([]listRow, 0, len(rows))
	for _, r := range rows {
		age := now.Sub(r.LastHeartbeatAt).Seconds()
		if age < 0 {
			age = 0
		}
		out = append(out, listRow{
			ID:                  r.ID,
			Role:                r.Role,
			Version:             r.Version,
			Drain:               drainKind(r),
			ActiveRuns:          r.ActiveRuns,
			HeartbeatAgeSeconds: age,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func drainKind(r domain.Instance) string {
	switch {
	case !r.Draining:
		return drainNone
	case r.DrainUntilRestart:
		return drainUntilRestart
	}
	return drainPersistent
}

// drainingLabel is the DRAINING column of the human-readable list.
func drainingLabel(r domain.Instance) string {
	switch drainKind(r) {
	case drainUntilRestart:
		return "until restart"
	case drainPersistent:
		return "yes"
	}
	return "no"
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

func runDrain(args []string) {
	const usage = "usage: triagefactory instance drain [--until-restart] <id>"
	fset := flag.NewFlagSet("instance drain", flag.ContinueOnError)
	untilRestart := fset.Bool("until-restart", false, "drain only until the instance next restarts")
	parseArgs(fset, args, usage)
	id := instanceID(fset.Args(), usage)

	store, closeFn := openInstanceStore()
	defer func() { _ = closeFn() }()

	var (
		matched bool
		err     error
	)
	if *untilRestart {
		matched, err = store.SetDrainUntilRestart(context.Background(), id)
	} else {
		matched, err = store.SetDraining(context.Background(), id, true)
	}
	if err != nil {
		fail("drain %s: %v", id, err)
	}
	if !matched {
		failUnknown(id)
	}
	if *untilRestart {
		fmt.Printf("instance %s is now draining until it next restarts: it will stop claiming new runs (existing runs carry on until they finish or park), and claims again once it restarts, with no undrain needed. A plain `instance drain` on it still holds after the restart; `instance list` shows which drain is in effect.\n", id)
		return
	}
	fmt.Printf("instance %s is now draining: it will stop claiming new runs (existing runs carry on until they finish or park), and stays drained across restarts until undrained; check `triagefactory instance list` for RUNS reaching 0/N before retiring it.\n", id)
}

func runUndrain(args []string) {
	const usage = "usage: triagefactory instance undrain <id>"
	fset := flag.NewFlagSet("instance undrain", flag.ContinueOnError)
	parseArgs(fset, args, usage)
	id := instanceID(fset.Args(), usage)

	store, closeFn := openInstanceStore()
	defer func() { _ = closeFn() }()

	matched, err := store.SetDraining(context.Background(), id, false)
	if err != nil {
		fail("undrain %s: %v", id, err)
	}
	if !matched {
		failUnknown(id)
	}
	fmt.Printf("instance %s is no longer draining: it will resume claiming new runs on its next heartbeat.\n", id)
}

// parseArgs parses a verb's flags, exiting 0 on --help and 2 on a bad flag.
func parseArgs(fset *flag.FlagSet, args []string, usage string) {
	fset.SetOutput(os.Stderr)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, usage)
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
}

// instanceID is a drain verb's one positional argument.
func instanceID(args []string, usage string) string {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, usage)
		fmt.Fprintln(os.Stderr, "run `triagefactory instance list` to see registered instance ids")
		os.Exit(2)
	}
	return args[0]
}

func failUnknown(id string) {
	fail("no registered instance with id %q — run `triagefactory instance list` to see registered ids", id)
}

func printUsage() {
	fmt.Println(`triagefactory instance — fleet registry operator commands.

USAGE
  triagefactory instance list [--json]     show every registered instance
  triagefactory instance drain <id>        stop new claims, quiesce, until
                                           undrained
  triagefactory instance drain --until-restart <id>
                                           stop new claims until the
                                           instance next restarts
  triagefactory instance undrain <id>      resume claims (ends either drain)
  triagefactory instance placement ...     inspect / steer run placement
                                           (run "instance placement --help")

NOTES
  Draining takes effect within one heartbeat interval (a few seconds) of
  the running instance itself, not immediately on this command's return —
  it writes the instance's registry row, which the instance reads back on
  its next heartbeat. A drained instance's live runs carry on until they
  finish or park; no new claims start. Safe to retire once RUNS reaches 0.

  A plain drain survives restarts: it is how an instance is taken out of
  rotation for maintenance or retirement, and a crash or host reboot does
  not put it back. A drain --until-restart ends when the instance next
  starts, with nothing to undo; deploy upgrade drains executors this way
  before it recreates them. DRAINING in the list shows no, yes or
  until restart.

  --json prints an array of objects with id, role, version, drain (none,
  persistent or until_restart), active_runs (null on a control instance)
  and heartbeat_age_seconds.

  Local mode: the single instance's row still exists (N=1) — drain works
  the same way, useful for a controlled shutdown ahead of an upgrade.`)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory instance: "+format+"\n", args...)
	os.Exit(1)
}
