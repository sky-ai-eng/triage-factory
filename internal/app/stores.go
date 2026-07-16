package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
)

// openStores opens the right backend for the runtime mode, wires the
// per-resource store bundle against it, and reads the boot-time
// instance_config (local only). A misconfigured TF_MODE=multi fails fast
// here — without this guard the local SQLite file would be created and
// migrated before the multi branch could reject.
//
// Multi mode is unreachable end-to-end until the v1 multi-tenant epic
// completes; the error on an unknown mode makes that explicit instead of
// surfacing later as a pile of confusing SQL failures.
func (a *App) openStores(ctx context.Context) error {
	switch runmode.Current() {
	case runmode.ModeLocal:
		database, err := db.Open()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		if err := db.Migrate(database, "sqlite3"); err != nil {
			database.Close()
			return fmt.Errorf("migrate database: %w", err)
		}
		// No tenant rows exist on a fresh local DB — provisioning is the
		// explicit "Start your factory" action (db.BootstrapLocalOrg via
		// POST /api/setup/start), not a boot- or migration-time side
		// effect. The server, pollers, scorer, router, and spawner all
		// start and idle cleanly with zero tenant rows.
		a.database = database
		a.stores = sqlitestore.New(database)

	case runmode.ModeMulti:
		// Multi-mode boot wires two Postgres pools against the same
		// server. admin (superuser) handles migrations + system-service
		// reads + tenant bootstrap; app (authenticator → tf_app) handles
		// RLS-active request handlers. The admin DSN comes in whole via
		// TF_DATABASE_URL; the app DSN reuses host/db/options but swaps
		// userinfo to authenticator + its own password (set out-of-band by
		// the postgres-postinit sidecar). Two passwords by design — see
		// CLAUDE.md and the postgres-postinit service in docker-compose.yml.
		adminDSN := secretenv.Get("TF_DATABASE_URL")
		if adminDSN == "" {
			return errors.New("TF_MODE=multi requires TF_DATABASE_URL")
		}
		// The dedicated LISTEN connections (the shared tf_ctl listener in
		// ctl.go, the WS backplane's tf_ws/tf_bus listeners) resolve their
		// own session-mode DSN via wsbackplane.DirectDSN — a pooled *sql.DB
		// can't hold a session-mode LISTEN, so no raw-DSN field is kept here.
		authPassword := secretenv.Get("TF_AUTHENTICATOR_PASSWORD")
		if authPassword == "" {
			return errors.New("TF_MODE=multi requires TF_AUTHENTICATOR_PASSWORD")
		}
		adminDB, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return fmt.Errorf("open admin DB: %w", err)
		}
		applyPGPoolDefaultsForRole(adminDB, a.plan.role)
		if err := adminDB.Ping(); err != nil {
			adminDB.Close()
			return fmt.Errorf("ping admin DB: %w", err)
		}
		if a.plan.role == runmode.RoleExecutor {
			warnIfExecutorAdminDBIsSuperuser(ctx, adminDB)
		}
		appDSN, err := db.RewriteDSNCreds(adminDSN, "authenticator", authPassword)
		if err != nil {
			adminDB.Close()
			return fmt.Errorf("derive app DSN: %w", err)
		}
		appDB, err := sql.Open("pgx", appDSN)
		if err != nil {
			adminDB.Close()
			return fmt.Errorf("open app DB: %w", err)
		}
		applyPGPoolDefaultsForRole(appDB, a.plan.role)
		if err := appDB.Ping(); err != nil {
			appDB.Close()
			adminDB.Close()
			return fmt.Errorf("ping app DB: %w", err)
		}
		// App-layer AES-256-GCM key for org/user integration secrets
		// (TFAC-402). Required in every role EXCEPT executor (TFAC-614):
		// an executor never holds the secret-decryption key — all per-run
		// credential material arrives pre-resolved via sealed
		// run_credentials bundles instead (see internal/credprovision and
		// the awaiting-credentials wait in internal/delegate). If the var
		// is set on an executor anyway (a stale compose file, an operator
		// override) it is logged once and ignored, never loaded — the
		// property this exists to guarantee is "the key never enters this
		// process," not "the key is unused."
		a.database = adminDB
		a.appDB = appDB
		if a.plan.role == runmode.RoleExecutor {
			if secretenv.Get(pgstore.EnvSecretEncryptionKey) != "" {
				appLog.Warn("TF_SECRET_ENCRYPTION_KEY is set but ignored on TF_ROLE=executor — executors never hold the secret-decryption key; per-run credentials arrive via sealed bundles instead (TFAC-614)")
			}
			a.stores = pgstore.NewWithoutSecrets(adminDB, appDB)
		} else {
			secretKey, err := aead.LoadKeyFromEnv(pgstore.EnvSecretEncryptionKey)
			if err != nil {
				appDB.Close()
				adminDB.Close()
				return fmt.Errorf("load secret encryption key: %w", err)
			}
			a.stores = pgstore.New(adminDB, appDB, secretKey)
		}

		// Schema handling, in-process at boot (not just via the container
		// entrypoint's `migrate up`, which a k8s command: override / systemd
		// unit / multi-mode-dev.sh run would skip). db.Migrate routes by role:
		// a control/all process runs goose.Up under the advisory lock (M
		// booting pods serialize); a TF_ROLE=executor process runs NO DDL and
		// instead asserts the connected schema matches its build, refusing to
		// boot on a behind/ahead mismatch (TFAC-580). Runs against the admin
		// pool, before ReapOrphans needs the schema to exist.
		if err := db.Migrate(adminDB, "postgres"); err != nil {
			appDB.Close()
			adminDB.Close()
			return fmt.Errorf("migrate/assert schema: %w", err)
		}

		// Start the cap-broker BEFORE ReapOrphans below, on any host that
		// sandboxes, so the boot-time reap sweep — like every other
		// privileged sandbox op for the rest of this process's life —
		// routes through the broker. Fatal if the broker can't start (the
		// broker is the only sandbox path, so there is no in-process
		// fallback); a no-op when this host never sandboxes.
		if err := a.startCapBrokerIfSandboxing(ctx); err != nil {
			appDB.Close()
			adminDB.Close()
			return fmt.Errorf("cap-broker: %w", err)
		}

		// Best-effort startup cleanup of orphaned sandboxes from a prior
		// hard-crashed TF process. Never fatal — failure here just means
		// orphaned resources stick around until the next boot.
		//
		// Skipped on a control pod: it never launches a sandbox, so it has
		// nothing of its own to reap, and it holds no broker to route the
		// (privileged) reap through — running it would only log EPERM noise
		// from the capless orchestrator. Any pre-upgrade leftovers from a
		// container that once sandboxed are container-scoped (netns, veth,
		// iptables) and die with the container's recreation.
		if a.plan.role != runmode.RoleControl {
			if err := sandbox.ReapOrphans(ctx); err != nil {
				sandboxLog.Warn("reap orphans at boot failed", "error", err)
			}
		}

	default:
		return fmt.Errorf("unknown runmode: %v", runmode.Current())
	}

	return a.readInstanceConfig(ctx)
}

// warnIfExecutorAdminDBIsSuperuser checks whether this executor's admin
// pool authenticated as a Postgres superuser (rolsuper) — the posture the
// least-privilege tf_system role exists to avoid. It never fails boot: the
// supported deployment path (docker-compose.yml's `scale` profile) always
// wires an executor's TF_DATABASE_URL to tf_system, so this only fires for
// a bare-metal / custom-orchestrator executor whose operator pointed it at
// a superuser DSN (e.g. reusing a dev control pod's connection string).
// That's a real misconfiguration worth flagging loudly, but not one this
// process should brick over — hence one ERROR log with remediation,
// then continue. Control/all pods are unaffected: connecting as
// supabase_admin there is by design (they run migrations).
func warnIfExecutorAdminDBIsSuperuser(ctx context.Context, adminDB *sql.DB) {
	var isSuperuser bool
	if err := adminDB.QueryRowContext(ctx,
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuperuser); err != nil {
		appLog.Warn("executor DB posture check failed; continuing", "error", err)
		return
	}
	if isSuperuser {
		appLog.Error("executor admin DB pool is connected as a Postgres superuser — " +
			"executor should connect as tf_system, the least-privilege role, not a superuser DSN; " +
			"see docs/self-hosting/scaling.md#executor-database-role-tf_system")
	}
}

// readInstanceConfig loads the small remainder of process-wide boot state
// (server port) from the local instance_config table. The table is
// local-only — hosted multi-mode uses env vars for these — so the read is
// skipped in multi mode and the port falls back to the default.
//
// The stored port is surfaced to the Settings GET response; the actual
// bind still wins from --port at boot.
func (a *App) readInstanceConfig(ctx context.Context) error {
	a.storedPort = DefaultPort
	if !a.local() {
		return nil
	}

	var storedPort int
	if err := a.database.QueryRowContext(ctx,
		`SELECT server_port FROM instance_config WHERE id = 1`,
	).Scan(&storedPort); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read instance_config: %w", err)
	}
	// Default to the binary's DefaultPort when the row is missing or holds
	// the zero value — that's what the Settings GET response should render.
	if storedPort == 0 {
		storedPort = DefaultPort
	}
	a.storedPort = storedPort
	return nil
}

// Per-role Postgres pool ceilings (per pool; multi mode opens two — the
// admin and app pools — against the same server). database/sql's default
// MaxOpenConns is unlimited, which can exhaust Postgres' max_connections
// under load, so every pool is capped.
//
// The API tier (control/all) keeps the historical 25×2; an executor ships
// much smaller ceilings — its dispatcher claim loop, heartbeat, and run
// sinks need a fraction of what the API tier's request concurrency does,
// and its app pool is effectively idle (no RLS request handlers run on an
// executor). This is what keeps total fleet connection demand fitting a
// documented max_connections: at these defaults a 1-control + 2-executor
// profile is 25×2 + 2×(6×2) = 74 connections. See TF_DB_MAX_OPEN_CONNS in
// .env.example and the max_connections arithmetic in
// docs/self-hosting/scaling.md.
const (
	defaultPoolMaxConns         = 25
	defaultExecutorPoolMaxConns = 6
	// minPoolMaxConns floors the ceiling at 2. runPostgresMigrationLocked
	// holds one pool connection (the advisory lock) while goose.Up checks
	// out another for DDL, so a control/all pool of 1 would self-deadlock at
	// migrate time; 2 is the smallest safe value even for an executor.
	minPoolMaxConns = 2
)

// poolMaxConnsForRole resolves this pool's MaxOpenConns ceiling: the role
// default, overridden by TF_DB_MAX_OPEN_CONNS when set to a non-negative
// integer, and floored at minPoolMaxConns (so 0 or 1 select the minimum, 2 —
// deliberately NOT the database/sql "0 = unlimited" convention, which is the
// opposite of the capping this exists to do). A negative or non-numeric value
// logs and falls back to the role default rather than bricking boot.
func poolMaxConnsForRole(role runmode.DeployRole) int {
	def := defaultPoolMaxConns
	if role == runmode.RoleExecutor {
		def = defaultExecutorPoolMaxConns
	}
	n := def
	if raw := strings.TrimSpace(os.Getenv("TF_DB_MAX_OPEN_CONNS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			n = parsed
		} else {
			appLog.Warn("invalid TF_DB_MAX_OPEN_CONNS (want a non-negative integer); using role default", "value", raw, "default", def)
		}
	}
	if n < minPoolMaxConns {
		n = minPoolMaxConns
	}
	return n
}

// applyPGPoolDefaultsForRole sets the per-role connection-pool ceilings on
// a Postgres *sql.DB. Operators tuning production raise TF_DB_MAX_OPEN_CONNS
// alongside Postgres' max_connections.
func applyPGPoolDefaultsForRole(d *sql.DB, role runmode.DeployRole) {
	n := poolMaxConnsForRole(role)
	d.SetMaxOpenConns(n)
	d.SetMaxIdleConns(n)
	d.SetConnMaxLifetime(5 * time.Minute)
}
