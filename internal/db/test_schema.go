package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// BootstrapSchemaForTest applies the full schema and seed data to db
// from a cached page image. Equivalent to Migrate, but the image is
// built once per process — each test pays one deserialize (a copy of
// ~1MB of pages) instead of running goose's full Up cycle, or even
// replaying the schema as DDL text.
//
// The image is captured by running Migrate against a fresh in-memory
// template, seeding the synthetic local tenant, and asking SQLite for
// that database's serialized pages. Deserializing hands the target
// connection those bytes wholesale — tables, indexes, triggers, views,
// and every seeded row — so the fixture equals the template by
// construction, with no hand-maintained list of which tables are worth
// carrying across. The migration runner itself is still covered by
// migrations_test.go, which uses Migrate directly, and
// TestBootstrapSchemaForTest_MatchesMigrate pins this path against it.
//
// Deserializing replaces the database behind one connection, so db is
// expected to be an in-memory handle pinned to a single connection —
// the shape every caller already uses, because :memory: gives each
// connection its own private database regardless.
//
// Tests-only. Production code uses Migrate.
func BootstrapSchemaForTest(db *sql.DB) error {
	image, err := cachedSchemaImage()
	if err != nil {
		return err
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("check out connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	return conn.Raw(func(driverConn any) error {
		target, ok := driverConn.(deserializer)
		if !ok {
			return fmt.Errorf("bootstrap schema: driver connection %T cannot restore a page image; "+
				"this fixture is built on modernc.org/sqlite's serialize/deserialize pair", driverConn)
		}
		// Deserialize copies image into SQLite-owned memory, so the
		// cached slice stays immutable and shareable across concurrent
		// tests.
		return target.Deserialize(image)
	})
}

// serializer and deserializer are the two halves of the driver's
// page-image API. They are methods on modernc.org/sqlite's unexported
// connection type, so an interface assertion through (*sql.Conn).Raw is
// the only way to reach them.
type (
	serializer   interface{ Serialize() ([]byte, error) }
	deserializer interface{ Deserialize([]byte) error }
)

var (
	schemaImageOnce sync.Once
	schemaImage     []byte
	schemaImageErr  error
)

func cachedSchemaImage() ([]byte, error) {
	schemaImageOnce.Do(func() {
		schemaImage, schemaImageErr = buildSchemaImage()
	})
	return schemaImage, schemaImageErr
}

func buildSchemaImage() ([]byte, error) {
	template, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open template: %w", err)
	}
	defer template.Close()
	template.SetMaxOpenConns(1)
	template.SetMaxIdleConns(1)

	if err := Migrate(template, "sqlite3"); err != nil {
		return nil, fmt.Errorf("migrate template: %w", err)
	}

	// Seed the synthetic local tenant into the template so the image
	// below captures it. Production no longer provisions at boot or in
	// the migration (provisioning is the explicit "Start your Triage
	// Factory" action via BootstrapLocalOrg) — but the vast majority of
	// store + handler tests assume a provisioned local install as their
	// fixture (inserting agents / team_agents / org_settings / etc. that
	// FK to these rows). BootstrapSchemaForTest therefore represents a
	// provisioned install; tests that want the genuine tenant-less boot
	// state use Migrate directly instead.
	if err := SeedLocalTenantRows(context.Background(), template); err != nil {
		return nil, fmt.Errorf("seed local tenant into template: %w", err)
	}

	conn, err := template.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("check out template connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var image []byte
	if err := conn.Raw(func(driverConn any) error {
		source, ok := driverConn.(serializer)
		if !ok {
			return fmt.Errorf("driver connection %T cannot produce a page image; "+
				"this fixture is built on modernc.org/sqlite's serialize/deserialize pair", driverConn)
		}
		var err error
		image, err = source.Serialize()
		return err
	}); err != nil {
		return nil, fmt.Errorf("serialize template: %w", err)
	}
	if len(image) == 0 {
		return nil, fmt.Errorf("serialize template: empty page image")
	}
	return image, nil
}
