package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
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
		adminDSN := os.Getenv("TF_DATABASE_URL")
		if adminDSN == "" {
			return errors.New("TF_MODE=multi requires TF_DATABASE_URL")
		}
		authPassword := os.Getenv("TF_AUTHENTICATOR_PASSWORD")
		if authPassword == "" {
			return errors.New("TF_MODE=multi requires TF_AUTHENTICATOR_PASSWORD")
		}
		adminDB, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return fmt.Errorf("open admin DB: %w", err)
		}
		applyPGPoolDefaults(adminDB)
		if err := adminDB.Ping(); err != nil {
			adminDB.Close()
			return fmt.Errorf("ping admin DB: %w", err)
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
		applyPGPoolDefaults(appDB)
		if err := appDB.Ping(); err != nil {
			appDB.Close()
			adminDB.Close()
			return fmt.Errorf("ping app DB: %w", err)
		}
		// App-layer AES-256-GCM key for org/user integration secrets
		// (TFAC-402). Required in multi mode — fail fast on a missing or
		// malformed key, mirroring the session-key load in httpserver.go.
		secretKey, err := aead.LoadKeyFromEnv(pgstore.EnvSecretEncryptionKey)
		if err != nil {
			appDB.Close()
			adminDB.Close()
			return fmt.Errorf("load secret encryption key: %w", err)
		}
		// Legacy *sql.DB consumers route to the admin pool for
		// system-service reads (no JWT-claims context). Close closes both.
		a.database = adminDB
		a.appDB = appDB
		a.stores = pgstore.New(adminDB, appDB, secretKey)

		// Start the cap-broker BEFORE ReapOrphans below, when TF_PRIVSEP=1,
		// so the boot-time reap sweep — like every other privileged sandbox
		// op for the rest of this process's life — routes through the
		// broker rather than running in-process. No-op (and no error) when
		// the flag is off or this host never sandboxes.
		if err := a.startCapBrokerIfEnabled(ctx); err != nil {
			appDB.Close()
			adminDB.Close()
			return fmt.Errorf("cap-broker: %w", err)
		}

		// Best-effort startup cleanup of orphaned sandboxes from a prior
		// hard-crashed TF process. Never fatal — failure here just means
		// orphaned resources stick around until the next boot.
		if err := sandbox.ReapOrphans(ctx); err != nil {
			sandboxLog.Warn("reap orphans at boot failed", "error", err)
		}

	default:
		return fmt.Errorf("unknown runmode: %v", runmode.Current())
	}

	return a.readInstanceConfig(ctx)
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

// applyPGPoolDefaults sets connection-pool ceilings on a Postgres *sql.DB.
// database/sql's default MaxOpenConns is unlimited, which can exhaust
// Postgres' max_connections under load — and multi-mode opens two pools
// against the same server, so the budget per pool must leave room for the
// other. These are conservative defaults that fit a default
// supabase/postgres install; operators tuning production should raise them
// alongside Postgres' max_connections.
func applyPGPoolDefaults(d *sql.DB) {
	d.SetMaxOpenConns(25)
	d.SetMaxIdleConns(25)
	d.SetConnMaxLifetime(5 * time.Minute)
}
