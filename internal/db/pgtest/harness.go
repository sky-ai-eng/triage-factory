// Package pgtest spins up a supabase/postgres testcontainer for D3+
// Postgres-backed tests. The harness is shared per-process via sync.Once
// — the first test pays the ~5s boot cost; subsequent tests share the
// same container and call Reset between cases to TRUNCATE state.
//
// Two SQL connections are exposed:
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

// Harness is the shared per-process testcontainer + two connections.
// All tests that touch Postgres acquire it via Shared(t).
type Harness struct {
	Container *postgres.PostgresContainer
	AdminDB   *sql.DB // supabase_admin; bypasses RLS
	AppDB     *sql.DB // authenticator; use WithUser for RLS-active txns
}

var (
	sharedOnce sync.Once
	shared     *Harness
	sharedErr  error
)

// Shared returns the package-scoped harness, booting the container on
// the first call. Subsequent calls return the same instance. Tests
// that need isolation should call h.Reset(t) at the top of the test.
//
// Two outcomes are distinct on purpose:
//   - Docker is genuinely unreachable → t.Skip. Lets the SQLite suite
//     run cleanly in CI environments without a Docker daemon.
//   - Docker is healthy but boot failed (migration error, SQL bug,
//     image regression, anything else) → t.Fatalf. Treating these as
//     skips would let a real schema regression silently turn the
//     Postgres suite into a green-but-empty pass.
func Shared(t *testing.T) *Harness {
	t.Helper()
	// Probe Docker first so unreachable-daemon failures are
	// disambiguated from boot failures. The probe is cheap — pings the
	// Docker socket via the testcontainers provider — and runs on
	// every Shared() call (cheap is fine; sharedOnce guards the
	// expensive boot itself).
	testcontainers.SkipIfProviderIsNotHealthy(t)

	sharedOnce.Do(boot)
	if sharedErr != nil {
		t.Fatalf("pgtest: boot failed (Docker is reachable but bring-up errored — this is NOT a skip-worthy condition): %v", sharedErr)
	}
	return shared
}

func boot() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

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

	pg, err := postgres.Run(ctx, Image,
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
		),
		testcontainers.WithWaitStrategyAndDeadline(3*time.Minute, waitStrategies...),
	)
	if err != nil {
		sharedErr = fmt.Errorf("start container: %w", err)
		return
	}

	pgDSN, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pg.Terminate(ctx)
		sharedErr = fmt.Errorf("admin dsn: %w", err)
		return
	}

	// The supabase image demotes `postgres` to non-superuser during
	// init (see 10000000000000_demote-postgres.sql). The real
	// superuser is `supabase_admin`, whose password is set to
	// POSTGRES_PASSWORD by the image's migrate.sh. Connect as
	// supabase_admin for migrations + reserved-role ALTERs (the
	// supautils extension would otherwise reject "ALTER ROLE
	// authenticator" from a non-superuser).
	adminDSN, err := rewriteUser(pgDSN, "supabase_admin", "postgres")
	if err != nil {
		_ = pg.Terminate(ctx)
		sharedErr = fmt.Errorf("admin dsn rewrite: %w", err)
		return
	}
	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		_ = pg.Terminate(ctx)
		sharedErr = fmt.Errorf("open admin db: %w", err)
		return
	}

	// Run goose migrations as supabase_admin (real superuser). RLS is
	// bypassed here by design — migrations create roles, grant
	// defaults, install policies. Trying to do this as tf_app would
	// fail on the GRANT statements.
	if err := db.Migrate(adminDB, "postgres"); err != nil {
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		sharedErr = fmt.Errorf("migrate: %w", err)
		return
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
		sharedErr = fmt.Errorf("set authenticator password: %w", err)
		return
	}

	appDSN, err := rewriteUser(pgDSN, "authenticator", authPassword)
	if err != nil {
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		sharedErr = fmt.Errorf("build app dsn: %w", err)
		return
	}
	appDB, err := sql.Open("pgx", appDSN)
	if err != nil {
		_ = adminDB.Close()
		_ = pg.Terminate(ctx)
		sharedErr = fmt.Errorf("open app db: %w", err)
		return
	}

	shared = &Harness{Container: pg, AdminDB: adminDB, AppDB: appDB}
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
	"curator_pending_context", "curator_messages", "curator_requests",
	"swipe_events",
	"run_worktrees",
	"pending_firings",
	"artifacts",
	"run_memory", "run_messages", "runs",
	"task_events", "tasks",
	"event_handlers",
	"events", "entity_links", "entities",
	"project_knowledge", "projects",
	"repo_profiles", "poller_state",
	"prompts",
	// marketplace_listings cascades into its version/event/vote/install
	// children via their own org_id-CASCADE FKs, same as prompts →
	// system_prompt_versions below — only the parent needs listing here.
	"marketplace_listings",
	// slack_event_deliveries carries no org_id column and no FK (keyed on
	// workspace_id text alone; see the migration), so unlike
	// org_slack_workspaces (which cascades transitively via its org_id FK
	// into orgs), it must be listed explicitly or Reset would leak
	// delivery-dedup rows across tests sharing this container.
	"slack_event_deliveries",
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
	// users last — most other tables FK into it.
	"users",
	// NOT INCLUDED explicitly: system_prompt_versions, events_catalog.
	// events_catalog survives Reset, but system_prompt_versions does not:
	// TRUNCATE ... CASCADE reaches it transitively because it FK-depends
	// on prompts, which is truncated above.
}

// Reset truncates all org-scoped tables (CASCADE follows FKs into
// children we don't enumerate explicitly). auth.users is NOT cleared
// — that's image-owned and our users table cascades from it. Tests
// that seed auth.users via SeedAuthUser should call Reset *first*, then
// re-seed.
func (h *Harness) Reset(t *testing.T) {
	t.Helper()
	// Build a single TRUNCATE statement so CASCADE works across all
	// tables at once (TRUNCATE on a single table can fail if another
	// table not in the list FKs into it; lumping them together avoids
	// that ordering issue).
	stmt := "TRUNCATE TABLE " + strings.Join(orgScopedTables, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := h.AdminDB.Exec(stmt); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// Drop auth.users rows we may have seeded. Image schema FKs from
	// public.users → auth.users(id) ON DELETE CASCADE, but the TRUNCATE
	// above goes the other direction. Wipe auth.users explicitly so
	// SeedAuthUser can re-insert the same IDs without conflict.
	if _, err := h.AdminDB.Exec(`DELETE FROM auth.users`); err != nil {
		t.Fatalf("Reset auth.users: %v", err)
	}
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
