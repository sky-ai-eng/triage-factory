package projectbundle

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

type fakeProbe struct {
	cloneURLs map[string]string
	errs      map[string]error
}

func (p fakeProbe) CloneURLForRepo(_ context.Context, owner, repo string) (string, error) {
	slug := owner + "/" + repo
	if err, ok := p.errs[slug]; ok {
		return "", err
	}
	if cloneURL, ok := p.cloneURLs[slug]; ok {
		return cloneURL, nil
	}
	return "", fmt.Errorf("repo %s unreachable", slug)
}

func newBundleTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

type fixture struct {
	projectID string
}

func seedFixture(t *testing.T, database *sql.DB, projectName string) fixture {
	t.Helper()

	const slug = "sky-ai-eng/triage-factory"
	const cloneURL = "https://github.com/sky-ai-eng/triage-factory.git"

	if _, err := sqlitestore.New(database).Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{
		Owner:       "sky-ai-eng",
		Repo:        "triage-factory",
		CloneURL:    cloneURL,
		ProfiledAt:  ptrTime(time.Now().UTC()),
		Description: "fixture",
	}); err != nil {
		t.Fatalf("seed repository: %v", err)
	}

	created, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{
		Name:           projectName,
		Description:    "Fixture project",
		PinnedRepos:    []string{slug},
		JiraProjectKey: "SKY",
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	projectID := created.ID

	if _, err := paths.StateRootErr(); err != nil {
		t.Fatalf("resolve state root: %v", err)
	}
	kbDir := filepath.Join(paths.ProjectKBDir(runmode.LocalDefaultOrgID, projectID), "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir knowledge dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "notes.md"), []byte("# Notes\nkeep this"), 0o644); err != nil {
		t.Fatalf("write notes.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "diagram.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o644); err != nil {
		t.Fatalf("write diagram.png: %v", err)
	}

	return fixture{projectID: projectID}
}

func exportFixtureBundle(t *testing.T, database *sql.DB, projectID string) []byte {
	t.Helper()
	reader, err := Export(context.Background(), sqlitestore.New(database).Tx, nil, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, projectID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	return data
}

func buildZipEntries(t *testing.T, files map[string][]byte) map[string]*zip.File {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip reader: %v", err)
	}
	entries, err := indexZipEntries(zr.File)
	if err != nil {
		t.Fatalf("index zip entries: %v", err)
	}
	return entries
}

func TestImport_RoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sourceDB := newBundleTestDB(t)
	f := seedFixture(t, sourceDB, "Roundtrip source")
	bundleBytes := exportFixtureBundle(t, sourceDB, f.projectID)

	targetDB := newBundleTestDB(t)
	imported, warnings, err := Import(
		context.Background(),
		sqlitestore.New(targetDB).Tx,
		nil,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID,
		bytes.NewReader(bundleBytes),
		int64(len(bundleBytes)),
		fakeProbe{cloneURLs: map[string]string{"sky-ai-eng/triage-factory": "https://github.com/sky-ai-eng/triage-factory.git"}},
	)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	if imported.ID == f.projectID {
		t.Fatal("import should allocate a new project id")
	}

	if _, err := paths.StateRootErr(); err != nil {
		t.Fatalf("resolve state root: %v", err)
	}
	newRoot := paths.ProjectKBDir(runmode.LocalDefaultOrgID, imported.ID)
	notes, err := os.ReadFile(filepath.Join(newRoot, "knowledge-base", "notes.md"))
	if err != nil {
		t.Fatalf("read imported notes: %v", err)
	}
	if string(notes) != "# Notes\nkeep this" {
		t.Fatalf("imported notes mismatch: %q", string(notes))
	}

	// The imported pin must be tracked for the importing team, not just
	// brought into the repository registry. The router team↔repo gate keys
	// off the tracking table — without this row the team's handlers are
	// dropped for the repo and polled events create no tasks until the user
	// re-saves the selection.
	var tracked int
	if err := targetDB.QueryRow(`
		SELECT count(*) FROM team_github_repos g
		JOIN repositories r ON r.id = g.repository_id
		WHERE g.team_id = ? AND r.owner = ? AND r.repo = ?`,
		runmode.LocalDefaultTeamID, "sky-ai-eng", "triage-factory",
	).Scan(&tracked); err != nil {
		t.Fatalf("team_github_repos lookup: %v", err)
	}
	if tracked != 1 {
		t.Fatalf("imported pin not tracked for team: team_github_repos rows = %d, want 1", tracked)
	}
}

func TestImport_MissingReposAbortsWithoutWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sourceDB := newBundleTestDB(t)
	f := seedFixture(t, sourceDB, "Missing repo source")
	bundleBytes := exportFixtureBundle(t, sourceDB, f.projectID)

	targetDB := newBundleTestDB(t)
	_, _, err := Import(
		context.Background(),
		sqlitestore.New(targetDB).Tx,
		nil,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID,
		bytes.NewReader(bundleBytes),
		int64(len(bundleBytes)),
		fakeProbe{errs: map[string]error{"sky-ai-eng/triage-factory": errors.New("returned 404")}},
	)
	var missing *MissingReposError
	if !errors.As(err, &missing) {
		t.Fatalf("expected MissingReposError, got %v", err)
	}
	if len(missing.Missing) != 1 || missing.Missing[0].Repo != "sky-ai-eng/triage-factory" {
		t.Fatalf("unexpected missing repos payload: %+v", missing.Missing)
	}
	projects, _, err := sqlitestore.New(targetDB).Projects.List(t.Context(), runmode.LocalDefaultOrgID, db.ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("import should not create projects on preflight failure, got %d", len(projects))
	}
}

func TestImport_DuplicateNameAborts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sourceDB := newBundleTestDB(t)
	f := seedFixture(t, sourceDB, "Duplicate Name")
	bundleBytes := exportFixtureBundle(t, sourceDB, f.projectID)

	targetDB := newBundleTestDB(t)
	if _, err := sqlitestore.New(targetDB).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "Duplicate Name"}); err != nil {
		t.Fatalf("seed duplicate name: %v", err)
	}
	_, _, err := Import(
		context.Background(),
		sqlitestore.New(targetDB).Tx,
		nil,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID,
		bytes.NewReader(bundleBytes),
		int64(len(bundleBytes)),
		fakeProbe{cloneURLs: map[string]string{"sky-ai-eng/triage-factory": "https://github.com/sky-ai-eng/triage-factory.git"}},
	)
	var dup *DuplicateNameError
	if !errors.As(err, &dup) {
		t.Fatalf("expected DuplicateNameError, got %v", err)
	}
	projects, _, err := sqlitestore.New(targetDB).Projects.List(t.Context(), runmode.LocalDefaultOrgID, db.ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("duplicate-name import should not create rows, got %d", len(projects))
	}
}

func TestCopyZipEntryRaw_EnforcesTotalExtractionLimit(t *testing.T) {
	entries := buildZipEntries(t, map[string][]byte{
		knowledgePrefix + "big.bin": []byte("abcdef"),
	})
	zf := entries[knowledgePrefix+"big.bin"]
	if zf == nil {
		t.Fatal("missing knowledge entry")
	}
	dest := filepath.Join(t.TempDir(), "big.bin")
	err := copyZipEntryRaw(zf, dest, 0o644, newZipExtractionBudget(5, 32))
	if err == nil || !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("expected total-limit error, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destination file should not exist after limit failure; stat err=%v", statErr)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
