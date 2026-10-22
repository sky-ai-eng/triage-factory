package instance

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
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

// placementStores is what the placement verbs read and write: the overrides,
// the repository registry an override's repository is resolved against and
// named from, the org store read for the org's current GitHub host, and a
// resolver for the explainer.
type placementStores struct {
	overrides db.PlacementOverrideStore
	repos     db.RepositoryStore
	orgs      db.OrgsStore
	resolver  *placement.Resolver
}

// openPlacementStores opens the DB and returns the stores the placement verbs
// need. Callers must call the returned close func.
func openPlacementStores() (placementStores, func() error) {
	conn, dialect, err := db.OpenForCLI()
	if err != nil {
		fail("%v", err)
	}
	var instances db.InstanceStore
	var st placementStores
	switch dialect {
	case "postgres":
		instances = pgstore.NewInstanceStore(conn)
		st.overrides = pgstore.NewPlacementOverrideStore(conn)
		stores := pgstore.NewWithoutSecrets(conn, conn)
		st.orgs, st.repos = stores.Orgs, stores.Repos
	default:
		instances = sqlitestore.NewInstanceStore(conn)
		st.overrides = sqlitestore.NewPlacementOverrideStore(conn)
		stores := sqlitestore.New(conn)
		st.orgs, st.repos = stores.Orgs, stores.Repos
	}
	// The explainer's Explain() ignores the enabled flag, but reads the
	// configured aging/liveness windows to report them — resolve them from env
	// the same way the server does.
	cfg, cfgErr := placement.ConfigFromEnv(os.Getenv, runmode.Current() == runmode.ModeLocal)
	if cfgErr != nil {
		_ = conn.Close()
		fail("%v", cfgErr)
	}
	st.resolver = placement.NewResolver(instances, st.overrides, cfg)
	return st, conn.Close
}

// placementKeyFlags binds the shared --org / --repo / --host /
// --repository-id flags and resolves the repository they name.
type placementKeyFlags struct {
	org          string
	repo         string
	host         string
	repositoryID string
}

func (f *placementKeyFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.org, "org", "", "org id (defaults to the local sentinel org in local mode)")
	fs.StringVar(&f.repo, "repo", "", "the repository, owner/repo")
	fs.StringVar(&f.host, "host", "", "GitHub host the repo lives on, e.g. https://ghe.example.com (defaults to the org's current GitHub host)")
	fs.StringVar(&f.repositoryID, "repository-id", "", "the repository's registry row id, in place of --repo (as `list` prints it)")
}

// resolve returns the org and the registry row of the repository the flags
// name: --repository-id, which reaches a retired row too, or --repo, an
// owner/repo resolved on --host in its canonical form or else the org's
// current GitHub host, since a name names a repository only within one host.
// The key and every override are that row's, so a name is resolved here once
// and never stored. A name no live row answers to is refused: TF registers a
// repository when it first records something in it, so no run is keyed on a
// repository without a row.
func (f *placementKeyFlags) resolve(st placementStores) (string, *domain.Repository) {
	ctx := context.Background()
	orgID := f.org
	if orgID == "" {
		orgID = runmode.LocalDefaultOrgID
	}
	switch {
	case f.repositoryID != "" && (f.repo != "" || f.host != ""):
		fail("--repository-id names the repository alone; drop --repo and --host")
	case f.repositoryID != "":
		repo, err := st.repos.GetSystem(ctx, orgID, f.repositoryID)
		if errors.Is(err, db.ErrNoSuchRepository) {
			fail("no repository with id %s in org %s", f.repositoryID, orgID)
		}
		if err != nil {
			fail("read repository %s: %v", f.repositoryID, err)
		}
		return orgID, repo
	case f.repo == "":
		fail("a --repo or --repository-id is required")
	}
	owner, name, ok := strings.Cut(f.repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		fail("--repo must be owner/repo, got %q", f.repo)
	}
	host := domain.GitHubHost(f.host)
	if f.host == "" {
		var err error
		if host, err = db.OrgGitHubHostSystem(ctx, st.orgs, orgID); err != nil {
			fail("resolve the org's GitHub host: %v", err)
		}
	}
	repo, err := st.repos.GetByRefSystem(ctx, orgID, domain.RepoRef{Host: host, Owner: owner, Repo: name})
	if err != nil {
		fail("resolve %s on %s: %v", f.repo, host, err)
	}
	if repo == nil {
		fail("no repository answers to %s on %s in org %s; TF registers a repository when it first records something in it", f.repo, host, orgID)
	}
	return orgID, repo
}

// describeRepo names a registry row for output: its current name and host,
// and its id.
func describeRepo(repo *domain.Repository) string {
	return fmt.Sprintf("%s on %s (repository %s)", repo.Slug(), repo.Host, repo.ID)
}

func runPlacementExplain(args []string) {
	fs := flag.NewFlagSet("placement explain", flag.ExitOnError)
	var kf placementKeyFlags
	kf.bind(fs)
	_ = fs.Parse(args)

	st, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()
	orgID, repo := kf.resolve(st)

	plan, err := st.resolver.Explain(context.Background(), orgID, repo.ID)
	if err != nil {
		fail("explain %s: %v", describeRepo(repo), err)
	}

	state := "advisory-off (queue is global-oldest)"
	if plan.Enabled {
		state = "enabled"
	}
	fmt.Printf("placement for %s (org %s): %s\n", describeRepo(repo), orgID, state)
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

	st, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()
	orgID, repo := kf.resolve(st)

	if _, err := st.overrides.Upsert(context.Background(), domain.PlacementOverride{
		OrgID: orgID, RepositoryID: repo.ID, PinnedInstanceID: *instanceID,
	}); err != nil {
		fail("pin: %v", err)
	}
	fmt.Printf("pinned %s (org %s) -> %s\n", describeRepo(repo), orgID, *instanceID)
	fmt.Println("takes effect on the next run enqueued for this repository; run `... placement explain` to preview.")
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

	st, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()
	orgID, repo := kf.resolve(st)

	if _, err := st.overrides.Upsert(context.Background(), domain.PlacementOverride{
		OrgID: orgID, RepositoryID: repo.ID, Replicas: *k,
	}); err != nil {
		fail("replicas: %v", err)
	}
	fmt.Printf("set replicas=%d for %s (org %s)\n", *k, describeRepo(repo), orgID)
	fmt.Println("the top-K rendezvous candidates now all count as preferred; runs of this repository spread across them.")
}

func runPlacementClear(args []string) {
	fs := flag.NewFlagSet("placement clear", flag.ExitOnError)
	var kf placementKeyFlags
	kf.bind(fs)
	_ = fs.Parse(args)

	st, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()
	orgID, repo := kf.resolve(st)

	matched, err := st.overrides.Delete(context.Background(), orgID, repo.ID)
	if err != nil {
		fail("clear: %v", err)
	}
	if !matched {
		fmt.Printf("no override on %s (org %s) — nothing to clear\n", describeRepo(repo), orgID)
		return
	}
	fmt.Printf("cleared override on %s (org %s); placement reverts to the pure rendezvous hash\n", describeRepo(repo), orgID)
}

func runPlacementList(args []string) {
	fs := flag.NewFlagSet("placement list", flag.ExitOnError)
	org := fs.String("org", "", "org id (defaults to the local sentinel org in local mode)")
	_ = fs.Parse(args)
	orgID := *org
	if orgID == "" {
		orgID = runmode.LocalDefaultOrgID
	}

	st, closeFn := openPlacementStores()
	defer func() { _ = closeFn() }()

	ctx := context.Background()
	rows, err := st.overrides.List(ctx, orgID)
	if err != nil {
		fail("list overrides: %v", err)
	}
	if len(rows) == 0 {
		fmt.Printf("no placement overrides for org %s (the rendezvous hash is the default map)\n", orgID)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REPOSITORY ID\tHOST\tREPO\tPIN\tREPLICAS\tUPDATED")
	for _, ov := range rows {
		// The name is the registry row's current one, read here rather than
		// stored on the override.
		repo, err := st.repos.GetSystem(ctx, orgID, ov.RepositoryID)
		if err != nil {
			fail("read repository %s: %v", ov.RepositoryID, err)
		}
		name := repo.Slug()
		if repo.RetiredAt != nil {
			name += " (retired)"
		}
		pin := ov.PinnedInstanceID
		if pin == "" {
			pin = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", ov.RepositoryID, repo.Host, name, pin, ov.Replicas, ov.UpdatedAt.Format("2006-01-02 15:04"))
	}
	_ = w.Flush()
}

func printPlacementUsage() {
	fmt.Println(`triagefactory instance placement — inspect and steer run placement.

USAGE
  triagefactory instance placement explain  --org <id> (--repo <owner/repo> [--host <url>] | --repository-id <id>)
  triagefactory instance placement pin      --org <id> (--repo <owner/repo> [--host <url>] | --repository-id <id>) --instance <id>
  triagefactory instance placement replicas --org <id> (--repo <owner/repo> [--host <url>] | --repository-id <id>) --k <N>
  triagefactory instance placement clear    --org <id> (--repo <owner/repo> [--host <url>] | --repository-id <id>)
  triagefactory instance placement list     --org <id>

NOTES
  Placement is advisory: a repeatedly-hit repo tends to execute on its
  rendezvous owner to keep its clone/worktree cache warm, but any executor
  can run any run — a saturated or dead owner's work ages briefly, then
  flows to spare capacity. The queue is the truth; placement is a preference.

  explain shows the computed candidate order for a repository (weights,
  scores, liveness, overrides) — the same order the two-tier claim honors.

  pin / replicas / clear manage human-intent overrides. A pin forces one
  owner; replicas=K spreads a hot repository's runs across its top-K
  candidates. Overrides take effect on the next enqueue, not retroactively.

  A repository's key, and its overrides, are its registry row's id, so a
  rename keeps both. --repo is resolved to that row once: an owner/repo names
  a repository only on one GitHub host, so it is looked up on --host, or the
  org's current GitHub host when that is omitted. --repository-id names the
  row directly, as list prints it, and reaches a retired repository's
  override too.

  Local mode: single-process N=1, so placement is a no-op (the hash always
  returns self) — these verbs still work for inspection.`)
}
