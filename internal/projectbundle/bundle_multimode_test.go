package projectbundle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestImportExport_MultiMode_Postgres is the end-to-end multi-mode
// conformance test for the bundle path (TFAC-109): real Postgres via
// pgtest, TF_MODE=multi paths (org-scoped state root, sandbox-shaped
// session tree at <KB dir>/.claude/projects/-work), claims-bound store
// writes under RLS, and a cross-org export→import round trip.
//
// Skips (via pgtest.Shared) when Docker is unavailable, and on
// non-Linux where the sandbox path branch never activates.
func TestImportExport_MultiMode_Postgres(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("multi-mode sandbox session paths are linux-only")
	}
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv("TF_STATE_ROOT", t.TempDir())

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	srcOrg, srcUser, srcTeam := pgtest.SeedOrgWithUser(t, h, "exporter")

	const slug = "sky-ai-eng/triage-factory"
	const sessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Source project with one pinned repo; the curator session handle lands
	// on the exporter's conversation below.
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

	// Session transcript where the SANDBOXED curator actually wrote it:
	// <projectRoot>/.claude/projects/-work/<sessionID>.jsonl, with the
	// in-jail cwd ("/work") embedded — exactly what a multi-mode run
	// leaves behind.
	sessionDir, err := worktree.ClaudeProjectDir(projectRoot)
	if err != nil {
		t.Fatalf("resolve session dir: %v", err)
	}
	if !strings.HasPrefix(sessionDir, projectRoot) {
		t.Fatalf("multi-mode session dir %q must live under the project root %q", sessionDir, projectRoot)
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	transcript := `{"type":"user","message":{"content":"hello"},"cwd":"` + agentproc.SandboxWorkRoot + `","sessionId":"` + sessionID + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, sessionID+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// One completed curator turn (claims-bound message writes + System-door
	// claim writes, the production split) plus a pending injection.
	var convID string
	var msgID int64
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, srcOrg, srcUser, func(tx db.TxStores) error {
		conv, e := tx.Curator.GetOrCreateConversation(ctx, srcOrg, projectID, srcUser)
		if e != nil {
			return e
		}
		convID = conv.ID
		msgID, e = tx.Curator.EnqueueUserMessage(ctx, srcOrg, conv.ID, srcUser, "hi")
		return e
	}); err != nil {
		t.Fatalf("seed curator turn: %v", err)
	}
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "src-exec", 1, db.ClaimPlacement{})
	if err != nil || claimed == nil || claimed.ID != convID {
		t.Fatalf("claim turn = (%+v, %v), want conversation %s", claimed, err, convID)
	}
	claimID := claimed.ClaimID
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, srcOrg, srcUser, func(tx db.TxStores) error {
		if _, e := tx.Curator.BeginTurn(ctx, srcOrg, projectID, convID, msgID); e != nil {
			return e
		}
		if e := tx.Curator.SetSDKSession(ctx, srcOrg, convID, sessionID); e != nil {
			return e
		}
		_, e := tx.Conversations.InsertMessage(ctx, srcOrg, &domain.Message{
			ConversationID: convID, UserID: srcUser, ClaimID: claimID,
			Role: "assistant", Content: "ack", CreatedAt: time.Now().UTC(),
		})
		return e
	}); err != nil {
		t.Fatalf("run curator turn: %v", err)
	}
	if _, err := stores.Curator.ReleaseActiveTurnSystem(ctx, srcOrg, convID, "completed", "", 0.01, 10, 1); err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if err := stores.Curator.QueueContextChangeSystem(ctx, srcOrg, projectID, domain.ChangeTypePinnedRepos, `["`+slug+`"]`); err != nil {
		t.Fatalf("queue pending context: %v", err)
	}

	// Export under the exporting user's claims.
	preview, err := Preview(ctx, stores.Tx, nil, srcOrg, srcUser, projectID)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	var sawTranscript bool
	for _, f := range preview.Files {
		if f.Path == sessionTranscriptPath {
			sawTranscript = true
		}
	}
	if !sawTranscript {
		t.Fatalf("preview missing %s — multi-mode export did not find the sandbox-side transcript; files=%+v warnings=%v",
			sessionTranscriptPath, preview.Files, preview.Warnings)
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
		stores.Curator,
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
	var importedConv *domain.Conversation
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, dstOrg, dstUser, func(tx db.TxStores) error {
		c, e := tx.Curator.GetLiveConversation(ctx, dstOrg, imported.ID, dstUser)
		importedConv = c
		return e
	}); err != nil {
		t.Fatalf("read imported conversation: %v", err)
	}
	if importedConv == nil {
		t.Fatal("import did not restore the curator conversation")
	}
	newSessionID := importedConv.SessionID
	if imported.ID == projectID || newSessionID == sessionID || newSessionID == "" {
		t.Fatalf("import must mint fresh ids: project=%s session=%s", imported.ID, newSessionID)
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

	// Transcript landed where the destination org's SANDBOXED curator
	// will resume it, with the session id rewritten and the in-jail cwd
	// preserved.
	newSessionDir, err := worktree.ClaudeProjectDir(newRoot)
	if err != nil {
		t.Fatalf("resolve new session dir: %v", err)
	}
	if !strings.HasPrefix(newSessionDir, newRoot) {
		t.Fatalf("imported session dir %q must live under the new project root %q", newSessionDir, newRoot)
	}
	body, err := os.ReadFile(filepath.Join(newSessionDir, newSessionID+".jsonl"))
	if err != nil {
		t.Fatalf("read imported transcript: %v", err)
	}
	if strings.Contains(string(body), sessionID) {
		t.Fatalf("imported transcript still contains old session id: %s", body)
	}
	if !strings.Contains(string(body), `"cwd":"`+agentproc.SandboxWorkRoot+`"`) {
		t.Fatalf("imported transcript lost the in-jail cwd: %s", body)
	}

	// Curator history restored under the IMPORTING user's identity, and
	// visible to them under RLS.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, dstOrg, dstUser, func(tx db.TxStores) error {
		if importedConv.CreatorUserID != dstUser {
			t.Errorf("imported conversation creator = %s, want the importer %s", importedConv.CreatorUserID, dstUser)
		}
		claims, e := tx.Curator.ListClaims(ctx, dstOrg, importedConv.ID)
		if e != nil {
			return e
		}
		if len(claims) != 1 || claims[0].Outcome != "completed" {
			t.Errorf("imported claims = %+v, want one completed engagement", claims)
		}
		msgs, e := tx.Curator.ListConversationMessages(ctx, dstOrg, importedConv.ID, 0)
		if e != nil {
			return e
		}
		var pendingInjections int
		for _, m := range msgs {
			if m.Subtype == "injection:context" && m.Delivered != nil && !*m.Delivered {
				pendingInjections++
			}
			if m.UserID != "" && m.UserID != dstUser {
				t.Errorf("imported message user = %s, want the importer %s", m.UserID, dstUser)
			}
		}
		if pendingInjections != 1 {
			t.Errorf("imported pending injections = %d, want 1", pendingInjections)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify imported curator rows: %v", err)
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
	// still 1 and its curator rows still attribute to the exporter.
	var srcProjects int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM projects WHERE org_id = $1`, srcOrg).Scan(&srcProjects); err != nil {
		t.Fatalf("count src projects: %v", err)
	}
	if srcProjects != 1 {
		t.Fatalf("source org project count = %d after cross-org import, want 1", srcProjects)
	}
}
