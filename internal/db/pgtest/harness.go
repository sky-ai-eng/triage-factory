// Package pgtest spins up a supabase/postgres testcontainer for D3+
// Postgres-backed tests. The harness is shared per-process — the first
// test pays the boot cost; subsequent tests share the same container and
// call Reset between cases to TRUNCATE state.
//
// The shared container is a single point of failure for the whole test
// binary: on a loaded CI runner the kernel OOM killer sometimes takes
// out the postmaster mid-suite, which used to cascade into every
// remaining Postgres test failing at Reset ("unexpected EOF" from the
// dying connections, then "connection refused" forever after). Shared
// and Reset now detect that death (liveness probe, never error-string
// matching) and boot a replacement in place — see reviveLocked — so a
// one-off kill costs one container boot instead of the rest of the
// suite.
//
// Three SQL connections are exposed (SystemDB being the tf_system
// executor role documented on the Harness struct):
//   - AdminDB connects as `supabase_admin` — the real superuser in the
//     supabase image. The image's own migrations demote `postgres` to
//     NOSUPERUSER (see 10000000000000_demote-postgres.sql), so attempts
//     to ALTER reserved roles like `authenticator` from a postgres
//     connection are rejected by the supautils extension. supabase_admin
//     bypasses RLS; use this for migrations, fixture seeding, and the
//     explicit "this should be visible to admin" assertion.
//   - AppDB connects as `authenticator`. Always pair with WithUser (or
//     the SET LOCAL ROLE tf_app + claims-set ceremony directly) — raw
//     AppDB queries without that ceremony fail because tf_app inherits
//     no privileges by default (NOINHERIT on authenticator).
//
// Picking the wrong connection is the single biggest test-author trap:
// AdminDB silently bypasses RLS. The harness exposes them as separate
// fields so the choice is visible in code review.
package pgtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// SecretKey is a fixed app-layer AES-256-GCM key for pgstore.New /
// pgstore.NewForTx in tests (TFAC-402). Most store tests never touch
// secrets and pass it only to satisfy the constructor; the SecretStore
// tests rely on it being stable so a value written through NewForTx reads
// back through New. Non-zero so a "did we forget to wire the key" bug
// can't masquerade as a passing zero-key round-trip.
var SecretKey = aead.Key{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// Image is pinned to match the multi-mode prod compose stack. Drift
// here = drift between test and prod auth/vault schemas.
const Image = "supabase/postgres:15.1.0.147"

// authPassword is the password we set on the authenticator role inside
// the container after migrations run. The image ships authenticator
// LOGIN/NOINHERIT with no password; we set one for AppDB.
const authPassword = "auth_test_pw"

// systemPassword is the password the harness ALTERs onto tf_system
// (baseline-created NOLOGIN) so SystemDB can connect. Mirrors
// authPassword's role — see the tf_system section of the pg baseline
// migration for why the role ships without LOGIN by default.
const systemPassword = "system_test_pw"

// Harness is the shared per-process testcontainer + three connections.
// All tests that touch Postgres acquire it via Shared(t).
type Harness struct {
	Container *postgres.PostgresContainer
	AdminDB   *sql.DB // supabase_admin; bypasses RLS
	AppDB     *sql.DB // authenticator; use WithUser for RLS-active txns
	SystemDB  *sql.DB // tf_system; least-privilege executor role, BYPASSRLS
}

var (
	// sharedMu guards the boot/revive lifecycle below. The pg-backed
	// suites run their tests sequentially (no t.Parallel), so contention
	// is nil — the mutex exists so reviveLocked's in-place connection
	// swap can never race a concurrent Shared call under -race.
	sharedMu  sync.Mutex
	booted    bool
	shared    *Harness
	sharedErr error
	// reviveBudget bounds how many container deaths a single test binary
	// will absorb. One death is CI resource pressure; a container that
	// keeps dying means the environment is genuinely unstable (or a test
	// is crashing Postgres), and re-booting per test would just stretch a
	// doomed run out to the go-test timeout.
	reviveBudget = 2
)

// Shared returns the package-scoped harness, booting the container on
// the first call. Subsequent calls return the same instance. Tests
// that need isolation should call h.Reset(t) at the top of the test.
//
// Three outcomes are distinct on purpose:
//   - Docker is genuinely unreachable → t.Skip. Lets the SQLite suite
//     run cleanly in CI environments without a Docker daemon.
//   - Docker is healthy but boot failed (migration error, SQL bug,
//     image regression, anything else) → t.Fatalf. Treating these as
//     skips would let a real schema regression silently turn the
//     Postgres suite into a green-but-empty pass.
//   - A previously healthy container no longer answers a liveness probe
//     (OOM-killed postmaster on a loaded CI runner) → revive: boot a
//     replacement in place, bounded by reviveBudget.
func Shared(t *testing.T) *Harness {
	t.Helper()
	// Probe Docker first so unreachable-daemon failures are
	// disambiguated from boot failures. The probe is cheap — pings the
	// Docker socket via the testcontainers provider — and runs on
	// every Shared() call (cheap is fine; booted guards the expensive
	// boot itself).
	testcontainers.SkipIfProviderIsNotHealthy(t)

	sharedMu.Lock()
	defer sharedMu.Unlock()
	if !booted {
		booted = true
		shared, sharedErr = boot()
	}
	if sharedErr != nil {
		t.Fatalf("pgtest: harness unavailable (Docker is reachable but bring-up errored — this is NOT a skip-worthy condition): %v", sharedErr)
	}
	if err := pingSharedLocked(); err != nil {
		if rerr := reviveLocked(fmt.Errorf("liveness probe: %w", err)); rerr != nil {
			t.Fatalf("pgtest: shared container died and revive failed: %v", rerr)
		}
	}
	return shared
}

// pingSharedLocked probes the shared container for liveness. The bound
// exists so a hung-but-listening server classifies as dead instead of
// stalling the binary into go test's panic timeout; against a dead
// container the dial fails immediately. Callers hold sharedMu.
func pingSharedLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return shared.AdminDB.PingContext(ctx)
}

// reviveLocked replaces a dead shared container with a freshly booted,
// fully migrated one, swapping the new connections into the existing
// Harness value so pointers tests already hold observe the replacement.
// A failed revive (or an exhausted budget) is sticky via sharedErr —
// every subsequent Shared call fails fast rather than each paying a
// multi-minute boot attempt against a broken environment. Callers hold
// sharedMu; failures are returned, never t.Fatalf'd here, so callers
// can release the lock before failing the test (t.Fatalf runs Goexit,
// which would leak the lock and deadlock the rest of the suite).
func reviveLocked(cause error) error {
	// The dead instance is torn down before the budget check so every
	// exit path leaves no stale handles or exited container behind.
	// Bounded: a stuck Docker daemon must not hang the revive forever.
	// Terminate on an already-dead container just errors; ignore it.
	_ = shared.AdminDB.Close()
	_ = shared.AppDB.Close()
	_ = shared.SystemDB.Close()
	termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = shared.Container.Terminate(termCtx)
	termCancel()

	if reviveBudget <= 0 {
		sharedErr = fmt.Errorf("container died again (%v) with the revive budget spent — repeated deaths mean genuine environment instability, not a one-off kill", cause)
		return sharedErr
	}
	reviveBudget--
	fmt.Fprintf(os.Stderr, "pgtest: shared Postgres container unreachable (%v); booting a replacement (%d revive(s) left in this process)\n", cause, reviveBudget)

	fresh, err := boot()
	if err != nil {
		sharedErr = fmt.Errorf("revive after container death (%v): %w", cause, err)
		return sharedErr
	}
	*shared = *fresh
	return nil
}

// bootContainer starts a fresh supabase/postgres container and returns
// it alongside its raw (postgres-user) DSN, its supabase_admin
// (real superuser) DSN, and an admin connection opened from that DSN.
// Factored out of boot() so NewInstance can reuse the exact same
// container recipe to hand a test a dedicated, unmigrated instance —
// the image's own init (auth schema, vault extension, authenticator
// role) is complete, but db.Migrate has not yet run, which boot()
// cannot offer once shared has been populated.
func bootContainer(ctx context.Context) (pg *postgres.PostgresContainer, pgDSN, adminDSN string, adminDB *sql.DB, err error) {
	// Wait strategy: a single SQL probe for auth.users in the
	// POSTGRES_DB-named DB. We tried a two-stage approach earlier
	// (wait.ForLog "PostgreSQL init process complete" THEN ForSQL)
	// but the log marker fires DURING init, before the image's
	// post-init restart (the one that picks up shared_preload_libraries),
	// which leaves a race where the SQL probe runs against a Postgres
	// that's about to be shut down for restart. By contrast, the
	// SQL probe alone can only succeed AFTER the restart — once
	// auth.users is reachable on a stable connection, both the init
	// scripts AND the restart cycle have completed.
	//
	// Probing the POSTGRES_DB-named DB (not /postgres) matters: image
	// init only seeds the auth schema in the configured DB.
	//
	// Why the long timeout: the supabase image's init phase (auth
	// schema, vault key bootstrap, ~20 supabase migrations, restart)
	// takes 30-60s on warm machines and longer on first pull.
	// WithWaitStrategy wraps strategies in a 60s deadline that
	// overrides each strategy's own timeout — *AndDeadline bumps it.
	waitStrategies := []wait.Strategy{
		wait.ForSQL("5432/tcp", "pgx", func(host string, port string) string {
			// wait.ForSQL passes `network.Port.String()` here, which
			// returns "54331/tcp" (number + proto suffix), not just the
			// number. Pasting raw makes Postgres see "tcp/tf_test" as
			// the database name. Strip the suffix.
			if i := strings.Index(port, "/"); i > 0 {
				port = port[:i]
			}
			return fmt.Sprintf("postgres://postgres:postgres@%s:%s/tf_test?sslmode=disable", host, port)
		}).WithQuery("SELECT 1 WHERE to_regclass('auth.users') IS NOT NULL").
			WithStartupTimeout(3 * time.Minute).
			WithPollInterval(1 * time.Second),
	}

	pg, err = postgres.Run(ctx, Image,
		postgres.WithDatabase("tf_test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		// postgres.Run defaults Cmd to ["postgres", "-c", "fsync=off"],
		// which makes Postgres ignore the supabase image's bundled
		// /etc/postgresql/postgresql.conf entirely. That file carries
		// both `listen_addresses='*'` (without it the container binds
		// only to 127.0.0.1) AND `shared_preload_libraries` including
		// pgsodium (without it vault.create_secret raises
		// "pgsodium_derive: no server secret key defined"). Point
		// Postgres at the bundled config so we get both.
		testcontainers.WithCmd("postgres",
			"-c", "config_file=/etc/postgresql/postgresql.conf",
			"-c", "fsync=off",
			// The bundled config sizes shared_buffers=128MB for real
			// workloads; a test DB holds a few hundred rows. Later -c
			// flags win over the config file. Trimming keeps N
			// concurrent harness containers (one per test binary under
			// `go test ./...`) cheap, and — because shared buffers count
			// toward every backend's RSS — stops Postgres processes from
			// looking like the fattest OOM-kill candidates on a loaded
			// CI runner.
			"-c", "shared_buffers=32MB",
		),
		testcontainers.WithWaitStrategyAndDeadline(3*time.Minute, waitStrategies...),
	)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("start container: %w", err)
	}

	pgDSN, err = pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pg.Terminate(ctx)
		return nil, "", "", nil, fmt.Errorf("admin dsn: %w", err)
	}

	// The supabase image demotes `postgres` to non-superuser during
	// init (see 10000000000000_demote-postgres.sql). The real
	// superuser is `supabase_admin`, whose password is set to
	// POSTGRES_PASSWORD by the image's migrate.sh. Connect as
	// supabase_admin for migrations + reserved-role ALTERs (the
	// supautils extension would otherwise reject "ALTER ROLE
	// authenticator" from a non-superuser).
	adminDSN, err = rewriteUser(pgDSN, "supabase_admin", "postgres")
	if err != nil {
		_ = pg.Terminate(ctx)
		return nil, "", "", nil, fmt.Errorf("admin dsn rewrite: %w", err)
	}
	adminDB, err = sql.Open("pgx", adminDSN)
	if err != nil {
		_ = pg.Terminate(ctx)
		return nil, "", "", nil, fmt.Errorf("open admin db: %w", err)
	}
	return pg, pgDSN, adminDSN, adminDB, nil
}

func boot() (*Harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pg, pgDSN, _, adminDB, err := bootContainer(ctx)
	if err != nil {
		return nil, err
	}

	// Run goose migrations as supabase_admin (real superuser). RLS is
	// bypassed here by design — migrations create roles, grant
	// defaults, install policies. Trying to do this as tf_app would
	// fail on the GRANT statements.
	if err := db.Migrate(adminDB, "postgres"); err != nil {
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// The image ships authenticator LOGIN but with no password. Set
	// one so AppDB can connect. Reserved-role ALTERs only succeed as
	// the real superuser.
	escapedAuthPassword := strings.ReplaceAll(authPassword, "'", "''")
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("ALTER ROLE authenticator WITH PASSWORD '%s'", escapedAuthPassword),
	); err != nil {
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("set authenticator password: %w", err)
	}

	appDSN, err := rewriteUser(pgDSN, "authenticator", authPassword)
	if err != nil {
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("build app dsn: %w", err)
	}
	appDB, err := sql.Open("pgx", appDSN)
	if err != nil {
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("open app db: %w", err)
	}

	// tf_system ships NOLOGIN in the baseline (production LOGIN comes from
	// the postgres-postinit sidecar's ALTER, driven by TF_SYSTEM_PASSWORD —
	// see docker-compose.yml). Mirror that here so SystemDB can connect.
	escapedSystemPassword := strings.ReplaceAll(systemPassword, "'", "''")
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("ALTER ROLE tf_system WITH LOGIN PASSWORD '%s'", escapedSystemPassword),
	); err != nil {
		_ = appDB.Close()
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("set tf_system password: %w", err)
	}
	systemDSN, err := rewriteUser(pgDSN, "tf_system", systemPassword)
	if err != nil {
		_ = appDB.Close()
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("build system dsn: %w", err)
	}
	systemDB, err := sql.Open("pgx", systemDSN)
	if err != nil {
		_ = appDB.Close()
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("open system db: %w", err)
	}

	return &Harness{Container: pg, AdminDB: adminDB, AppDB: appDB, SystemDB: systemDB}, nil
}

// NewInstance boots a dedicated, independent supabase/postgres
// container — NOT the process-shared Shared(t) instance — and returns
// its admin (supabase_admin, BYPASSRLS) connection WITHOUT running
// db.Migrate, alongside that same connection's DSN. The image's own
// init is complete (auth schema, vault extension, authenticator role),
// but the TF app schema has not been applied — the exact state a
// genuinely fresh install's control pod sees at boot.
//
// The DSN is returned so a caller can hand it to a genuinely separate
// OS process (e.g. via exec.Command re-invoking the test binary) —
// necessary for tests that need real cross-process concurrency rather
// than goroutines racing on this one process's copy of goose's
// package-level globals (goose.SetBaseFS / goose.SetDialect have no
// synchronization of their own, so two goroutines calling db.Migrate
// concurrently race on them regardless of what the pg_advisory_lock
// they end up contending on does — that lock coordinates the shared
// database across processes, not goose's in-memory config within one).
//
// Use this instead of Shared(t) when a test needs to observe or race
// the migration step itself — e.g. concurrent db.Migrate calls, or an
// executor-role assert against a not-yet-migrated schema. Shared(t)'s
// tf_test database is always already fully migrated by boot() before
// any test can touch it, so it can't reproduce that pre-migration
// state.
//
// Expensive: pays the full ~30-60s container boot on every call, with
// no sharing across tests. Reserve for tests that specifically need
// the pre-migration state. Terminates the container via t.Cleanup, so
// callers don't need to.
func NewInstance(t *testing.T) (adminDB *sql.DB, adminDSN string) {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pg, _, adminDSN, adminDB, err := bootContainer(ctx)
	if err != nil {
		t.Fatalf("pgtest.NewInstance: boot failed (Docker is reachable but bring-up errored): %v", err)
	}
	t.Cleanup(func() {
		_ = adminDB.Close()
		// Bounded, not context.Background(): an unbounded context here
		// would let a stuck Docker daemon or testcontainers/ryuk hang
		// this cleanup — and with it the whole test run — indefinitely.
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = pg.Terminate(termCtx)
	})
	return adminDB, adminDSN
}

// orgScopedTables is the closed list of tables Reset truncates. Order
// doesn't matter because CASCADE follows FK dependencies; the list is
// just the set of tables that hold tenant-derived rows.
var orgScopedTables = []string{
	// Tenancy:
	"sessions",
	"memberships",
	"org_memberships",
	"org_invites",
	"sso_break_glass",
	"sso_domains",
	"sso_connections",
	"teams",
	"orgs",
	// users + auth.users handled separately (auth.users is image-owned).
	// Settings:
	"org_settings", "team_settings", "user_settings", "jira_project_status_rules",
	"team_github_groups", "team_github_repos",
	"user_github_identities",
	"user_jira_identities",
	"org_secrets",
	// TF data:
	"swipe_events",
	"conversation_worktrees",
	"pending_firings",
	"artifacts",
	"conversation_memory", "conversation_memory_entities",
	"claim_credentials", "messages", "claims", "conversations",
	"task_events", "tasks",
	"event_handlers",
	"events", "entity_links", "entities",
	"repositories", "poller_state",
	"prompts",
	// marketplace_listings cascades into its version/event/vote/install
	// children via their own org_id-CASCADE FKs — only the parent needs
	// listing here.
	"marketplace_listings",
	// slack_event_deliveries carries no org_id column and no FK (keyed on
	// workspace_id text alone; see the migration), so unlike
	// org_slack_workspaces (which cascades transitively via its org_id FK
	// into orgs), it must be listed explicitly or Reset would leak
	// delivery-dedup rows across tests sharing this container.
	"slack_event_deliveries",
	// github_webhook_deliveries: same shape and same reason — keyed
	// (installation_id, delivery_id) with no org_id and no FK, deliberately,
	// so TRUNCATE CASCADE from orgs would never reach it and a leaked row
	// would make a later test's first delivery read as a redelivery.
	"github_webhook_deliveries",
	// slack_channels, team_slack_channels (TFAC-541): both carry an org_id
	// FK (cascading via orgs, and via teams for the latter) so TRUNCATE
	// CASCADE would reach them even unlisted; listed explicitly anyway,
	// matching the team_github_repos / team_github_groups convention above.
	"slack_channels", "team_slack_channels",
	// instances: carries no org_id column and no FK at all (a fleet
	// member isn't tenant data) — TRUNCATE CASCADE from orgs would
	// never reach it, so it must be listed explicitly or Reset would leak
	// registered-instance rows across tests sharing this container.
	"instances",
	// leases (TFAC-583): same shape as instances — no org_id, no FK — so
	// it must be listed explicitly or Reset would leak lease rows (and
	// their fencing terms) across tests sharing this container.
	"leases",
	// poll_readiness (TFAC-583): carries an org_id column but deliberately
	// no FK to orgs (admin-pool-only system state, same posture as
	// instances) — TRUNCATE CASCADE from orgs would never reach it, so it
	// must be listed explicitly too.
	"poll_readiness",
	// placement_overrides (TFAC-587): org_id column but no FK to orgs
	// (admin-pool-only system state, same posture as poll_readiness) — not
	// reached by CASCADE, so listed explicitly.
	"placement_overrides",
	// instance_stats + operators (TFAC-589): admin-pool-only system tables
	// with no FK to orgs (fleet telemetry samples / the deployment-operator
	// identity) — same posture as instances / placement_overrides, so they
	// must be listed explicitly or Reset would leak sample and operator rows
	// across tests sharing this container (the exact cross-subtest bleed the
	// instance_stat/operator conformance suites hit otherwise).
	"instance_stats", "operators",
	// sandbox_stats: same posture again — the per-sandbox resource series is
	// keyed on a bare claim_id uuid with deliberately no FK to claims, so
	// CASCADE never reaches it and Reset would otherwise leak samples across
	// tests sharing this container.
	"sandbox_stats",
	// users last — most other tables FK into it.
	"users",
	// NOT INCLUDED explicitly: events_catalog — it is a read-only system
	// registry seeded once and deliberately survives Reset.
}

// Reset truncates all org-scoped tables (CASCADE follows FKs into
// children we don't enumerate explicitly). auth.users is NOT cleared
// — that's image-owned and our users table cascades from it. Tests
// that seed auth.users via SeedAuthUser should call Reset *first*, then
// re-seed.
//
// Reset is also where a mid-suite container death surfaces in practice
// (it's the first DB touch of nearly every Postgres test), so a failed
// truncate is classified before failing the test: if a liveness probe
// on the same connection also fails, the container is dead — revive it
// and retry once. A live server answering the probe means the truncate
// error was a genuine SQL failure, which fails loudly as before.
func (h *Harness) Reset(t *testing.T) {
	t.Helper()
	err := h.truncateAll()
	if err == nil {
		return
	}

	sharedMu.Lock()
	pingErr := pingSharedLocked()
	var reviveErr error
	if pingErr != nil {
		reviveErr = reviveLocked(fmt.Errorf("%v (liveness probe: %v)", err, pingErr))
	}
	sharedMu.Unlock()

	if pingErr == nil {
		t.Fatalf("Reset: %v", err)
	}
	if reviveErr != nil {
		t.Fatalf("Reset: container died mid-suite and revive failed: %v", reviveErr)
	}
	if err := h.truncateAll(); err != nil {
		t.Fatalf("Reset (after revive): %v", err)
	}
}

// truncateAll is Reset's actual work. Bounded so a wedged server
// converts into a classifiable error instead of hanging the whole
// binary into go test's panic timeout.
func (h *Harness) truncateAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Build a single TRUNCATE statement so CASCADE works across all
	// tables at once (TRUNCATE on a single table can fail if another
	// table not in the list FKs into it; lumping them together avoids
	// that ordering issue).
	stmt := "TRUNCATE TABLE " + strings.Join(orgScopedTables, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := h.AdminDB.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("truncate org-scoped tables: %w", err)
	}
	// Drop auth.users rows we may have seeded. Image schema FKs from
	// public.users → auth.users(id) ON DELETE CASCADE, but the TRUNCATE
	// above goes the other direction. Wipe auth.users explicitly so
	// SeedAuthUser can re-insert the same IDs without conflict.
	if _, err := h.AdminDB.ExecContext(ctx, `DELETE FROM auth.users`); err != nil {
		return fmt.Errorf("clear auth.users: %w", err)
	}
	return nil
}

// SeedAuthUser inserts a row into auth.users with the given id + email.
// Used by RLS tests that need a valid users.id FK target without
// running GoTrue. Production rows from GoTrue are richer (encrypted
// password, last_sign_in_at, etc.); these are minimal stand-ins
// sufficient for FK satisfaction.
func (h *Harness) SeedAuthUser(t *testing.T, id, email string) {
	t.Helper()
	_, err := h.AdminDB.Exec(`
		INSERT INTO auth.users (id, email, instance_id, aud, role, created_at, updated_at)
		VALUES ($1, $2, '00000000-0000-0000-0000-000000000000', 'authenticated', 'authenticated', now(), now())
	`, id, email)
	if err != nil {
		t.Fatalf("SeedAuthUser %s: %v", id, err)
	}
}

// WithUser runs fn inside a transaction on AppDB, with the connection
// having been switched into tf_app and the JWT claims set to
// {sub: userID, org_id: orgID}. RLS policies see exactly the
// claims fn's queries should observe.
//
// fn returning a non-nil error rolls the transaction back. fn returning
// nil commits. The caller's *sql.Tx must not escape fn — using it after
// return is a use-after-commit race.
func (h *Harness) WithUser(t *testing.T, userID, orgID string, fn func(tx *sql.Tx) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.withUserCtx(ctx, userID, orgID, fn)
}

func (h *Harness) withUserCtx(ctx context.Context, userID, orgID string, fn func(tx *sql.Tx) error) error {
	tx, err := h.AppDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE tf_app`); err != nil {
		return fmt.Errorf("set role tf_app: %w", err)
	}

	claims := map[string]string{"sub": userID, "org_id": orgID}
	payload, err := json.Marshal(claims)
	if err != nil {
		return fmt.Errorf("marshal jwt claims: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('request.jwt.claims', $1, true)`, string(payload)); err != nil {
		return fmt.Errorf("set jwt claims: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// rewriteUser swaps the userinfo (user:password) component of a DSN.
// pgx accepts both URL-style and keyword=value DSNs; the supabase
// module returns URL-style, so we parse + rebuild.
func rewriteUser(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
