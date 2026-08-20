package projectbundle

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestBundle_MultiMode_StoreRoundTrip proves that in multi mode a project's
// knowledge base exports FROM the blob store and imports back INTO it — the
// control pod hosts no KB on disk, so both directions must go through kbstore.
func TestBundle_MultiMode_StoreRoundTrip(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()

	org := runmode.LocalDefaultOrgID
	team := runmode.LocalDefaultTeamID
	user := runmode.LocalDefaultUserID

	// Source project with KB only in the store (no pinned repos, no session).
	srcDB := newBundleTestDB(t)
	srcStores := sqlitestore.New(srcDB)
	created, err := srcStores.Projects.Create(ctx, org, team, domain.Project{Name: "Store RT"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID := created.ID
	srcKB := kbstore.New(storage.NewFS(t.TempDir()))
	kbFiles := map[string]string{"design.md": "# design", "diagram.bin": "\x00\x01\x02binary"}
	for name, body := range kbFiles {
		if err := srcKB.Put(ctx, org, projectID, name, bytes.NewReader([]byte(body))); err != nil {
			t.Fatalf("seed kb %q: %v", name, err)
		}
	}

	// Export must pull the KB from the store, not disk.
	reader, err := Export(ctx, srcStores.Tx, srcKB, org, user, projectID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bundleBytes, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	// The zip carries the KB entries under knowledge-base/.
	zr, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	inZip := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		inZip[f.Name] = string(b)
	}
	for name, body := range kbFiles {
		if got := inZip["knowledge-base/"+name]; got != body {
			t.Fatalf("zip entry knowledge-base/%s = %q; want %q", name, got, body)
		}
	}

	// Import into a fresh DB + fresh store: the KB must land in the store.
	dstDB := newBundleTestDB(t)
	dstStores := sqlitestore.New(dstDB)
	dstKB := kbstore.New(storage.NewFS(t.TempDir()))
	imported, warnings, err := Import(
		ctx,
		dstStores.Tx,
		dstStores.Curator,
		dstKB,
		org, team, user,
		bytes.NewReader(bundleBytes),
		int64(len(bundleBytes)),
		fakeProbe{},
	)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}

	list, err := dstKB.List(ctx, org, imported.ID)
	if err != nil {
		t.Fatalf("list imported kb: %v", err)
	}
	if len(list) != len(kbFiles) {
		t.Fatalf("imported kb has %d files; want %d (%+v)", len(list), len(kbFiles), list)
	}
	for name, body := range kbFiles {
		rc, err := dstKB.Get(ctx, org, imported.ID, name)
		if err != nil {
			t.Fatalf("get imported %q: %v", name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != body {
			t.Fatalf("imported %q = %q; want %q", name, b, body)
		}
	}
}
