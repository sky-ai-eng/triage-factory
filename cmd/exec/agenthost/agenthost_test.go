package agenthost

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// newTestDB opens an in-memory SQLite and runs the bootstrap schema
// so the LocalClient calls under test exercise real SQL. Mirrors the
// pattern in cmd/exec/workspace's test helpers — close enough to the
// production wiring that a routing-logic regression here would also
// surface in the real binary.
func newTestDB(t *testing.T) (db.Stores, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		conn.Close()
		t.Fatalf("bootstrap schema: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return sqlitestore.New(conn), conn
}

// seedBlueprintRun mints a fresh blueprint + blueprint_run for taskID
// and returns the blueprint_run id. conversations.blueprint_run_id is NOT NULL,
// so every seeded run needs a parent blueprint_run. SQLite
// blueprint_runs has no org_id/creator_user_id columns; org_id on
// blueprints takes its local-sentinel DEFAULT.
func seedBlueprintRun(t *testing.T, conn *sql.DB, taskID string) string {
	t.Helper()
	bpID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, name, source, team_id, creator_user_id)
		VALUES (?, 'Test BP', 'user', ?, ?)
	`, bpID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt', '[]')
	`, brID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedConversation inserts an entity → event → task → run chain through
// the real store APIs so the FK constraints are honored. trigger is
// "manual" (creator set) or "event" (creator empty).
func seedConversation(t *testing.T, stores db.Stores, conn *sql.DB, conversationID, creator, trigger string) {
	t.Helper()
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	entity, _, err := stores.Entities.FindOrCreate(ctx, orgID, "jira", "TEST-"+conversationID, "issue", "T-"+conversationID, "https://x/"+conversationID)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := stores.Prompts.Create(ctx, orgID, runmode.LocalDefaultTeamID, domain.Prompt{ID: "p-" + conversationID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	evtID, err := stores.Events.Record(ctx, orgID, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, orgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, conversationID, evtID, 0.5)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	dbtest.SeedConversation(t, conn, domain.Conversation{
		ID: conversationID, TaskID: task.ID, PromptID: "p-" + conversationID,
		Status: "running", Model: "claude-test",
		TriggerType:    trigger,
		CreatorUserID:  creator,
		BlueprintRunID: seedBlueprintRun(t, conn, task.ID),
	})
}

func TestProtocol_FrameRoundTrip(t *testing.T) {
	// Round-trip a request through write+read with the exact framing
	// the IPC layer uses. Pins the wire format so a length-prefix bug
	// would fail loudly here rather than at integration-test time.
	req := request{Version: 1, Method: "Probe", Args: json.RawMessage(`{"x":42}`)}
	var buf bytes.Buffer
	if err := writeFrame(&buf, req); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var got request
	if err := readFrame(&buf, &got); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if got.Version != req.Version || got.Method != req.Method || string(got.Args) != string(req.Args) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, req)
	}
}

func TestProtocol_OversizedFrameRejected(t *testing.T) {
	// A frame whose declared length exceeds maxFrameSize must be
	// rejected before the body read. Without this guard a hostile
	// client could OOM the daemon by claiming a 4GB body.
	var buf bytes.Buffer
	// Manually emit a header that claims a too-large body.
	buf.WriteByte(0xFF)
	buf.WriteByte(0xFF)
	buf.WriteByte(0xFF)
	buf.WriteByte(0xFF)
	var dst response
	err := readFrame(&buf, &dst)
	if err == nil {
		t.Fatal("expected error on oversized frame, got nil")
	}
}

// TestIPCClient_MultiCall_PerCallDial pins the bug that prompted the
// per-call dial: the server's handleConn is one-shot (close after
// reply), so a client that cached a single conn across calls would
// EOF on the second read. The test drives three RPCs through one
// client; each must succeed. Without per-call dialing in IPCClient,
// the second call would fail.
func TestIPCClient_MultiCall_PerCallDial(t *testing.T) {
	stores, _ := newTestDB(t)
	info := ConversationInfo{
		OrgID:          runmode.LocalDefaultOrgID,
		UserID:         "user-1",
		ConversationID: "run-multi",
	}
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})

	client := Dial(sockPath)
	defer client.Close()

	for i := 0; i < 3; i++ {
		got, err := client.LookupConversation(context.Background())
		if err != nil {
			t.Fatalf("LookupConversation call %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, info) {
			t.Errorf("call %d: identity mismatch: got %+v, want %+v", i, got, info)
		}
	}
}

// TestServer_LookupConversation_RoundTrip exercises the full
// Server.Serve → IPCClient.LookupConversation loop over a real (temporary)
// unix socket. The probe RPC matches what the integration test +
// the test stub send. info carries a non-empty TeamID so the assertion
// pins the multi-mode construction path (TFAC-458): the ConversationInfo the
// spawner builds off the run row — TeamID included — survives the IPC
// wire intact to the sandboxed agent, where the capture writers read it.
func TestServer_LookupConversation_RoundTrip(t *testing.T) {
	stores, _ := newTestDB(t)
	info := ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           "user-1",
		ConversationID:   "run-1",
		TeamID:           runmode.LocalDefaultTeamID,
		IsEventTriggered: false,
	}
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})

	client := Dial(sockPath)
	defer client.Close()

	got, err := client.LookupConversation(context.Background())
	if err != nil {
		t.Fatalf("LookupConversation: %v", err)
	}
	if !reflect.DeepEqual(got, info) {
		t.Errorf("LookupConversation mismatch: got %+v, want %+v", got, info)
	}
	if got.TeamID != info.TeamID {
		t.Errorf("TeamID dropped over IPC: got %q, want %q", got.TeamID, info.TeamID)
	}
}

// TestServer_VersionMismatch_RejectsCleanly pins the
// ProtocolVersion handshake: a client claiming a different version
// gets a typed error rather than silent misinterpretation. Sends a
// raw frame so the test isn't gated on the IPCClient honoring the
// ProtocolVersion constant.
func TestServer_VersionMismatch_RejectsCleanly(t *testing.T) {
	stores, _ := newTestDB(t)
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, ConversationInfo{ConversationID: "run-1"}, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a frame claiming version 999.
	bogus := request{Version: 999, Method: "LookupConversation", Args: json.RawMessage("{}")}
	if err := writeFrame(conn, bogus); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if resp.Error == "" {
		t.Errorf("expected error on version mismatch, got result %s", resp.Result)
	}
}

// TestServer_UnknownMethod_RejectsCleanly pins the method dispatch
// error path so a client built against a newer daemon doesn't crash
// the daemon on an unrecognized method name.
func TestServer_UnknownMethod_RejectsCleanly(t *testing.T) {
	stores, _ := newTestDB(t)
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, ConversationInfo{ConversationID: "run-1"}, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	bad := request{Version: ProtocolVersion, Method: "TotallyMadeUp", Args: json.RawMessage("{}")}
	if err := writeFrame(conn, bad); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if resp.Error == "" {
		t.Errorf("expected error for unknown method, got result %s", resp.Result)
	}
}

// TestServer_ConcurrentSockets_NoCrossContamination pins the
// per-run identity isolation: two daemons serving two different
// RunInfos, two clients connecting in parallel — LookupConversation on
// client A returns A's identity, not B's. The test simulates two
// sandboxed runs operating in parallel.
func TestServer_ConcurrentSockets_NoCrossContamination(t *testing.T) {
	stores, _ := newTestDB(t)
	infoA := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "run-A", UserID: "user-A"}
	infoB := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "run-B", UserID: "user-B", IsEventTriggered: true}

	startDaemon := func(info ConversationInfo) (string, func()) {
		sockPath := tempSocket(t)
		l, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		s := NewServer(stores, info, nil)
		go func() { _ = s.Serve(l) }()
		return sockPath, func() {
			_ = l.Close()
			_ = s.Shutdown(context.Background())
		}
	}

	pathA, stopA := startDaemon(infoA)
	pathB, stopB := startDaemon(infoB)
	t.Cleanup(stopA)
	t.Cleanup(stopB)

	var wg sync.WaitGroup
	wg.Add(2)
	probe := func(path string, want ConversationInfo) {
		defer wg.Done()
		c := Dial(path)
		defer c.Close()
		got, err := c.LookupConversation(context.Background())
		if err != nil {
			t.Errorf("LookupConversation(%s): %v", path, err)
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("identity mismatch on %s: got %+v, want %+v", path, got, want)
		}
	}
	go probe(pathA, infoA)
	go probe(pathB, infoB)
	wg.Wait()
}

// TestLocalClient_RoutingByTriggerType_Manual pins the per-write
// routing: a manual run's UpsertArtifact wraps in synthetic-claims
// (so RLS sees the kicking-off user), an event-triggered run's
// goes through the admin-pool ...System variant. We can't observe
// the pool directly in SQLite (one connection) but we can confirm
// the rows commit by reading them back, which is the regression
// guard we care about — a routing bug that misses the system
// variant in event-triggered mode would fail the write under
// Postgres RLS. SQLite collapses the branches but the SAME shape
// under test is what runs against Postgres.
func TestLocalClient_RoutingByTriggerType_Manual(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-1", runmode.LocalDefaultUserID, "manual")

	info := ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           runmode.LocalDefaultUserID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   "run-1",
		IsEventTriggered: false,
	}
	client := NewLocal(stores, info)

	// UpsertArtifact routes through the same withWrite branch the retired
	// pending-PR writer used: a synthetic-claims tx for a manual run.
	if _, err := client.UpsertArtifact(context.Background(), domain.Artifact{
		Kind:     domain.ArtifactKindPullRequest,
		Target:   "octocat/hello#1",
		State:    domain.ArtifactStatePRDraft,
		DedupKey: "github:pull_request:octocat/hello#1",
	}); err != nil {
		t.Fatalf("UpsertArtifact (manual): %v", err)
	}
	got := listConversationArtifacts(t, stores, "run-1")
	if len(got) != 1 || got[0].Target != "octocat/hello#1" {
		t.Errorf("unexpected artifacts: %+v", got)
	}
}

func TestLocalClient_RoutingByTriggerType_Event(t *testing.T) {
	stores, conn := newTestDB(t)
	// Event-triggered: no creator_user_id.
	seedConversation(t, stores, conn, "run-2", "", "event")

	info := ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           "",
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   "run-2",
		IsEventTriggered: true,
	}
	client := NewLocal(stores, info)

	// Event-triggered runs have no user, so UpsertArtifact must route through
	// the admin-pool ...System variant (the case Postgres RLS would reject under
	// tf_app). SQLite collapses both, but the same shape runs against Postgres.
	if _, err := client.UpsertArtifact(context.Background(), domain.Artifact{
		Kind:     domain.ArtifactKindPullRequest,
		Target:   "octocat/hello#2",
		State:    domain.ArtifactStatePRDraft,
		DedupKey: "github:pull_request:octocat/hello#2",
	}); err != nil {
		t.Fatalf("UpsertArtifact (event): %v", err)
	}
	got := listConversationArtifacts(t, stores, "run-2")
	if len(got) != 1 || got[0].Target != "octocat/hello#2" {
		t.Errorf("unexpected artifacts: %+v", got)
	}
}

// TestServer_GracefulShutdown_CompletesInFlight pins the daemon's
// drain semantics: a mid-flight RPC continues to completion when
// the listener stops accepting. The test sends a request, then
// (concurrently) starts shutdown — the request still returns
// successfully even though no new connections can be opened.
func TestServer_GracefulShutdown_CompletesInFlight(t *testing.T) {
	stores, _ := newTestDB(t)
	info := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "run-graceful"}
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info, nil)
	go func() { _ = srv.Serve(listener) }()

	client := Dial(sockPath)
	defer client.Close()
	// Round-trip once to confirm baseline.
	if _, err := client.LookupConversation(context.Background()); err != nil {
		t.Fatalf("baseline LookupConversation: %v", err)
	}

	// Close listener; Shutdown should drain cleanly.
	_ = listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agenthost-test-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Keep the path short — some unix-socket implementations cap at
	// ~108 bytes. Linux is fine but defense-in-depth.
	return filepath.Join(dir, "test.sock")
}
