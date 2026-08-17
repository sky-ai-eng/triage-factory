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

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type fakeProbe struct {
	cloneURLs map[string]string
	errs      map[string]error
}

type jsonLineFixture struct {
	ID string `json:"id"`
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
	projectID    string
	projectName  string
	sessionID    string
	resolvedRoot string
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

	sessionID := "11111111-2222-3333-4444-555555555555"
	projectID, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{
		Name:           projectName,
		Description:    "Fixture project",
		PinnedRepos:    []string{slug},
		JiraProjectKey: "SKY",
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	root, err := curator.KnowledgeDir(runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("resolve knowledge dir: %v", err)
	}
	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir knowledge dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "notes.md"), []byte("# Notes\nkeep this"), 0o644); err != nil {
		t.Fatalf("write notes.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "diagram.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o644); err != nil {
		t.Fatalf("write diagram.png: %v", err)
	}

	// One completed turn (conversation + delivered user message + claimed
	// engagement + streamed reply) plus a pending injection — the write set
	// SendMessage → dispatch produces.
	stores := sqlitestore.New(database)
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	ctx := context.Background()
	conv, err := stores.Curator.GetOrCreateConversation(ctx, org, projectID, user)
	if err != nil {
		t.Fatalf("mint conversation: %v", err)
	}
	msgID, err := stores.Curator.EnqueueUserMessage(ctx, org, conv.ID, user, "hello")
	if err != nil {
		t.Fatalf("enqueue turn: %v", err)
	}
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "fixture-exec", 1, db.ClaimPlacement{})
	if err != nil || claimed == nil || claimed.ID != conv.ID {
		t.Fatalf("claim turn = (%+v, %v), want conversation %s", claimed, err, conv.ID)
	}
	claimID := claimed.ClaimID
	if _, err := stores.Curator.BeginTurn(ctx, org, projectID, conv.ID, msgID); err != nil {
		t.Fatalf("begin turn: %v", err)
	}
	if err := stores.Curator.SetSDKSession(ctx, org, conv.ID, sessionID); err != nil {
		t.Fatalf("set sdk session: %v", err)
	}
	if _, err := stores.Conversations.InsertMessage(ctx, org, &domain.Message{
		ConversationID: conv.ID, UserID: user, ClaimID: claimID,
		Role: "assistant", Content: "done", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert curator message: %v", err)
	}
	if _, err := stores.Curator.ReleaseActiveTurnSystem(ctx, org, conv.ID, "completed", "", 0.12, 2400, 4); err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if err := stores.Curator.QueueContextChangeSystem(ctx, org, projectID, domain.ChangeTypePinnedRepos, `["sky-ai-eng/triage-factory"]`); err != nil {
		t.Fatalf("queue pending context: %v", err)
	}

	resolvedRoot := worktree.ResolveClaudeProjectCwd(root)
	encoded := worktree.EncodeClaudeProjectDir(resolvedRoot)
	claudeRoot := filepath.Join(os.Getenv("HOME"), ".claude", "projects", encoded)
	if err := os.MkdirAll(claudeRoot, 0o700); err != nil {
		t.Fatalf("mkdir claude root: %v", err)
	}
	transcript := filepath.Join(claudeRoot, sessionID+".jsonl")
	transcriptBody := strings.Join([]string{
		fmt.Sprintf(`{"type":"permission-mode","sessionId":"%s"}`, sessionID),
		fmt.Sprintf(`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","cwd":"%s","sessionId":"%s","compactMetadata":{"trigger":"manual"}}`, resolvedRoot, sessionID),
		fmt.Sprintf(`{"type":"user","message":{"content":"Summary block"},"cwd":"%s","sessionId":"%s"}`, resolvedRoot, sessionID),
		fmt.Sprintf(`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","cwd":"%s","sessionId":"%s","compactMetadata":{"trigger":"manual"}}`, resolvedRoot, sessionID),
		"",
	}, "\n")
	if err := os.WriteFile(transcript, []byte(transcriptBody), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	subagentDir := filepath.Join(claudeRoot, sessionID, "subagents")
	toolDir := filepath.Join(claudeRoot, sessionID, "tool-results")
	if err := os.MkdirAll(subagentDir, 0o700); err != nil {
		t.Fatalf("mkdir subagent dir: %v", err)
	}
	if err := os.MkdirAll(toolDir, 0o700); err != nil {
		t.Fatalf("mkdir tool-results dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subagentDir, "agent-one.jsonl"), []byte(fmt.Sprintf(`{"sessionId":"%s","cwd":"%s","agentId":"agent-one"}`+"\n", sessionID, resolvedRoot)), 0o600); err != nil {
		t.Fatalf("write subagent jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subagentDir, "agent-one.meta.json"), []byte(fmt.Sprintf(`{"cwd":"%s","sessionId":"%s"}`, resolvedRoot, sessionID)), 0o600); err != nil {
		t.Fatalf("write subagent meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "toolu_123.txt"), []byte(fmt.Sprintf("session=%s cwd=%s\n", sessionID, resolvedRoot)), 0o600); err != nil {
		t.Fatalf("write tool-results: %v", err)
	}

	return fixture{
		projectID:    projectID,
		projectName:  projectName,
		sessionID:    sessionID,
		resolvedRoot: resolvedRoot,
	}
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

func TestImport_RoundTripSessionTreeAndCompactions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sourceDB := newBundleTestDB(t)
	f := seedFixture(t, sourceDB, "Roundtrip source")
	bundleBytes := exportFixtureBundle(t, sourceDB, f.projectID)

	targetDB := newBundleTestDB(t)
	imported, warnings, err := Import(
		context.Background(),
		sqlitestore.New(targetDB).Tx,
		sqlitestore.New(targetDB).Curator,
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
	targetStores := sqlitestore.New(targetDB)
	importedConv, err := targetStores.Curator.GetLiveConversation(t.Context(), runmode.LocalDefaultOrgID, imported.ID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("read imported conversation: %v", err)
	}
	if importedConv == nil {
		t.Fatal("import did not restore the curator conversation")
	}
	newSessionID := importedConv.SessionID
	if newSessionID == "" || newSessionID == f.sessionID {
		t.Fatalf("expected fresh curator session id, got %q", newSessionID)
	}

	newRoot, err := curator.KnowledgeDir(runmode.LocalDefaultOrgID, imported.ID)
	if err != nil {
		t.Fatalf("resolve imported root: %v", err)
	}
	notes, err := os.ReadFile(filepath.Join(newRoot, "knowledge-base", "notes.md"))
	if err != nil {
		t.Fatalf("read imported notes: %v", err)
	}
	if string(notes) != "# Notes\nkeep this" {
		t.Fatalf("imported notes mismatch: %q", string(notes))
	}

	newResolved := worktree.ResolveClaudeProjectCwd(newRoot)
	newEncoded := worktree.EncodeClaudeProjectDir(newResolved)
	newTranscript := filepath.Join(home, ".claude", "projects", newEncoded, newSessionID+".jsonl")
	transcriptBody, err := os.ReadFile(newTranscript)
	if err != nil {
		t.Fatalf("read imported transcript: %v", err)
	}
	body := string(transcriptBody)
	if strings.Count(body, `"subtype":"compact_boundary"`) != 2 {
		t.Fatalf("expected two compact_boundary markers in imported transcript, got %d", strings.Count(body, `"subtype":"compact_boundary"`))
	}
	if strings.Contains(body, f.sessionID) {
		t.Fatalf("imported transcript still contains old session id %s", f.sessionID)
	}
	if !strings.Contains(body, newSessionID) {
		t.Fatalf("imported transcript missing new session id %s", newSessionID)
	}
	if strings.Contains(body, f.resolvedRoot) {
		t.Fatalf("imported transcript still contains old cwd %s", f.resolvedRoot)
	}
	if !strings.Contains(body, newResolved) {
		t.Fatalf("imported transcript missing rewritten cwd %s", newResolved)
	}

	subagentBody, err := os.ReadFile(filepath.Join(home, ".claude", "projects", newEncoded, newSessionID, "subagents", "agent-one.jsonl"))
	if err != nil {
		t.Fatalf("read imported subagent jsonl: %v", err)
	}
	if strings.Contains(string(subagentBody), f.sessionID) || strings.Contains(string(subagentBody), f.resolvedRoot) {
		t.Fatalf("subagent jsonl did not rewrite old session/cwd: %s", string(subagentBody))
	}
	toolBody, err := os.ReadFile(filepath.Join(home, ".claude", "projects", newEncoded, newSessionID, "tool-results", "toolu_123.txt"))
	if err != nil {
		t.Fatalf("read imported tool result: %v", err)
	}
	if strings.Contains(string(toolBody), f.sessionID) || strings.Contains(string(toolBody), f.resolvedRoot) {
		t.Fatalf("tool result did not rewrite old session/cwd: %s", string(toolBody))
	}

	// Conversation state round-tripped: one claim with its accounting, the
	// delivered user turn, the claim-attributed reply, and the pending
	// injection (still undelivered — a queued delta survives the trip).
	claims, err := targetStores.Curator.ListClaims(t.Context(), runmode.LocalDefaultOrgID, importedConv.ID)
	if err != nil {
		t.Fatalf("list imported claims: %v", err)
	}
	if len(claims) != 1 || claims[0].Outcome != "completed" {
		t.Fatalf("imported claims = %+v, want one completed engagement", claims)
	}
	msgs, err := targetStores.Curator.ListConversationMessages(t.Context(), runmode.LocalDefaultOrgID, importedConv.ID, 0)
	if err != nil {
		t.Fatalf("list imported messages: %v", err)
	}
	var userTurns, replies, pendingInjections int
	for _, m := range msgs {
		switch {
		case m.Role == "user" && m.Subtype == "":
			userTurns++
		case m.Role == "assistant":
			replies++
			if m.ClaimID != claims[0].ID {
				t.Errorf("imported reply claim = %q, want remapped %q", m.ClaimID, claims[0].ID)
			}
		case m.Subtype == "injection:context":
			pendingInjections++
			if m.Delivered == nil || *m.Delivered {
				t.Errorf("imported injection row delivered = %v, want undelivered", m.Delivered)
			}
		}
	}
	if userTurns != 1 || replies != 1 || pendingInjections != 1 {
		t.Fatalf("imported transcript shape = (turns=%d, replies=%d, injections=%d), want (1,1,1): %+v", userTurns, replies, pendingInjections, msgs)
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
		sqlitestore.New(targetDB).Curator,
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
		sqlitestore.New(targetDB).Curator,
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

func TestDecodeZipJSONLines_EnforcesRowLimit(t *testing.T) {
	entries := buildZipEntries(t, map[string][]byte{
		curatorClaimsPath: []byte(`{"id":"r1"}` + "\n" + `{"id":"r2"}` + "\n"),
	})
	zf := entries[curatorClaimsPath]
	if zf == nil {
		t.Fatal("missing curator requests entry")
	}
	var seen int
	err := decodeZipJSONLines(
		zf,
		1<<20,
		1,
		func(row jsonLineFixture) error {
			seen++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "row limit") {
		t.Fatalf("expected row-limit error, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected callback to run once before row-limit failure, got %d", seen)
	}
}

func TestDecodeZipJSONLines_EnforcesByteLimit(t *testing.T) {
	entries := buildZipEntries(t, map[string][]byte{
		curatorMessagesPath: []byte(`{"id":"message-that-is-longer-than-limit"}` + "\n"),
	})
	zf := entries[curatorMessagesPath]
	if zf == nil {
		t.Fatal("missing curator messages entry")
	}
	err := decodeZipJSONLines[jsonLineFixture](
		zf,
		8,
		10,
		func(jsonLineFixture) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "8-byte limit") {
		t.Fatalf("expected byte-limit error, got %v", err)
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

func TestCopyZipEntryRewritten_EnforcesPerFileLimit(t *testing.T) {
	entries := buildZipEntries(t, map[string][]byte{
		sessionTranscriptPath: []byte("abcdef"),
	})
	zf := entries[sessionTranscriptPath]
	if zf == nil {
		t.Fatal("missing session transcript entry")
	}
	dest := filepath.Join(t.TempDir(), "transcript.jsonl")
	err := copyZipEntryRewritten(
		zf,
		dest,
		[]byteReplacement{},
		0o600,
		newZipExtractionBudget(64, 5),
	)
	if err == nil || !strings.Contains(err.Error(), "5-byte limit") {
		t.Fatalf("expected per-file limit error, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destination file should not exist after limit failure; stat err=%v", statErr)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
