package projectbundle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestImportExport_MultiMode_Postgres is the end-to-end multi-mode
// conformance test for the bundle path: real Postgres via pgtest, TF_MODE=multi
// paths (org-scoped state root), and a cross-org export→import round trip —
// pinned repos tracked and clone URLs seeded under RLS, the knowledge base
// landing under the destination org's own state root, and the source org left
// untouched.
//
// Skips (via pgtest.Shared) when Docker is unavailable.
func TestImportExport_MultiMode_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv("TF_STATE_ROOT", t.TempDir())

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	srcOrg, srcUser, srcTeam := pgtest.SeedOrgWithUser(t, h, "exporter")

	const slug = "sky-ai-eng/triage-factory"

	var projectID string
	if err := stores.Tx.WithTx(ctx, srcOrg, srcUser, func(tx db.TxStores) error {
		created, e := tx.Projects.Create(ctx, srcOrg, srcTeam, domain.Project{
			Name:        "Multi source",
			Description: "multi fixture",
			PinnedRepos: []string{slug},
		})
		projectID = created.ID
		return e
	}); err != nil {
		t.Fatalf("create source project: %v", err)
	}

	// Knowledge-base file under the org-scoped state root.
	projectRoot := paths.ProjectKBDir(srcOrg, projectID)
	kbDir := filepath.Join(projectRoot, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "notes.md"), []byte("# multi notes"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	// Export under the exporting user's claims.
	preview, err := Preview(ctx, stores.Tx, nil, srcOrg, srcUser, projectID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(preview.Warnings) != 0 {
		t.Fatalf("unexpected preview warnings: %v", preview.Warnings)
	}

	reader, err := Export(ctx, stores.Tx, nil, srcOrg, srcUser, projectID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bundleBytes, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	// Import into a DIFFERENT org — the cross-tenant round trip.
	dstOrg, dstUser, dstTeam := pgtest.SeedOrgWithUser(t, h, "importer")
	imported, warnings, err := Import(
		ctx,
		stores.Tx,
		nil,
		dstOrg, dstTeam, dstUser,
		bytes.NewReader(bundleBytes),
		int64(len(bundleBytes)),
		fakeProbe{cloneURLs: map[string]string{slug: "https://github.com/sky-ai-eng/triage-factory.git"}},
	)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected import warnings: %+v", warnings)
	}
	if imported.ID == projectID {
		t.Fatal("import should allocate a new project id")
	}

	// KB file landed under the DESTINATION org's state root.
	newRoot := paths.ProjectKBDir(dstOrg, imported.ID)
	notes, err := os.ReadFile(filepath.Join(newRoot, "knowledge-base", "notes.md"))
	if err != nil {
		t.Fatalf("read imported notes: %v", err)
	}
	if string(notes) != "# multi notes" {
		t.Fatalf("imported notes = %q", notes)
	}

	// The pinned repo is tracked for the destination team (the
	// repo-selection-save semantic), and the clone URL got seeded into
	// the destination org's repositories.
	var tracked int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = $1 AND r.owner = 'sky-ai-eng' AND r.repo = 'triage-factory'
	`, dstTeam).Scan(&tracked); err != nil {
		t.Fatalf("count tracked: %v", err)
	}
	if tracked != 1 {
		t.Fatalf("imported pin not tracked for destination team: %d rows", tracked)
	}
	var cloneURL string
	if err := h.AdminDB.QueryRow(`
		SELECT COALESCE(clone_url, '') FROM repositories WHERE org_id = $1 AND owner = 'sky-ai-eng' AND repo = 'triage-factory'
	`, dstOrg).Scan(&cloneURL); err != nil {
		t.Fatalf("read repository: %v", err)
	}
	if cloneURL != "https://github.com/sky-ai-eng/triage-factory.git" {
		t.Fatalf("clone_url = %q, not seeded from preflight", cloneURL)
	}

	// The source org is untouched by the import: its project count is
	// still 1.
	var srcProjects int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM projects WHERE org_id = $1`, srcOrg).Scan(&srcProjects); err != nil {
		t.Fatalf("count src projects: %v", err)
	}
	if srcProjects != 1 {
		t.Fatalf("source org project count = %d after cross-org import, want 1", srcProjects)
	}
}
