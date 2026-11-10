package db

import (
	"bytes"
	"embed"
	"io/fs"
	"testing"
)

// Embedded migrations must be byte-identical on every platform; a CR means a checkout converted line endings.
func TestEmbeddedMigrations_NoCR(t *testing.T) {
	t.Parallel()
	for name, fsys := range map[string]embed.FS{
		"migrations-sqlite":   migrationsSQLiteFS,
		"migrations-postgres": migrationsPostgresFS,
	} {
		files := 0
		err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			files++
			b, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			if bytes.IndexByte(b, '\r') >= 0 {
				t.Errorf("embedded %s contains a carriage return", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", name, err)
		}
		if files == 0 {
			t.Fatalf("no embedded files found in %s", name)
		}
	}
}
