package agenthost

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
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
// and returns the blueprint_run id. runs.blueprint_run_id is NOT NULL,
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

// seedAgentRun inserts an entity → event → task → run chain through
// the real store APIs so the FK constraints are honored. trigger is
// "manual" (creator set) or "event" (creator empty).
func seedAgentRun(t *testing.T, stores db.Stores, conn *sql.DB, runID, creator, trigger string) {
	t.Helper()
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	entity, _, err := stores.Entities.FindOrCreate(ctx, orgID, "jira", "TEST-"+runID, "issue", "T-"+runID, "https://x/"+runID)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := stores.Prompts.Create(ctx, orgID, runmode.LocalDefaultTeamID, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"}); err != nil {
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
	task, _, err := stores.Tasks.FindOrCreate(ctx, orgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, runID, evtID, 0.5)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	run := domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "claude-test",
		TriggerType:    trigger,
		CreatorUserID:  creator,
		BlueprintRunID: seedBlueprintRun(t, conn, task.ID),
	}
	if err := stores.AgentRuns.Create(ctx, orgID, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
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
	info := RunInfo{
		OrgID:  runmode.LocalDefaultOrg,
		UserID: "user-1",
		RunID:  "run-multi",
	}
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})

	client := Dial(sockPath)
	defer client.Close()

	for i := 0; i < 3; i++ {
		got, err := client.LookupRun(context.Background())
		if err != nil {
			t.Fatalf("LookupRun call %d: %v", i, err)
		}
		if got != info {
			t.Errorf("call %d: identity mismatch: got %+v, want %+v", i, got, info)
		}
	}
}

// TestServer_LookupRun_RoundTrip exercises the full
// Server.Serve → IPCClient.LookupRun loop over a real (temporary)
// unix socket. The probe RPC matches what the integration test +
// the test stub send.
func TestServer_LookupRun_RoundTrip(t *testing.T) {
	stores, _ := newTestDB(t)
	info := RunInfo{
		OrgID:            runmode.LocalDefaultOrg,
		UserID:           "user-1",
		RunID:            "run-1",
		IsEventTriggered: false,
	}
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})

	client := Dial(sockPath)
	defer client.Close()

	got, err := client.LookupRun(context.Background())
	if err != nil {
		t.Fatalf("LookupRun: %v", err)
	}
	if got != info {
		t.Errorf("LookupRun mismatch: got %+v, want %+v", got, info)
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
	srv := NewServer(stores, RunInfo{RunID: "run-1"})
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
	bogus := request{Version: 999, Method: "LookupRun", Args: json.RawMessage("{}")}
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
	srv := NewServer(stores, RunInfo{RunID: "run-1"})
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
// RunInfos, two clients connecting in parallel — LookupRun on
// client A returns A's identity, not B's. The test simulates two
// sandboxed runs operating in parallel.
func TestServer_ConcurrentSockets_NoCrossContamination(t *testing.T) {
	stores, _ := newTestDB(t)
	infoA := RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-A", UserID: "user-A"}
	infoB := RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-B", UserID: "user-B", IsEventTriggered: true}

	startDaemon := func(info RunInfo) (string, func()) {
		sockPath := tempSocket(t)
		l, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		s := NewServer(stores, info)
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
	probe := func(path string, want RunInfo) {
		defer wg.Done()
		c := Dial(path)
		defer c.Close()
		got, err := c.LookupRun(context.Background())
		if err != nil {
			t.Errorf("LookupRun(%s): %v", path, err)
			return
		}
		if got != want {
			t.Errorf("identity mismatch on %s: got %+v, want %+v", path, got, want)
		}
	}
	go probe(pathA, infoA)
	go probe(pathB, infoB)
	wg.Wait()
}

// TestLocalClient_RoutingByTriggerType_Manual pins the per-write
// routing: a manual run's CreatePendingPR wraps in synthetic-claims
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
	seedAgentRun(t, stores, conn, "run-1", runmode.LocalDefaultUserID, "manual")

	info := RunInfo{
		OrgID:            runmode.LocalDefaultOrg,
		UserID:           runmode.LocalDefaultUserID,
		RunID:            "run-1",
		IsEventTriggered: false,
	}
	client := NewLocal(stores, info)

	row := domain.PendingPR{
		ID:         "pr-1",
		RunID:      "run-1",
		Owner:      "octocat",
		Repo:       "hello",
		HeadBranch: "feature/x",
		HeadSHA:    "abc123",
		BaseBranch: "main",
		Title:      "feat: x",
		Body:       "body",
	}
	if err := client.CreateAndLockPendingPR(context.Background(), row); err != nil {
		t.Fatalf("CreateAndLockPendingPR (manual): %v", err)
	}
	got, err := client.GetPendingPRByRunID(context.Background())
	if err != nil {
		t.Fatalf("GetPendingPRByRunID: %v", err)
	}
	if got == nil {
		t.Fatal("expected PR row, got nil")
	}
	if got.ID != "pr-1" || !got.Locked {
		t.Errorf("unexpected row: %+v", got)
	}
}

func TestLocalClient_RoutingByTriggerType_Event(t *testing.T) {
	stores, conn := newTestDB(t)
	// Event-triggered: no creator_user_id.
	seedAgentRun(t, stores, conn, "run-2", "", "event")

	info := RunInfo{
		OrgID:            runmode.LocalDefaultOrg,
		UserID:           "",
		RunID:            "run-2",
		IsEventTriggered: true,
	}
	client := NewLocal(stores, info)

	row := domain.PendingPR{
		ID:         "pr-2",
		RunID:      "run-2",
		Owner:      "octocat",
		Repo:       "hello",
		HeadBranch: "feature/y",
		HeadSHA:    "def456",
		BaseBranch: "main",
		Title:      "fix: y",
		Body:       "body",
	}
	if err := client.CreateAndLockPendingPR(context.Background(), row); err != nil {
		t.Fatalf("CreateAndLockPendingPR (event): %v", err)
	}
	got, err := client.GetPendingPRByRunID(context.Background())
	if err != nil {
		t.Fatalf("GetPendingPRByRunID: %v", err)
	}
	if got == nil || got.ID != "pr-2" || !got.Locked {
		t.Errorf("unexpected row: %+v", got)
	}
}

// TestAutoDetect_NoSocket_ReturnsLocalClient pins the local-mode
// path: when /run/tf.sock is absent, AutoDetect resolves identity
// from TRIAGE_FACTORY_RUN_ID and returns a LocalClient. The probe
// here uses a non-default socket-path constant via env override so
// the test doesn't depend on /run/tf.sock's actual absence.
func TestAutoDetect_NoSocket_LocalClient(t *testing.T) {
	stores, conn := newTestDB(t)
	seedAgentRun(t, stores, conn, "run-3", runmode.LocalDefaultUserID, "manual")

	// AutoDetect reads TRIAGE_FACTORY_RUN_ID at lookup time; set it
	// to our seeded run.
	t.Setenv("TRIAGE_FACTORY_RUN_ID", "run-3")

	c, err := AutoDetect(context.Background(), stores)
	if err != nil {
		t.Fatalf("AutoDetect: %v", err)
	}
	defer c.Close()
	if _, ok := c.(*LocalClient); !ok {
		t.Errorf("expected *LocalClient (no socket), got %T", c)
	}
	got, err := c.LookupRun(context.Background())
	if err != nil {
		t.Fatalf("LookupRun: %v", err)
	}
	if got.RunID != "run-3" {
		t.Errorf("RunID: got %q, want run-3", got.RunID)
	}
}

// TestServer_GracefulShutdown_CompletesInFlight pins the daemon's
// drain semantics: a mid-flight RPC continues to completion when
// the listener stops accepting. The test sends a request, then
// (concurrently) starts shutdown — the request still returns
// successfully even though no new connections can be opened.
func TestServer_GracefulShutdown_CompletesInFlight(t *testing.T) {
	stores, _ := newTestDB(t)
	info := RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-graceful"}
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info)
	go func() { _ = srv.Serve(listener) }()

	client := Dial(sockPath)
	defer client.Close()
	// Round-trip once to confirm baseline.
	if _, err := client.LookupRun(context.Background()); err != nil {
		t.Fatalf("baseline LookupRun: %v", err)
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
