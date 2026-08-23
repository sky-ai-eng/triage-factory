package instance

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// runPlacement dispatches `triagefactory instance placement <verb>` — the
// operator surface for the placement affinity layer: the explainer
// (computed rendezvous candidate order for a key) plus the override CRUD
// (manual pin / hot-key replica count). Shares the `instance` command's
// shell-access operator boundary and its own DB handle (no IPC to a live
// process — an override is read on the next enqueue).
func runPlacement(args []string) {
	if len(args) == 0 {
		printPlacementUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "explain":
		runPlacementExplain(args[1:])
	case "pin":
		runPlacementPin(args[1:])
	case "replicas":
		runPlacementReplicas(args[1:])
	case "clear":
		runPlacementClear(args[1:])
	case "list":
		runPlacementList(args[1:])
	case "-h", "--help", "help":
		printPlacementUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown instance placement subcommand %q\n\n", args[0])
		printPlacementUsage()
		os.Exit(2)
	}
}

// openPlacementStores opens the DB and returns the two stores the placement
// verbs need plus a resolver for the explainer. Callers must close the conn.
func openPlacementStores() (instances db.InstanceStore, overrides db.PlacementOverrideStore, resolver *placement.Resolver, closeFn func() error) {
	conn, dialect, err := db.OpenForCLI()
	if err != nil {
		fail("%v", err)
	}
	switch dialect {
	case "postgres":
		instances = pgstore.NewInstanceStore(conn)
		overrides = pgstore.NewPlacementOverrideStore(conn)
	default:
		instances = sqlitestore.NewInstanceStore(conn)
		overrides = sqlitestore.NewPlacementOverrideStore(conn)
	}
	// The explainer's Explain() ignores the enabled flag, but reads the
	// configured aging/liveness windows to report them — resolve them from env
	// the same way the server does.
	cfg, cfgErr := placement.ConfigFromEnv(os.Getenv, runmode.Current() == runmode.ModeLocal)
	if cfgErr != nil {
		_ = conn.Close()
		fail("%v", cfgErr)
	}
	resolver = placement.NewResolver(instances, overrides, cfg)
	return instances, overrides, resolver, conn.Close
}

// placementKeyFlags binds the shared --org / --repo / --kind flags and
// resolves a (kind, value) key. --repo implies kind=repo; --kind overrides.
type placementKeyFlags struct {
	org  string
	repo string
	kind string
}

func (f *placementKeyFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.org, "org", "", "org id (defaults to the local sentinel org in local mode)")
	fs.StringVar(&f.repo, "repo", "", "repo key, owner/repo (delegation)")
	fs.StringVar(&f.kind, "kind", "", "key kind: repo (inferred from --repo)")
}

func (f *placementKeyFlags) resolve() (orgID, kind, value string) {
	orgID = f.org
	if orgID == "" {
		orgID = runmode.LocalDefaultOrgID
	}
	switch {
	case f.repo != "":
		kind, value = domain.PlacementKindRepo, f.repo
	default:
		fail("a --repo key is required")
	}
	if f.kind != "" {
		kind = f.kind
	}
	if kind != domain.PlacementKindRepo {
		fail("--kind must be 'repo'")
	}
	return orgID, kind, value
}

func runPlacementExplain(args []string) {
	fs := flag.NewFlagSet("placement explain", flag.ExitOnError)
	var kf placementKeyFlags
	kf.bind(fs)
	_ = fs.Parse(args)
	orgID, kind, value := kf.resolve()

	_, _, resolver, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()

	plan, err := resolver.Explain(context.Background(), orgID, kind, value)
	if err != nil {
		fail("explain %s/%s: %v", kind, value, err)
	}

	state := "advisory-off (queue is global-oldest)"
	if plan.Enabled {
		state = "enabled"
	}
	fmt.Printf("placement for %s key %q (org %s): %s\n", kind, value, orgID, state)
	fmt.Printf("  aging=%s liveness=%s\n", plan.Aging, plan.Liveness)
	if plan.Override != nil {
		if plan.Override.PinnedInstanceID != "" {
			fmt.Printf("  override: pinned -> %s\n", plan.Override.PinnedInstanceID)
		} else if plan.Override.Replicas > 0 {
			fmt.Printf("  override: replicas=%d\n", plan.Override.Replicas)
		}
	}
	if len(plan.PreferredSet) > 0 {
		fmt.Printf("  preferred: %v\n", plan.PreferredSet)
	} else {
		fmt.Printf("  preferred: (none — no live executor)\n")
	}
	if len(plan.Candidates) == 0 {
		fmt.Println("  no candidates (no registered executors)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  RANK\tINSTANCE\tWEIGHT\tSCORE\tELIGIBLE\tPREFERRED\tNOTE")
	rank := 1
	for _, c := range plan.Candidates {
		rankStr := "-"
		if c.Eligible {
			rankStr = fmt.Sprintf("%d", rank)
			rank++
		}
		fmt.Fprintf(w, "  %s\t%s\t%d\t%.4f\t%t\t%t\t%s\n",
			rankStr, c.InstanceID, c.Weight, c.Score, c.Eligible, c.Preferred, c.Reason)
	}
	_ = w.Flush()
}

func runPlacementPin(args []string) {
	fs := flag.NewFlagSet("placement pin", flag.ExitOnError)
	var kf placementKeyFlags
	kf.bind(fs)
	instanceID := fs.String("instance", "", "instance id to pin this key to")
	_ = fs.Parse(args)
	if *instanceID == "" {
		fail("--instance is required")
	}
	orgID, kind, value := kf.resolve()

	_, overrides, _, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()

	if _, err := overrides.Upsert(context.Background(), domain.PlacementOverride{
		OrgID: orgID, KeyKind: kind, KeyValue: value, PinnedInstanceID: *instanceID,
	}); err != nil {
		fail("pin: %v", err)
	}
	fmt.Printf("pinned %s key %q (org %s) -> %s\n", kind, value, orgID, *instanceID)
	fmt.Println("takes effect on the next run enqueued for this key; run `... placement explain` to preview.")
}

func runPlacementReplicas(args []string) {
	fs := flag.NewFlagSet("placement replicas", flag.ExitOnError)
	var kf placementKeyFlags
	kf.bind(fs)
	k := fs.Int("k", 0, "number of top rendezvous candidates to treat as preferred (hot-key K)")
	_ = fs.Parse(args)
	if *k < 1 {
		fail("--k must be >= 1")
	}
	orgID, kind, value := kf.resolve()

	_, overrides, _, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()

	if _, err := overrides.Upsert(context.Background(), domain.PlacementOverride{
		OrgID: orgID, KeyKind: kind, KeyValue: value, Replicas: *k,
	}); err != nil {
		fail("replicas: %v", err)
	}
	fmt.Printf("set replicas=%d for %s key %q (org %s)\n", *k, kind, value, orgID)
	fmt.Println("the top-K rendezvous candidates now all count as preferred; runs of this key spread across them.")
}

func runPlacementClear(args []string) {
	fs := flag.NewFlagSet("placement clear", flag.ExitOnError)
	var kf placementKeyFlags
	kf.bind(fs)
	_ = fs.Parse(args)
	orgID, kind, value := kf.resolve()

	_, overrides, _, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()

	matched, err := overrides.Delete(context.Background(), orgID, kind, value)
	if err != nil {
		fail("clear: %v", err)
	}
	if !matched {
		fmt.Printf("no override on %s key %q (org %s) — nothing to clear\n", kind, value, orgID)
		return
	}
	fmt.Printf("cleared override on %s key %q (org %s); placement reverts to the pure rendezvous hash\n", kind, value, orgID)
}

func runPlacementList(args []string) {
	fs := flag.NewFlagSet("placement list", flag.ExitOnError)
	org := fs.String("org", "", "org id (defaults to the local sentinel org in local mode)")
	_ = fs.Parse(args)
	orgID := *org
	if orgID == "" {
		orgID = runmode.LocalDefaultOrgID
	}

	_, overrides, _, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()

	rows, err := overrides.List(context.Background(), orgID)
	if err != nil {
		fail("list overrides: %v", err)
	}
	if len(rows) == 0 {
		fmt.Printf("no placement overrides for org %s (the rendezvous hash is the default map)\n", orgID)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tKEY\tPIN\tREPLICAS\tUPDATED")
	for _, ov := range rows {
		pin := ov.PinnedInstanceID
		if pin == "" {
			pin = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", ov.KeyKind, ov.KeyValue, pin, ov.Replicas, ov.UpdatedAt.Format("2006-01-02 15:04"))
	}
	_ = w.Flush()
}

func printPlacementUsage() {
	fmt.Println(`triagefactory instance placement — inspect and steer run placement.

USAGE
  triagefactory instance placement explain  --org <id> --repo <owner/repo>
  triagefactory instance placement pin      --org <id> --repo <owner/repo> --instance <id>
  triagefactory instance placement replicas --org <id> --repo <owner/repo> --k <N>
  triagefactory instance placement clear    --org <id> --repo <owner/repo>
  triagefactory instance placement list     --org <id>

NOTES
  Placement is advisory: a repeatedly-hit repo tends to execute on its
  rendezvous owner to keep its clone/worktree cache warm, but any executor
  can run any run — a saturated or dead owner's work ages briefly, then
  flows to spare capacity. The queue is the truth; placement is a preference.

  explain shows the computed candidate order for a key (weights, scores,
  liveness, overrides) — the same order the two-tier claim honors.

  pin / replicas / clear manage human-intent overrides. A pin forces one
  owner; replicas=K spreads a hot key's runs across its top-K candidates.
  Overrides take effect on the next enqueue, not retroactively. --kind
  overrides the inferred kind.

  Local mode: single-process N=1, so placement is a no-op (the hash always
  returns self) — these verbs still work for inspection.`)
}
