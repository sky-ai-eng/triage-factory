package agenthost

import (
	"context"
	"database/sql"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The memory-load tests drive LocalClient.MemoryLoad — the single seam the
// multi daemon dispatches through — against a real SQLite store bundle. A
// hit composes each entry with the human-feedback separator, honors the limit
// (most recent N), and records a durable touch; a miss mints nothing and
// touches nothing.

// memBase is a fixed clock so the seeded memories' created_at ordering (and
// therefore the "most recent N" tail) is deterministic — Date.now() is banned
// in workflow scripts but fine in a test.
var memBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seedAuthoringMemory inserts one prior run + its run_memory row + the
// run_memory_entities join row on entityID, with explicit content and
// created_at so the read's composition and ordering are pinned. role is the
// join row's classification (primary/produced/touched). human "" leaves
// human_content NULL (agent-only composition).
func seedAuthoringMemory(t *testing.T, conn *sql.DB, orgID, entityID, runID, agent, human string, createdAt time.Time, role string) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO runs (id, origin, status) VALUES (?, 'interactive', 'running')`, runID); err != nil {
		t.Fatalf("seed authoring run: %v", err)
	}
	var humanVal any
	if human != "" {
		humanVal = human
	}
	if _, err := conn.Exec(
		`INSERT INTO run_memory (id, run_id, entity_id, agent_content, human_content, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		uuid.New().String(), runID, entityID, agent, humanVal, createdAt,
	); err != nil {
		t.Fatalf("seed run_memory: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO run_memory_entities (org_id, run_id, entity_id, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		orgID, runID, entityID, role, createdAt,
	); err != nil {
		t.Fatalf("seed run_memory_entities: %v", err)
	}
}

// seedEntity resolves-or-creates the entity for (source, sourceID) via the
// admin-pool path and returns its id.
func seedEntity(t *testing.T, stores db.Stores, orgID, source, sourceID, title string) string {
	t.Helper()
	ent, _, err := stores.Entities.FindOrCreateSystem(context.Background(), orgID, source, sourceID, "pr", title, "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return ent.ID
}

// TestLocalClient_MemoryLoad_HitComposesAndLimits pins the hit path: the entity
// resolves, Count is the pre-limit total, Memories is the most recent N (tail of
// the ASC store order) composed with the human-feedback separator where present,
// and a durable 'touched' row lands for the reading run.
func TestLocalClient_MemoryLoad_HitComposesAndLimits(t *testing.T) {
	conn, stores, info, client := newGithubRecordingClientConn(t, "http://unused", true)
	ctx := context.Background()

	entityID := seedEntity(t, stores, runmode.LocalDefaultOrgID, "github", "octo/repo#7", "A PR")
	// Three prior runs, oldest→newest. The newest carries human feedback so the
	// composition separator is exercised; the tail (limit 2) drops the oldest.
	seedAuthoringMemory(t, conn, runmode.LocalDefaultOrgID, entityID, uuid.New().String(), "oldest agent note", "", memBase, domain.MemoryRolePrimary)
	seedAuthoringMemory(t, conn, runmode.LocalDefaultOrgID, entityID, uuid.New().String(), "middle agent note", "", memBase.Add(time.Hour), domain.MemoryRolePrimary)
	newestRun := uuid.New().String()
	seedAuthoringMemory(t, conn, runmode.LocalDefaultOrgID, entityID, newestRun, "newest agent note", "human verdict", memBase.Add(2*time.Hour), domain.MemoryRolePrimary)

	res, err := client.MemoryLoad(ctx, "github", "octo/repo#7", 2)
	if err != nil {
		t.Fatalf("MemoryLoad: %v", err)
	}
	if res.EntityID != entityID {
		t.Errorf("EntityID = %q, want %q", res.EntityID, entityID)
	}
	if res.Source != "github" || res.SourceID != "octo/repo#7" {
		t.Errorf("coordinates = %q/%q, want github/octo/repo#7", res.Source, res.SourceID)
	}
	if res.Title != "A PR" {
		t.Errorf("Title = %q, want %q", res.Title, "A PR")
	}
	if res.Count != 3 {
		t.Errorf("Count = %d, want 3 (pre-limit total)", res.Count)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("len(Memories) = %d, want 2 (the limit)", len(res.Memories))
	}
	// The tail keeps ASC order: [middle, newest]. The oldest is dropped.
	if res.Memories[0].Content != "middle agent note" {
		t.Errorf("Memories[0].Content = %q, want the middle note (oldest dropped by the limit)", res.Memories[0].Content)
	}
	newest := res.Memories[1]
	if newest.RunID != newestRun {
		t.Errorf("Memories[1].RunID = %q, want the newest run %q", newest.RunID, newestRun)
	}
	if !strings.Contains(newest.Content, "## Human feedback (post-run)") {
		t.Errorf("newest Content = %q, want it composed with the human-feedback separator", newest.Content)
	}
	if !strings.Contains(newest.Content, "newest agent note") || !strings.Contains(newest.Content, "human verdict") {
		t.Errorf("newest Content = %q, want both halves present", newest.Content)
	}

	// Loading IS an address: the reading run gets a durable touch on the entity.
	if role := touchRole(t, conn, info.RunID, entityID); role != domain.MemoryRoleTouched {
		t.Errorf("touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestLocalClient_MemoryLoad_Miss_NoEntityNoTouch pins the miss path: an unknown
// entity returns an empty result (empty EntityID, Count 0, empty non-nil
// Memories), mints NO entity, and records NO touch — the deliberate asymmetry vs
// the addressed-read touch resolver.
func TestLocalClient_MemoryLoad_Miss_NoEntityNoTouch(t *testing.T) {
	conn, stores, info, client := newGithubRecordingClientConn(t, "http://unused", true)
	ctx := context.Background()

	res, err := client.MemoryLoad(ctx, "github", "octo/repo#404", 20)
	if err != nil {
		t.Fatalf("MemoryLoad(miss): %v", err)
	}
	if res.EntityID != "" {
		t.Errorf("EntityID = %q, want empty on a miss", res.EntityID)
	}
	if res.Count != 0 {
		t.Errorf("Count = %d, want 0 on a miss", res.Count)
	}
	if res.Memories == nil {
		t.Error("Memories = nil, want a non-nil empty slice (renders [] not null)")
	}
	if len(res.Memories) != 0 {
		t.Errorf("Memories = %+v, want empty on a miss", res.Memories)
	}

	// No entity minted (unlike the touch resolver's FindOrCreate).
	ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#404")
	if err != nil {
		t.Fatalf("GetBySource: %v", err)
	}
	if ent != nil {
		t.Errorf("a miss must mint no entity, got %+v", ent)
	}
	// No touch row for the reading run at all.
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM run_memory_entities WHERE run_id = ?`, info.RunID).Scan(&n); err != nil {
		t.Fatalf("count touch rows: %v", err)
	}
	if n != 0 {
		t.Errorf("miss recorded %d touch row(s) for the reading run, want 0", n)
	}
}

// TestLocalClient_MemoryLoad_InvalidSource pins the host-side guard: a source
// outside {github,jira,slack} is a hard error, distinct from a valid-source
// miss.
func TestLocalClient_MemoryLoad_InvalidSource(t *testing.T) {
	_, _, _, client := newGithubRecordingClientConn(t, "http://unused", true)
	if _, err := client.MemoryLoad(context.Background(), "linear", "ENG-1", 20); err == nil {
		t.Fatal("expected an error for an unsupported source")
	}
}

// TestLocalClient_MemoryLoad_NonPositiveLimit_DefaultsBounded pins that a
// non-positive limit (a direct Client caller bypassing the CLI's positive-int
// guard) is treated as the default cap — NOT as unbounded, which the prior
// fetch-all-then-slice quietly did. With 25 memories seeded, limit 0 and limit
// -5 both return the 20 most recent, and Count stays the true pre-limit total.
func TestLocalClient_MemoryLoad_NonPositiveLimit_DefaultsBounded(t *testing.T) {
	conn, stores, _, client := newGithubRecordingClientConn(t, "http://unused", true)
	ctx := context.Background()

	entityID := seedEntity(t, stores, runmode.LocalDefaultOrgID, "github", "octo/repo#8", "Busy PR")
	const total = 25
	for i := 0; i < total; i++ {
		content := "m" + string(rune('A'+i)) // mA, mB, … distinct + ordered by seed time
		seedAuthoringMemory(t, conn, runmode.LocalDefaultOrgID, entityID, uuid.New().String(), content, "", memBase.Add(time.Duration(i)*time.Hour), domain.MemoryRolePrimary)
	}

	for _, limit := range []int{0, -5} {
		res, err := client.MemoryLoad(ctx, "github", "octo/repo#8", limit)
		if err != nil {
			t.Fatalf("MemoryLoad(limit %d): %v", limit, err)
		}
		if res.Count != total {
			t.Errorf("limit %d: Count = %d, want %d (pre-limit total)", limit, res.Count, total)
		}
		if len(res.Memories) != defaultMemoryLoadLimit {
			t.Fatalf("limit %d: len(Memories) = %d, want the default cap %d (NOT unbounded %d)", limit, len(res.Memories), defaultMemoryLoadLimit, total)
		}
		// The 20 most recent, ASC: the oldest 5 (mA..mE) are dropped, newest (mY) last.
		if res.Memories[0].Content != "mF" {
			t.Errorf("limit %d: Memories[0] = %q, want mF (oldest 5 dropped by the default cap)", limit, res.Memories[0].Content)
		}
		if last := res.Memories[len(res.Memories)-1].Content; last != "mY" {
			t.Errorf("limit %d: last = %q, want the newest mY", limit, last)
		}
	}
}

// TestServer_MemoryLoad_RoundTrip exercises the full Server.Serve →
// IPCClient.MemoryLoad loop over a real unix socket (mirrors
// TestServer_LookupRun_RoundTrip): the composed result survives the wire intact.
func TestServer_MemoryLoad_RoundTrip(t *testing.T) {
	stores, conn := newTestDB(t)
	orgID := runmode.LocalDefaultOrgID

	// A reading run for the touch FK, and an entity with one prior memory.
	readerRun := uuid.New().String()
	if _, err := conn.Exec(`INSERT INTO runs (id, origin, status) VALUES (?, 'interactive', 'running')`, readerRun); err != nil {
		t.Fatalf("seed reader run: %v", err)
	}
	entityID := seedEntity(t, stores, orgID, "jira", "SKY-123", "A ticket")
	seedAuthoringMemory(t, conn, orgID, entityID, uuid.New().String(), "prior narrative", "", memBase, domain.MemoryRolePrimary)

	info := RunInfo{OrgID: orgID, TeamID: runmode.LocalDefaultTeamID, RunID: readerRun, IsEventTriggered: true}
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

	res, err := client.MemoryLoad(context.Background(), "jira", "SKY-123", 20)
	if err != nil {
		t.Fatalf("MemoryLoad over IPC: %v", err)
	}
	if res.EntityID != entityID {
		t.Errorf("EntityID = %q, want %q (dropped over the wire?)", res.EntityID, entityID)
	}
	if res.Count != 1 || len(res.Memories) != 1 {
		t.Fatalf("Count=%d len(Memories)=%d, want 1/1", res.Count, len(res.Memories))
	}
	if res.Memories[0].Content != "prior narrative" {
		t.Errorf("Content = %q, want %q", res.Memories[0].Content, "prior narrative")
	}
	// The touch landed daemon-side.
	if role := touchRole(t, conn, readerRun, entityID); role != domain.MemoryRoleTouched {
		t.Errorf("touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestRelayRuntime_MemoryLoad_RoundTrips proves the sidecar (capless) path: the
// relay runtime ships MemoryLoad to the orchestrator, which does the entity
// lookup + team-scoped read + best-effort touch against its own stores and
// returns the composed result — so `memory load` works from the jail where no
// db.Stores exists. This is why MemoryLoad routes through the Runtime rather
// than reading c.stores directly.
func TestRelayRuntime_MemoryLoad_RoundTrips(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	ctx := context.Background()

	entityID := seedEntity(t, stores, info.OrgID, "github", "octo/repo#7", "A PR")
	seedAuthoringMemory(t, conn, info.OrgID, entityID, uuid.New().String(), "prior narrative", "", memBase, domain.MemoryRolePrimary)

	res, err := rt.MemoryLoad(ctx, "github", "octo/repo#7", 20)
	if err != nil {
		t.Fatalf("MemoryLoad relay: %v", err)
	}
	if res.EntityID != entityID || res.Count != 1 || len(res.Memories) != 1 || res.Memories[0].Content != "prior narrative" {
		t.Fatalf("relayed MemoryLoad result = %+v, want the seeded memory", res)
	}
	// The best-effort touch landed orchestrator-side (the write followed the
	// read back over the wire's one op).
	if role := touchRole(t, conn, info.RunID, entityID); role != domain.MemoryRoleTouched {
		t.Errorf("relayed touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestIPCClient_MemoryLoad_UnknownMethodOnOldDaemon simulates a daemon built
// before this RPC existed: it answers any request with the same "unknown
// method" error the dispatch default case produces. Pins that the client
// surfaces a clean error — the point of NOT bumping ProtocolVersion for a
// backward-compatible method addition.
func TestIPCClient_MemoryLoad_UnknownMethodOnOldDaemon(t *testing.T) {
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req request
		if err := readFrame(conn, &req); err != nil {
			return
		}
		_ = writeFrame(conn, response{Error: ErrUnknownMethod.Error() + ": " + req.Method})
	}()

	client := Dial(sockPath)
	defer client.Close()

	_, err = client.MemoryLoad(context.Background(), "github", "octo/repo#1", 20)
	if err == nil {
		t.Fatal("expected a clean error from an old daemon, got nil")
	}
	if !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("error = %q, want it to mention 'unknown method'", err)
	}
}
