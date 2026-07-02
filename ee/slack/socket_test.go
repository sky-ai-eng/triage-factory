package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackws "github.com/coder/websocket"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ---------- fast timings for tests ----------

// withFastSocketTimings shrinks every socket tunable to test-friendly
// scales and restores the originals on cleanup — mirrors withFakeSlackAPI's
// swap-and-restore seam. Read/hello/dial/ack budgets stay generous (a few
// seconds) so a slightly slow CI runner doesn't flake a functional test;
// only reconcile/backoff cadence (which tests actively wait out) shrink to
// milliseconds. Individual tests that need an even shorter read deadline
// (the "dead socket" case) override socketReadDeadline themselves.
func withFastSocketTimings(t *testing.T) {
	t.Helper()
	origReconcile, origBase, origCap, origReset := socketReconcileInterval, socketBackoffBase, socketBackoffCap, socketBackoffResetAfter
	origDial, origHello, origRead, origAck := socketDialTimeout, socketHelloTimeout, socketReadDeadline, socketAckTimeout
	socketReconcileInterval = 20 * time.Millisecond
	socketBackoffBase = 5 * time.Millisecond
	socketBackoffCap = 50 * time.Millisecond
	socketBackoffResetAfter = 150 * time.Millisecond
	socketDialTimeout = 3 * time.Second
	socketHelloTimeout = 3 * time.Second
	socketReadDeadline = 3 * time.Second
	socketAckTimeout = 3 * time.Second
	t.Cleanup(func() {
		socketReconcileInterval, socketBackoffBase, socketBackoffCap, socketBackoffResetAfter = origReconcile, origBase, origCap, origReset
		socketDialTimeout, socketHelloTimeout, socketReadDeadline, socketAckTimeout = origDial, origHello, origRead, origAck
	})
}

// ---------- fake Slack Socket Mode backend ----------

// fakeSlackSocket is a minimal fake Slack Socket Mode backend: an
// httptest.Server serving POST /apps.connections.open (returning its own
// upgrade URL, mirroring the real single-use-URL contract closely enough
// for tests) and a websocket endpoint that speaks hello/events_api/
// disconnect by hand. Points the package-level slackAPIBase at itself so
// socket_conn.go's real slackConnectionsOpen call reaches this fake instead
// of slack.com.
type fakeSlackSocket struct {
	srv      *httptest.Server
	authFail atomic.Bool // apps.connections.open returns {"ok":false,"error":"invalid_auth"}
	badURL   atomic.Bool // apps.connections.open returns a URL that 404s on upgrade (dial failure)
	conns    chan *slackws.Conn
}

func newFakeSlackSocket(t *testing.T) *fakeSlackSocket {
	t.Helper()
	f := &fakeSlackSocket{conns: make(chan *slackws.Conn, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/apps.connections.open", f.handleOpen)
	mux.HandleFunc("/socket", f.handleSocket)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	orig := slackAPIBase
	slackAPIBase = f.srv.URL
	t.Cleanup(func() { slackAPIBase = orig })
	return f
}

func (f *fakeSlackSocket) handleOpen(w http.ResponseWriter, r *http.Request) {
	if f.authFail.Load() {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	url := f.srv.URL + "/socket"
	if f.badURL.Load() {
		url = f.srv.URL + "/no-such-path"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": url})
}

func (f *fakeSlackSocket) handleSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := slackws.Accept(w, r, &slackws.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	if err := conn.Write(context.Background(), slackws.MessageText, []byte(`{"type":"hello"}`)); err != nil {
		return
	}
	f.conns <- conn
}

// nextConn waits for the next accepted server-side connection — one per
// dial the connection loop under test performs.
func (f *fakeSlackSocket) nextConn(t *testing.T) *slackws.Conn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a socket connection")
		return nil
	}
}

func sendEventsAPI(t *testing.T, conn *slackws.Conn, envelopeID string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env, err := json.Marshal(map[string]any{"type": "events_api", "envelope_id": envelopeID, "payload": json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := conn.Write(context.Background(), slackws.MessageText, env); err != nil {
		t.Fatalf("write events_api envelope: %v", err)
	}
}

func sendDisconnect(t *testing.T, conn *slackws.Conn, reason string) {
	t.Helper()
	env, _ := json.Marshal(map[string]any{"type": "disconnect", "reason": reason})
	if err := conn.Write(context.Background(), slackws.MessageText, env); err != nil {
		t.Fatalf("write disconnect envelope: %v", err)
	}
}

// sendEnvelopeType writes an envelope of an arbitrary type/envelope_id —
// used to exercise envelope types this leaf doesn't subscribe to
// (slash_commands, interactive, ...), which Slack's protocol still expects
// acked whenever they carry an envelope_id.
func sendEnvelopeType(t *testing.T, conn *slackws.Conn, typ, envelopeID string) {
	t.Helper()
	env, _ := json.Marshal(map[string]any{"type": typ, "envelope_id": envelopeID})
	if err := conn.Write(context.Background(), slackws.MessageText, env); err != nil {
		t.Fatalf("write %s envelope: %v", typ, err)
	}
}

func expectAck(t *testing.T, conn *slackws.Conn, envelopeID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var got struct {
		EnvelopeID string `json:"envelope_id"`
	}
	if json.Unmarshal(data, &got) != nil || got.EnvelopeID != envelopeID {
		t.Fatalf("ack = %s, want envelope_id %q", data, envelopeID)
	}
}

// ---------- test rig ----------

const (
	socketTestOrgID     = "org-socket-1"
	socketTestWorkspace = "T0SOCKET01"
	socketTestAppID     = "A0SOCKET01"
	socketTestAppRef    = "slack_ws_T0SOCKET01_A0SOCKET01_app_token"
	socketTestAppToken  = "xapp-test-token"
)

// socketTestRig bundles a manager wired to fakes plus the pieces a test
// needs to mutate: the workspace row set, the entitlement provider, and the
// published-events sink.
type socketTestRig struct {
	mgr           *socketManager
	wsStore       *fakeWorkspaceStore
	secrets       *fakeSecretStore
	configChanged chan struct{}
	published     *[]domain.Event
}

// newSocketTestRig builds a manager over rows, licensed for FeatureSlack and
// running in multi mode by default (the reconciler no-ops in local mode —
// see socketManager.run — and go test's default process mode is local, same
// as production's unset-TF_MODE default). The manager is NOT started —
// callers run mgr.run(ctx) themselves so they control the goroutine's
// lifetime. Callers that need different entitlement/mode state override it
// AFTER this returns, relying on this function's t.Cleanup registrations
// (LIFO) to restore the true defaults last.
func newSocketTestRig(t *testing.T, rows ...*slackstore.Workspace) *socketTestRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	wsStore := newFakeWorkspaceStore(rows...)
	// Seed the app-token secret for every row's (org, ref) pair, plus the
	// standard test org/ref unconditionally — tests that add a row AFTER
	// construction (e.g. via wsStore.setRow, simulating a connect that
	// arrives mid-run) still resolve a token when the connection dials.
	secretsValues := map[string]string{socketTestOrgID + "/" + socketTestAppRef: socketTestAppToken}
	for _, r := range rows {
		if r.AppTokenRef != "" {
			secretsValues[r.OrgID+"/"+r.AppTokenRef] = socketTestAppToken
		}
	}
	secrets := &fakeSecretStore{values: secretsValues}
	stores := db.Stores{
		Secrets: secrets,
		Ext:     map[string]any{slackstore.ExtKey: &slackstore.Bundle{Workspaces: wsStore, Deliveries: newFakeDeliveries()}},
	}

	published := &[]domain.Event{}
	pipeline := &ingestPipeline{
		entities:   newFakeEntities(),
		deliveries: newFakeDeliveries(),
		publish:    func(evt domain.Event) { *published = append(*published, evt) },
	}

	configChanged := make(chan struct{}, 1)
	mgr := newSocketManager(stores, pipeline, http.DefaultClient, configChanged)
	return &socketTestRig{mgr: mgr, wsStore: wsStore, secrets: secrets, configChanged: configChanged, published: published}
}

func socketTestRow(orgID string) *slackstore.Workspace {
	return &slackstore.Workspace{
		WorkspaceID: socketTestWorkspace, APIAppID: socketTestAppID, OrgID: orgID,
		Transport: transportSocket, BotUserID: "U0BOT", AppTokenRef: socketTestAppRef,
	}
}

func runManager(t *testing.T, mgr *socketManager) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("manager.run did not return after ctx cancellation")
		}
	})
	return cancel
}

func eventCallbackPayload(teamID, appID, eventID string, inner map[string]any) map[string]any {
	return map[string]any{
		"type": "event_callback", "team_id": teamID, "api_app_id": appID,
		"event_id": eventID, "event": inner,
	}
}

func mentionInner(channel, user, ts string) map[string]any {
	return map[string]any{"type": "app_mention", "channel": channel, "user": user, "ts": ts}
}

// ---------- backoff ----------

func TestBackoffDuration_WithinBoundsAndGrows(t *testing.T) {
	origBase, origCap := socketBackoffBase, socketBackoffCap
	socketBackoffBase, socketBackoffCap = 1*time.Second, 5*time.Minute
	t.Cleanup(func() { socketBackoffBase, socketBackoffCap = origBase, origCap })

	for attempt := 0; attempt < 12; attempt++ {
		wantMax := socketBackoffBase * time.Duration(uint64(1)<<uint(attempt))
		if wantMax > socketBackoffCap || wantMax <= 0 {
			wantMax = socketBackoffCap
		}
		for i := 0; i < 20; i++ {
			d := backoffDuration(attempt)
			if d < 0 || d > wantMax {
				t.Fatalf("attempt=%d: backoffDuration = %v, want in [0, %v]", attempt, d, wantMax)
			}
		}
	}
}

func TestBackoffDuration_CapsAtHighAttempts(t *testing.T) {
	origBase, origCap := socketBackoffBase, socketBackoffCap
	socketBackoffBase, socketBackoffCap = 1*time.Second, 5*time.Minute
	t.Cleanup(func() { socketBackoffBase, socketBackoffCap = origBase, origCap })

	for i := 0; i < 20; i++ {
		if d := backoffDuration(40); d > socketBackoffCap {
			t.Fatalf("backoffDuration(40) = %v, want <= cap %v", d, socketBackoffCap)
		}
	}
}

// ---------- desiredApps ----------

func TestDesiredApps_TwoRowsSameApp_OneEntry(t *testing.T) {
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	row1 := &slackstore.Workspace{WorkspaceID: "T1", APIAppID: socketTestAppID, OrgID: socketTestOrgID, Transport: transportSocket, AppTokenRef: socketTestAppRef}
	row2 := &slackstore.Workspace{WorkspaceID: "T2", APIAppID: socketTestAppID, OrgID: socketTestOrgID, Transport: transportSocket, AppTokenRef: socketTestAppRef}
	rig := newSocketTestRig(t, row1, row2)

	desired, err := rig.mgr.desiredApps(context.Background())
	if err != nil {
		t.Fatalf("desiredApps: %v", err)
	}
	if len(desired) != 1 {
		t.Fatalf("desiredApps = %v; want exactly 1 entry for the shared app", desired)
	}
	if d := desired[socketTestAppID]; d.orgID != socketTestOrgID {
		t.Errorf("desired app org = %q; want %q", d.orgID, socketTestOrgID)
	}
}

func TestDesiredApps_UnentitledOrg_Excluded(t *testing.T) {
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	entitlements.RegisterProvider(entitlements.Static()) // override: nothing licensed

	desired, err := rig.mgr.desiredApps(context.Background())
	if err != nil {
		t.Fatalf("desiredApps: %v", err)
	}
	if len(desired) != 0 {
		t.Fatalf("desiredApps = %v; want empty for an unentitled org", desired)
	}
}

func TestDesiredApps_WebhookOnlyRow_Excluded(t *testing.T) {
	row := &slackstore.Workspace{WorkspaceID: socketTestWorkspace, APIAppID: socketTestAppID, OrgID: socketTestOrgID, Transport: transportEventsAPI}
	rig := newSocketTestRig(t, row)
	desired, err := rig.mgr.desiredApps(context.Background())
	if err != nil {
		t.Fatalf("desiredApps: %v", err)
	}
	if len(desired) != 0 {
		t.Fatalf("desiredApps = %v; want empty for a webhook-only row", desired)
	}
}

// ---------- full connection lifecycle (fake Slack over the wire) ----------

func TestSocketManager_HappyPath_PublishesAndAcks(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	conn := fake.nextConn(t)
	payload := eventCallbackPayload(socketTestWorkspace, socketTestAppID, "Ev-socket-1", mentionInner("C1", "U1", "1.0"))
	sendEventsAPI(t, conn, "Env-1", payload)
	expectAck(t, conn, "Env-1")

	deadline := time.Now().Add(2 * time.Second)
	for len(*rig.published) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*rig.published) != 1 {
		t.Fatalf("published %d events; want 1", len(*rig.published))
	}
	if got := (*rig.published)[0]; got.OrgID != socketTestOrgID || got.EventType != domain.EventSlackMention {
		t.Errorf("published event = %+v; want slack:mention for %s", got, socketTestOrgID)
	}

	status, ok := rig.mgr.StatusFor(socketTestAppID)
	if !ok || status.State != stateOpen {
		t.Errorf("StatusFor = %+v, ok=%v; want state=open", status, ok)
	}
}

// TestSocketManager_UnsubscribedEnvelopeType_StillAcked covers an envelope
// type this leaf never subscribes to (the manifest requests no
// slash_commands/interactive scopes) — Slack's protocol acks by
// envelope_id alone, not by type, so failing to ack one would leave Slack
// redelivering it indefinitely even though nothing here will ever consume
// it. A subsequent events_api envelope on the same connection proves the
// loop kept serving normally afterward.
func TestSocketManager_UnsubscribedEnvelopeType_StillAcked(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	conn := fake.nextConn(t)
	sendEnvelopeType(t, conn, "interactive", "Env-interactive")
	expectAck(t, conn, "Env-interactive")

	payload := eventCallbackPayload(socketTestWorkspace, socketTestAppID, "Ev-after-1", mentionInner("C1", "U1", "1.0"))
	sendEventsAPI(t, conn, "Env-after", payload)
	expectAck(t, conn, "Env-after")
}

// TestSocketManager_LoadBalancing_TwoWorkspacesOneApp is the regression test
// for the bug that killed per-row pinning: two rows (T1, T2) share one app
// and one org. Both envelopes arrive on the SAME connection (Slack
// load-balances by app, not by workspace); each must resolve against its
// own row via per-envelope (team_id, api_app_id) lookup.
func TestSocketManager_LoadBalancing_TwoWorkspacesOneApp(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	row1 := &slackstore.Workspace{WorkspaceID: "T1LB", APIAppID: socketTestAppID, OrgID: socketTestOrgID, Transport: transportSocket, AppTokenRef: socketTestAppRef, BotUserID: "U0BOT"}
	row2 := &slackstore.Workspace{WorkspaceID: "T2LB", APIAppID: socketTestAppID, OrgID: socketTestOrgID, Transport: transportSocket, AppTokenRef: socketTestAppRef, BotUserID: "U0BOT"}
	rig := newSocketTestRig(t, row1, row2)
	runManager(t, rig.mgr)

	conn := fake.nextConn(t)

	sendEventsAPI(t, conn, "Env-T1", eventCallbackPayload("T1LB", socketTestAppID, "Ev-T1", mentionInner("C1", "U1", "1.0")))
	expectAck(t, conn, "Env-T1")
	sendEventsAPI(t, conn, "Env-T2", eventCallbackPayload("T2LB", socketTestAppID, "Ev-T2", mentionInner("C2", "U2", "2.0")))
	expectAck(t, conn, "Env-T2")

	deadline := time.Now().Add(2 * time.Second)
	for len(*rig.published) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*rig.published) != 2 {
		t.Fatalf("published %d events; want 2", len(*rig.published))
	}
	var meta1, meta2 SlackMentionMetadata
	_ = json.Unmarshal([]byte((*rig.published)[0].MetadataJSON), &meta1)
	_ = json.Unmarshal([]byte((*rig.published)[1].MetadataJSON), &meta2)
	if meta1.WorkspaceID != "T1LB" || meta2.WorkspaceID != "T2LB" {
		t.Errorf("workspace ids = %q, %q; want T1LB then T2LB (each envelope resolved against its own row)", meta1.WorkspaceID, meta2.WorkspaceID)
	}
}

// TestSocketManager_PipelineError_NoAck_DuplicateDedupedAndAcked covers both
// halves of the at-least-once contract: a pipeline failure gets no ack (so
// Slack redelivers), and a redelivery that succeeds via the pipeline's own
// dedup (a "duplicate" MarkDeliveredSystem result) is acked. Deliberately
// reads the wire only ONCE, after both sends: coder/websocket allows only one
// Reader at a time on a connection, so asserting "no ack" with a separate
// blocking Read (racing the later read for "Env-retry"'s ack) would corrupt
// the frame stream. Since Env-fail never produces an ack, the single frame
// that does arrive can only be Env-retry's — proving both halves at once.
func TestSocketManager_PipelineError_NoAck_DuplicateDedupedAndAcked(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))

	failing := &erroringDeliveries{}
	failing.failNext.Store(true)
	rig.mgr.pipeline.deliveries = failing
	runManager(t, rig.mgr)

	conn := fake.nextConn(t)
	payload := eventCallbackPayload(socketTestWorkspace, socketTestAppID, "Ev-fail-1", mentionInner("C1", "U1", "1.0"))

	sendEventsAPI(t, conn, "Env-fail", payload)
	time.Sleep(100 * time.Millisecond) // let the failing attempt actually run before flipping healthy
	failing.failNext.Store(false)

	sendEventsAPI(t, conn, "Env-retry", payload)
	expectAck(t, conn, "Env-retry")

	if len(*rig.published) != 1 {
		t.Fatalf("published %d events; want exactly 1 (the failed attempt published nothing)", len(*rig.published))
	}
}

// erroringDeliveries is a slackstore.DeliveryStore fake whose first call
// fails (simulating a transient store error mid-pipeline), then behaves
// like a normal fresh-delivery store. failNext is an atomic.Bool since the
// connection loop reads it from its own goroutine while the test writes it.
type erroringDeliveries struct {
	failNext atomic.Bool
	mu       sync.Mutex
	seen     map[string]bool
}

func (f *erroringDeliveries) MarkDeliveredSystem(_ context.Context, appID, eventID string) (bool, error) {
	if f.failNext.Load() {
		return false, context.DeadlineExceeded
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	key := appID + "/" + eventID
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	return true, nil
}

func TestSocketManager_UnconnectedWorkspace_SightingRecordedNoPublish(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	conn := fake.nextConn(t)
	payload := eventCallbackPayload("T0UNKNOWNTEAM", socketTestAppID, "Ev-unk-1", mentionInner("C1", "U1", "1.0"))
	sendEventsAPI(t, conn, "Env-unk", payload)
	expectAck(t, conn, "Env-unk")

	if len(*rig.published) != 0 {
		t.Fatalf("published %d events for an unconnected workspace; want 0", len(*rig.published))
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := rig.mgr.StatusFor(socketTestAppID)
		if len(status.Sightings) == 1 && status.Sightings[0].TeamID == "T0UNKNOWNTEAM" && status.Sightings[0].Count == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the unconnected-workspace sighting to be recorded")
}

func TestSocketManager_EntitlementLapseMidConnection_EnvelopeDropped(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	conn := fake.nextConn(t)

	// Flip entitlement off WITHOUT reconciling (simulates the race window
	// before the next tick tears the connection down).
	entitlements.RegisterProvider(entitlements.Static())

	payload := eventCallbackPayload(socketTestWorkspace, socketTestAppID, "Ev-lapsed-1", mentionInner("C1", "U1", "1.0"))
	sendEventsAPI(t, conn, "Env-lapsed", payload)
	expectAck(t, conn, "Env-lapsed") // dropped, but still acked — same as the webhook's ackEmpty

	if len(*rig.published) != 0 {
		t.Fatalf("published %d events for a lapsed org; want 0", len(*rig.published))
	}
}

func TestSocketManager_DisconnectFrame_CleanRedial(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	conn1 := fake.nextConn(t)
	sendDisconnect(t, conn1, "refresh_requested")

	conn2 := fake.nextConn(t) // a second dial proves the loop redialed fresh
	payload := eventCallbackPayload(socketTestWorkspace, socketTestAppID, "Ev-after-redial", mentionInner("C1", "U1", "1.0"))
	sendEventsAPI(t, conn2, "Env-after-redial", payload)
	expectAck(t, conn2, "Env-after-redial")
}

func TestSocketManager_DialFailure_BacksOff(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	fake.badURL.Store(true)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := rig.mgr.StatusFor(socketTestAppID)
		if ok && status.State == stateBackingOff && status.ConsecutiveFailures >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a dial failure to report state=backing_off")
}

func TestSocketManager_AuthFailure_CapAndAuthFailedStatus(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	fake.authFail.Store(true)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := rig.mgr.StatusFor(socketTestAppID)
		if ok && status.State == stateAuthFailed {
			if status.LastError == "" {
				t.Fatal("state=auth_failed but last_error is empty")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for an auth failure to report state=auth_failed")
}

// ---------- reconciler ----------

func TestSocketManager_NotifyConfigChanged_ConnectsWithoutWaitingForTicker(t *testing.T) {
	withFastSocketTimings(t)
	// A slow ticker so only the configChanged signal (not the ticker) can
	// plausibly explain a connection appearing this fast.
	socketReconcileInterval = 10 * time.Second

	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t) // no rows yet
	runManager(t, rig.mgr)

	rig.wsStore.setRow(socketTestRow(socketTestOrgID))
	select {
	case rig.configChanged <- struct{}{}:
	default:
	}

	fake.nextConn(t) // proves the connection appeared without waiting ~10s for the ticker
}

func TestSocketManager_RowDeleted_TornDown(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runManager(t, rig.mgr)
	fake.nextConn(t)

	rig.wsStore.deleteRow(socketTestWorkspace, socketTestAppID)
	select {
	case rig.configChanged <- struct{}{}:
	default:
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := rig.mgr.StatusFor(socketTestAppID); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the connection to be torn down after its row was deleted")
}

func TestSocketManager_TwoRowsOneApp_ExactlyOneConnection(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	row1 := &slackstore.Workspace{WorkspaceID: "T1ONE", APIAppID: socketTestAppID, OrgID: socketTestOrgID, Transport: transportSocket, AppTokenRef: socketTestAppRef}
	row2 := &slackstore.Workspace{WorkspaceID: "T2ONE", APIAppID: socketTestAppID, OrgID: socketTestOrgID, Transport: transportSocket, AppTokenRef: socketTestAppRef}
	rig := newSocketTestRig(t, row1, row2)
	runManager(t, rig.mgr)

	fake.nextConn(t)
	select {
	case c := <-fake.conns:
		t.Fatalf("got a second connection %v for one app with two rows; want exactly one", c)
	case <-time.After(200 * time.Millisecond):
	}

	rig.mgr.mu.Lock()
	n := len(rig.mgr.conns)
	rig.mgr.mu.Unlock()
	if n != 1 {
		t.Fatalf("manager has %d live connections; want exactly 1", n)
	}
}

func TestSocketManager_Shutdown_ClosesConnectionsPromptly(t *testing.T) {
	withFastSocketTimings(t)
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); rig.mgr.run(ctx) }()

	conn := fake.nextConn(t)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("manager.run did not return promptly after ctx cancellation")
	}

	// The server-side conn should observe the client going away.
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rcancel()
	if _, _, err := conn.Read(rctx); err == nil {
		t.Error("expected the connection to be closed after shutdown, but Read succeeded")
	}
}

// TestHandlers_LocalMode404 (workspaces_test.go) already proves local mode
// gates the HTTP surface; local mode also skips the reconciler entirely —
// there's no Postgres-backed Slack store to enumerate.
func TestSocketManager_LocalMode_RunIsNoOp(t *testing.T) {
	fake := newFakeSlackSocket(t)
	rig := newSocketTestRig(t, socketTestRow(socketTestOrgID))
	runmode.SetForTest(t, runmode.ModeLocal) // override newSocketTestRig's multi-mode default

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); rig.mgr.run(ctx) }()

	select {
	case <-fake.conns:
		t.Fatal("local mode dialed a connection; it must be a no-op")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after ctx cancellation in local mode")
	}
}
