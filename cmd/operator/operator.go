// Package operator is the CLI entrypoint for the `triagefactory operator`
// subcommand — the deployment-operator identity's operator surface (TFAC-589):
//
//	triagefactory operator add <email>       grant the fleet-console operator flag
//	triagefactory operator remove <email>    revoke it
//	triagefactory operator list              show every operator
//
// The bootstrap trust boundary is shell access to the deployment, the same
// boundary as `jwk-init` and `instance` — an operator is org-less deployment
// config (fleet administration is deployment-scoped, not a member role), so it
// is managed here rather than through any org-admin API. The verb opens its own
// DB handle (db.OpenForCLI), independent of a running server, and each grant/
// revoke appends a best-effort auth_events audit row (org-less, so auth_events —
// the SOC2 log with a nullable org — is its home, not the org-scoped
// access_change_log).
//
// Multi mode only. Local mode's single user is the operator implicitly and
// nothing there reads a grant, so the verbs refuse rather than write one.
package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Handle dispatches the operator subcommand.
func Handle(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "add":
		runAdd(args[1:])
	case "remove", "rm":
		runRemove(args[1:])
	case "list", "ls":
		runList()
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown operator subcommand %q\n\n", args[0])
		printUsage()
		os.Exit(2)
	}
}

// openStores opens the deployment's Postgres over the admin DSN and wraps it in
// the narrow operator + auth-event constructors — no full db.Stores bundle
// needed to reach two admin-pool tables. Callers close conn.
func openStores() (ops db.OperatorStore, audit db.AuthEventStore, closeFn func() error) {
	if err := checkProcess(); err != nil {
		fail("%v", err)
	}
	conn, _, err := db.OpenForCLI()
	if err != nil {
		fail("%v", err)
	}
	return pgstore.NewOperatorStore(conn), pgstore.NewAuthEventStore(conn), conn.Close
}

// checkProcess refuses, before anything connects, the processes that cannot
// manage a grant. Local mode has none to manage: its single user is the
// operator implicitly, and neither the fleet gate nor /api/me reads the table
// there, so a grant written locally would report success and change nothing.
// An executor's database role (tf_system) holds no grant on operators, because
// a role that can write the table can grant the fleet console, and binding a
// grant reads login identities, which that role cannot read either.
func checkProcess() error {
	if runmode.Current() != runmode.ModeMulti {
		return errors.New("local mode has no deployment operators: its single user is the operator implicitly; grants exist only on a multi-mode deployment")
	}
	if runmode.Role() == runmode.RoleExecutor {
		return errors.New("operator grants are managed from a control container (TF_ROLE=control); an executor's database role has no access to them")
	}
	return nil
}

func runAdd(args []string) {
	email := requireEmail(args, "add")
	ops, audit, closeFn := openStores()
	defer func() { _ = closeFn() }()

	actor := osActor()
	op, err := ops.Add(context.Background(), email, actor)
	if err != nil {
		fail("add operator %q: %v", email, err)
	}
	recordAudit(audit, domain.AuthEventOperatorGranted, email, actor)
	if op.Pending() {
		fmt.Printf("%s is now a deployment operator, pending: no sign-in holds that address verified yet, so the grant binds to the first person who signs in with it verified.\n", op.Email)
	} else {
		fmt.Printf("%s is now a deployment operator, bound to user %s, the account that holds that address verified. The grant stays with that account if its address changes later.\n", op.Email, op.UserID)
	}
	fmt.Println("They can reach the fleet console once the deployment is licensed for fleet administration (FeatureFleet); operability (drain, `instance list`) needs no license.")
}

func runRemove(args []string) {
	email := requireEmail(args, "remove")
	ops, audit, closeFn := openStores()
	defer func() { _ = closeFn() }()

	removed, err := ops.Remove(context.Background(), email)
	if err != nil {
		fail("remove operator %q: %v", email, err)
	}
	if !removed {
		fail("%q is not a deployment operator — run `triagefactory operator list` to see who is", email)
	}
	recordAudit(audit, domain.AuthEventOperatorRevoked, email, osActor())
	fmt.Printf("%s is no longer a deployment operator. The fleet console 404s for them on their next request.\n", email)
}

func runList() {
	ops, _, closeFn := openStores()
	defer func() { _ = closeFn() }()

	rows, err := ops.List(context.Background())
	if err != nil {
		fail("list operators: %v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no deployment operators (add one with `triagefactory operator add <email>`)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tUSER\tADDED\tADDED BY")
	for _, r := range rows {
		by := r.AddedBy
		if by == "" {
			by = "-"
		}
		user := r.UserID
		if r.Pending() {
			user = "(pending)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Email, user, r.AddedAt.Format(time.DateOnly), by)
	}
	_ = w.Flush()
}

func requireEmail(args []string, verb string) string {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintf(os.Stderr, "usage: triagefactory operator %s <email>\n", verb)
		os.Exit(2)
	}
	email := strings.TrimSpace(args[0])
	// A minimal sanity check — the store lower-cases and resolves the address
	// against verified login identities, so anything without an @ is a typo.
	if !strings.Contains(email, "@") {
		fail("%q does not look like an email address", email)
	}
	return email
}

// osActor names the OS user running the CLI, best-effort provenance for the
// audit row + `operator list`. Never an authorization input.
func osActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// recordAudit appends the grant/revoke to auth_events, best-effort — mirroring
// the server's own recordAuthEvent contract (a failed audit write logs but
// never fails the action). Org/user are empty (an org-less shell operation).
func recordAudit(audit db.AuthEventStore, eventType, email, actor string) {
	if audit == nil {
		return
	}
	detail, _ := json.Marshal(map[string]string{"email": strings.ToLower(email), "actor": actor})
	err := audit.RecordSystem(context.Background(), domain.AuthEvent{
		EventType:  eventType,
		DetailJSON: string(detail),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: operator change applied but audit write failed: %v\n", err)
	}
}

func printUsage() {
	fmt.Println(`triagefactory operator — deployment-operator identity for the fleet console.

USAGE
  triagefactory operator add <email>       grant the fleet-console operator flag
  triagefactory operator remove <email>    revoke it
  triagefactory operator list              show every operator

NOTES
  An operator is a DEPLOYMENT identity, not an org role: it gates the fleet
  administration console (the Fleet page + rich /api/fleet surfaces). The gate
  composes with the deployment license — a surface renders only when the caller
  is an operator AND the deployment is licensed for fleet administration. Core
  operability (the capacity read-out, drain via "instance drain", this CLI)
  needs no license.

  The email names the person: add a verified address of one of their sign-in
  methods (case does not matter). The grant binds to the account that holds
  that address verified, at once if one does, otherwise at the first sign-in
  that verifies it, and from then on admits that account only, even if the
  address later belongs to someone else. "operator list" shows each grant's
  account, or "(pending)". To move a grant to another account, remove it and
  add it again.

  Run it on a control container: an executor's database role has no access to
  operator grants. Shell access is the trust boundary (same as jwk-init). Each
  add/remove is recorded in the auth_events audit log.

  Multi mode only. In local mode the single user is the operator implicitly,
  so every verb refuses.`)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory operator: "+format+"\n", args...)
	os.Exit(1)
}
