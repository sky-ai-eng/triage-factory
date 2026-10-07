package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/pressly/goose/v3"
)

// BootstrapSchemaForTest applies the full schema and seed data to db
// from a cached page image. Equivalent to Migrate followed by
// SeedLocalTenantRows, but the image is built once per process — each
// test pays one deserialize (a copy of ~1MB of pages) instead of
// running goose's full Up cycle, or even replaying the schema as DDL
// text.
//
// The image is captured by restoring the tenantless image (see
// BootstrapTenantlessSchemaForTest) into an in-memory template, seeding
// the synthetic local tenant, and asking SQLite for that database's
// serialized pages. Deserializing hands the target connection those
// bytes wholesale — tables, indexes, triggers, views, and every seeded
// row — so the fixture equals the template by construction, with no
// hand-maintained list of which tables are worth carrying across. The
// migration runner itself is still covered by migrations_test.go, which
// uses Migrate directly, and TestBootstrapSchemaForTest_MatchesMigrate
// pins this path against it.
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
	return restoreImage(db, image)
}

// BootstrapTenantlessSchemaForTest is BootstrapSchemaForTest without the
// synthetic local tenant: db ends up exactly as Migrate leaves an empty
// database — schema, goose_db_version, events_catalog, and no org, team
// or user rows. It is the genuine fresh-install shape, for tests of the
// paths that provision a tenant or that must see a database nothing has
// provisioned yet.
//
// Same connection requirements as BootstrapSchemaForTest. Tests-only.
func BootstrapTenantlessSchemaForTest(db *sql.DB) error {
	image, err := cachedTenantlessImage()
	if err != nil {
		return err
	}
	return restoreImage(db, image)
}

// restoreImage replaces the database behind db's connection with image.
func restoreImage(db *sql.DB, image []byte) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("check out connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	return conn.Raw(func(driverConn any) error {
		target, ok := driverConn.(deserializer)
		if !ok {
			return fmt.Errorf("restore page image: driver connection %T cannot restore a page image; "+
				"this fixture is built on modernc.org/sqlite's serialize/deserialize pair", driverConn)
		}
		// Deserialize copies image into SQLite-owned memory, so the
		// cached slice stays immutable and shareable across concurrent
		// tests.
		return target.Deserialize(image)
	})
}

// serializeImage returns the page image of the database behind db's
// connection.
func serializeImage(db *sql.DB) ([]byte, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("check out connection: %w", err)
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
		return nil, fmt.Errorf("serialize: %w", err)
	}
	if len(image) == 0 {
		return nil, fmt.Errorf("serialize: empty page image")
	}
	return image, nil
}

// serializer and deserializer are the two halves of the driver's
// page-image API. They are methods on modernc.org/sqlite's unexported
// connection type, so an interface assertion through (*sql.Conn).Raw is
// the only way to reach them.
type (
	serializer   interface{ Serialize() ([]byte, error) }
	deserializer interface{ Deserialize([]byte) error }
)

// openTemplate opens the private in-memory database an image is built
// in, optionally starting from an earlier image.
func openTemplate(base []byte) (*sql.DB, error) {
	template, err := sql.Open("sqlite", TestDSNMemory)
	if err != nil {
		return nil, fmt.Errorf("open template: %w", err)
	}
	template.SetMaxOpenConns(1)
	template.SetMaxIdleConns(1)
	if base != nil {
		if err := restoreImage(template, base); err != nil {
			_ = template.Close()
			return nil, fmt.Errorf("restore base image: %w", err)
		}
	}
	return template, nil
}

var (
	migratedImagesMu sync.Mutex
	migratedImages   = map[int64][]byte{}
)

// migratedImageAt returns the page image of an empty database after
// goose.UpTo(version) over the SQLite migration tree — goose alone, so
// none of what Migrate does after goose (events_catalog) and no tenant.
// Migration tests restore it in place of replaying the chain up to the
// version they seed at.
//
// Images are cached by version and built lazily: a build restores the
// highest cached image at or below version and applies only the
// migrations after it. The image carries goose_db_version, so goose
// continues from the restored version exactly as it would on the
// database the image was taken from.
//
// Every image is built under TestDSNMemory, foreign keys on. A restore
// keeps the target connection's own pragmas, so a connection opened
// with foreign keys off stays off.
func migratedImageAt(version int64) ([]byte, error) {
	migratedImagesMu.Lock()
	defer migratedImagesMu.Unlock()

	if image, ok := migratedImages[version]; ok {
		return image, nil
	}
	var base []byte
	baseVersion := int64(-1)
	for v, image := range migratedImages {
		if v < version && v > baseVersion {
			base, baseVersion = image, v
		}
	}

	template, err := openTemplate(base)
	if err != nil {
		return nil, err
	}
	defer template.Close()

	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		return nil, err
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(template, dir, version)
	}
	gooseMu.Unlock()
	if upErr != nil {
		return nil, fmt.Errorf("goose.UpTo(%d) on template: %w", version, upErr)
	}

	image, err := serializeImage(template)
	if err != nil {
		return nil, fmt.Errorf("image at %d: %w", version, err)
	}
	migratedImages[version] = image
	return image, nil
}

// sqliteHeadVersion is the highest version in the embedded SQLite
// migration tree.
func sqliteHeadVersion() (int64, error) {
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		return 0, err
	}
	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(treeFS)
	return headMigrationVersion(dir)
}

var (
	tenantlessImageOnce sync.Once
	tenantlessImage     []byte
	tenantlessImageErr  error

	schemaImageOnce sync.Once
	schemaImage     []byte
	schemaImageErr  error
)

func cachedTenantlessImage() ([]byte, error) {
	tenantlessImageOnce.Do(func() {
		tenantlessImage, tenantlessImageErr = buildTenantlessImage()
	})
	return tenantlessImage, tenantlessImageErr
}

func cachedSchemaImage() ([]byte, error) {
	schemaImageOnce.Do(func() {
		schemaImage, schemaImageErr = buildSchemaImage()
	})
	return schemaImage, schemaImageErr
}

// buildTenantlessImage is Migrate on an empty database, taken in two
// steps so the goose half lands in the version cache: the image at head,
// then Migrate over it, which finds nothing pending and does the rest.
func buildTenantlessImage() ([]byte, error) {
	head, err := sqliteHeadVersion()
	if err != nil {
		return nil, fmt.Errorf("head migration version: %w", err)
	}
	headImage, err := migratedImageAt(head)
	if err != nil {
		return nil, err
	}
	template, err := openTemplate(headImage)
	if err != nil {
		return nil, err
	}
	defer template.Close()

	if err := Migrate(template, "sqlite3"); err != nil {
		return nil, fmt.Errorf("migrate template: %w", err)
	}
	image, err := serializeImage(template)
	if err != nil {
		return nil, fmt.Errorf("tenantless image: %w", err)
	}
	return image, nil
}

func buildSchemaImage() ([]byte, error) {
	tenantless, err := cachedTenantlessImage()
	if err != nil {
		return nil, err
	}
	template, err := openTemplate(tenantless)
	if err != nil {
		return nil, err
	}
	defer template.Close()

	// Seed the synthetic local tenant into the template so the image
	// below captures it. Production no longer provisions at boot or in
	// the migration (provisioning is the explicit "Start your Triage
	// Factory" action via BootstrapLocalOrg) — but the vast majority of
	// store + handler tests assume a provisioned local install as their
	// fixture (inserting agents / team_agents / org_settings / etc. that
	// FK to these rows). BootstrapSchemaForTest therefore represents a
	// provisioned install; tests that want the genuine tenant-less boot
	// state use BootstrapTenantlessSchemaForTest instead.
	if err := SeedLocalTenantRows(context.Background(), template); err != nil {
		return nil, fmt.Errorf("seed local tenant into template: %w", err)
	}
	image, err := serializeImage(template)
	if err != nil {
		return nil, fmt.Errorf("schema image: %w", err)
	}
	return image, nil
}
