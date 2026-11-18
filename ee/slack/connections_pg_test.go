package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// upsertActiveConnection upserts c as an active connection, through an org's
// own app unless c names another source: the shape a fixture wants unless it
// is about an inactive one (insertInactiveConnection).
func upsertActiveConnection(ctx context.Context, tx db.TxStores, c slackstore.Connection) error {
	if c.Source == "" {
		c.Source = slackstore.SourceOrg
	}
	c.Active = true
	_, err := slackstore.FromTx(tx).Connections.Upsert(ctx, c)
	return err
}

// insertInactiveConnection writes an inactive events_api connection of an
// org's own app directly, as the switch that would leave one behind is not
// built yet. Its bot token is stored under the connection's keys.
func insertInactiveConnection(t *testing.T, h *pgtest.Harness, stores db.Stores, orgID, userID, workspaceID, apiAppID, botToken string) slackstore.Connection {
	t.Helper()
	keys := integrations.SlackConnectionKeysFor(workspaceID, apiAppID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO org_slack_connections (
			workspace_id, api_app_id, org_id, source, active, workspace_name, transport,
			bot_user_id, bot_token_ref, signing_secret_ref
		) VALUES ($1, $2, $3, 'org', false, 'Acme', 'events_api', 'U0INACTIVE', $4, $5)
	`, workspaceID, apiAppID, orgID, keys.BotToken, keys.SigningSecret); err != nil {
		t.Fatalf("insert inactive connection: %v", err)
	}
	if botToken != "" {
		if err := stores.Tx.WithTx(t.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Secrets.Put(t.Context(), orgID, keys.BotToken, botToken, "Slack bot token")
		}); err != nil {
			t.Fatalf("store inactive connection's bot token: %v", err)
		}
	}
	conn, err := slackstore.FromStores(stores).Connections.GetByWorkspaceAppSystem(t.Context(), workspaceID, apiAppID)
	if err != nil || conn == nil {
		t.Fatalf("read inactive connection: %v (%v)", conn, err)
	}
	return *conn
}

// Each test below holds one workspace with an active connection and an
// inactive one, and checks that a reader picks the active one.

// TestActiveConnection_LifecycleResolvesTheActiveToken: a mention that arrived
// through the app that is now inactive is acknowledged by the active one.
func TestActiveConnection_LifecycleResolvesTheActiveToken(t *testing.T) {
	h, stores, fake, orgID, owner, _ := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A-active", "xoxb-active")
	insertInactiveConnection(t, h, stores, orgID, owner, "T1", "A-inactive", "xoxb-inactive")
	eventID, _ := seedSlackMessageEvent(t, h, orgID, "T1", "A-inactive", "C1", "1700000000.000100", "", true)

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	caller, token, ok := adapter.resolveBotToken(t.Context(), orgID, "T1")
	if !ok || token != "xoxb-active" {
		t.Fatalf("resolveBotToken = (%q, %v); want the active connection's xoxb-active", token, ok)
	}
	if want := slackBotCaller(orgID, "T1", "A-active"); caller != want {
		t.Errorf("caller = %+v; want %+v", caller, want)
	}

	adapter.dispatch(t.Context(), dispositionEvent(orgID, eventID, domain.EventSlackMessage, events.DispositionTaskCreated), map[string]*conversationEntry{})
	if calls := fake.reactionCalls(); len(calls) != 1 {
		t.Errorf("reactions.add calls = %+v; want the one acknowledgement", calls)
	}
}

// TestActiveConnection_ResolveWorkspace: resolve_workspace answers with the
// active connection, for a conversation with no Slack context and for one
// whose message arrived through the inactive app.
func TestActiveConnection_ResolveWorkspace(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-active-resolve")
	r.seedWorkspace(orgID, owner, "T1", "A-active", "xoxb-active")
	insertInactiveConnection(t, r.h, r.stor, orgID, owner, "T1", "A-inactive", "xoxb-inactive")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	args := json.RawMessage(`{"channel":"C1"}`)
	want := slackWorkspaceIdentity{WorkspaceID: "T1", APIAppID: "A-active"}

	for name, conversationID := range map[string]string{
		"no slack context": r.seedConversation(orgID, teamID, owner, true),
		"message through the inactive app": seedSlackMessageConversation(
			t, r.h, orgID, owner, teamID, "T1", "A-inactive", "C1", "1700000000.000100", "").ConversationID,
	} {
		t.Run(name, func(t *testing.T) {
			info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}
			got, err := slackOpResolveWorkspace(t.Context(), r.stor, info, args)
			if err != nil {
				t.Fatalf("resolve_workspace: %v", err)
			}
			if got != want {
				t.Errorf("resolve_workspace = %+v; want %+v", got, want)
			}
		})
	}
}

// TestActiveConnection_RecordThreadRootResolvesTheActiveConnection: the
// permalink a channel-root post dispatches is resolved with the org's active
// connection in the workspace the sidecar names, not a connection named by an
// app id it supplies.
func TestActiveConnection_RecordThreadRootResolvesTheActiveConnection(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-active-thread-root")
	r.seedWorkspace(orgID, owner, "T1", "A-active", "xoxb-active")
	insertInactiveConnection(t, r.h, r.stor, orgID, owner, "T1", "A-inactive", "xoxb-inactive")
	r.seedChannel(orgID, "T1", "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	args := json.RawMessage(`{"workspace_id":"T1","api_app_id":"A-inactive","channel":"C1","ts":"1700000000.000200","text":"root"}`)
	if _, err := slackOpRecordThreadRoot(t.Context(), r.stor, info, args); err != nil {
		t.Fatalf("record_thread_root: %v", err)
	}
	var tokens []string
	waitForCondition(t, 2*time.Second, func() bool {
		r.fake.permalinkMu.Lock()
		defer r.fake.permalinkMu.Unlock()
		tokens = slices.Clone(r.fake.permalinkTokens)
		return len(tokens) > 0
	})
	if !slices.Equal(tokens, []string{"xoxb-active"}) {
		t.Errorf("chat.getPermalink tokens = %v; want the active connection's xoxb-active", tokens)
	}
}

// TestActiveConnection_ResolveWorkspaceForDownload: with no Slack context, a
// download speaks as the active connection of the org's one workspace that
// has one; a workspace holding only an inactive connection doesn't count, and
// neither does the inactive app beside the active one.
func TestActiveConnection_ResolveWorkspaceForDownload(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-active-download")
	r.seedWorkspace(orgID, owner, "T1", "A-active", "xoxb-active")
	insertInactiveConnection(t, r.h, r.stor, orgID, owner, "T1", "A-inactive", "xoxb-inactive")
	insertInactiveConnection(t, r.h, r.stor, orgID, owner, "T2", "A-only-inactive", "xoxb-t2")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	got, err := slackOpResolveWorkspaceForDownload(t.Context(), r.stor, info, nil)
	if err != nil {
		t.Fatalf("resolve_workspace_download: %v", err)
	}
	if want := (slackWorkspaceIdentity{WorkspaceID: "T1", APIAppID: "A-active"}); got != want {
		t.Errorf("resolve_workspace_download = %+v; want %+v", got, want)
	}
}

// TestActiveConnection_SealedBundle: the sealed set holds the active
// connection's token for the tracked workspace, and not the inactive one's.
func TestActiveConnection_SealedBundle(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-active-sealed")
	r.seedWorkspace(orgID, owner, "T1", "A-active", "xoxb-active")
	insertInactiveConnection(t, r.h, r.stor, orgID, owner, "T1", "A-inactive", "xoxb-inactive")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")

	raw, err := slackProviderCredential(t.Context(), r.stor, agenthost.ProvisionScope{OrgID: orgID, TeamID: teamID})
	if err != nil {
		t.Fatalf("resolve slack credentials: %v", err)
	}
	var creds slackBundleCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("unmarshal sealed set: %v", err)
	}
	want := []slackWorkspaceToken{{WorkspaceID: "T1", APIAppID: "A-active", BotToken: "xoxb-active"}}
	if !slices.Equal(creds.Workspaces, want) {
		t.Errorf("sealed %+v; want %+v", creds.Workspaces, want)
	}
}

// TestActiveConnection_Availability: an org whose only connection is inactive
// is unconfigured, through both probes; adding an active one makes it
// available.
func TestActiveConnection_Availability(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, h, "slack-active-availability")

	probe := func() (eventsource.State, eventsource.State) {
		t.Helper()
		var claims eventsource.State
		if err := stores.Tx.WithReadTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
			var e error
			claims, e = availability(t.Context(), tx, orgID)
			return e
		}); err != nil {
			t.Fatalf("availability: %v", err)
		}
		system, err := availabilitySystem(t.Context(), stores, orgID)
		if err != nil {
			t.Fatalf("availabilitySystem: %v", err)
		}
		return claims, system
	}

	insertInactiveConnection(t, h, stores, orgID, owner, "T1", "A-inactive", "")
	if claims, system := probe(); claims != eventsource.StateUnconfigured || system != eventsource.StateUnconfigured {
		t.Errorf("only an inactive connection: availability = %q / %q; want unconfigured from both", claims, system)
	}

	if err := stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		return upsertActiveConnection(t.Context(), tx, slackstore.Connection{
			WorkspaceID: "T1", APIAppID: "A-active", OrgID: orgID, Transport: transportEventsAPI,
			BotTokenRef: "bot", SigningSecretRef: "sig",
		})
	}); err != nil {
		t.Fatalf("connect the active app: %v", err)
	}
	if claims, system := probe(); claims != eventsource.StateAvailable || system != eventsource.StateAvailable {
		t.Errorf("an active connection beside the inactive one: availability = %q / %q; want available from both", claims, system)
	}
}

// makeInactive connects removalTestApp in workspaceID over transport, then
// makes it the inactive one beside another active app.
func (r *removalRig) makeInactive(workspaceID, transport string) {
	r.t.Helper()
	r.connect(workspaceID, "xoxb-"+workspaceID, transport)
	if _, err := r.h.AdminDB.Exec(`
		UPDATE org_slack_connections SET active = false WHERE workspace_id = $1 AND api_app_id = $2
	`, workspaceID, removalTestApp); err != nil {
		r.t.Fatalf("deactivate: %v", err)
	}
	if err := r.stores.Tx.WithTx(r.t.Context(), r.orgID, r.owner, func(tx db.TxStores) error {
		return upsertActiveConnection(r.t.Context(), tx, slackstore.Connection{
			WorkspaceID: workspaceID, APIAppID: "A0ACTIVE01", OrgID: r.orgID, Transport: transportEventsAPI,
			BotTokenRef: "bot", SigningSecretRef: "sig",
		})
	}); err != nil {
		r.t.Fatalf("connect the active app: %v", err)
	}
}

// TestActiveConnection_InactiveDeliveryDropped: a signed mention delivered
// through an inactive connection, over either transport, is acknowledged and
// produces nothing: no event, and no delivery mark.
func TestActiveConnection_InactiveDeliveryDropped(t *testing.T) {
	mention := mentionInner("C1", "U0PERSON", "1700000000.000100")

	t.Run("webhook", func(t *testing.T) {
		r := newRemovalRig(t)
		r.makeInactive("T0INACT1", transportEventsAPI)
		rec := r.postSigned("T0INACT1", eventCallbackPayload("T0INACT1", removalTestApp, "Ev-inactive", mention))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
			t.Fatalf("delivery = %d %q; want 200 {}", rec.Code, rec.Body.String())
		}
		r.assertDropped()
	})

	t.Run("socket", func(t *testing.T) {
		r := newRemovalRig(t)
		r.makeInactive("T0INACT1", transportSocket)
		c := &appConnection{appID: removalTestApp, orgID: r.orgID, sightings: map[string]*sightingView{}}
		env := socketEnvelope{
			Type: "events_api", EnvelopeID: "env-1",
			Payload: mustJSON(t, eventCallbackPayload("T0INACT1", removalTestApp, "Ev-inactive", mention)),
		}
		if !c.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env) {
			t.Fatal("envelope not acked; want acked")
		}
		r.assertDropped()
	})
}

func (r *removalRig) assertDropped() {
	r.t.Helper()
	if len(*r.published) != 0 {
		r.t.Errorf("published %+v; want nothing", *r.published)
	}
	if got := r.outcomes(); got[dropInactive] != 1 || len(got) != 1 {
		r.t.Errorf("outcomes = %v; want exactly one %q", got, dropInactive)
	}
	var marks int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM slack_event_deliveries`).Scan(&marks); err != nil {
		r.t.Fatalf("count deliveries: %v", err)
	}
	if marks != 0 {
		r.t.Errorf("delivery marks = %d; want 0", marks)
	}
}

// TestActiveConnection_InactiveRemovalApplied: Slack reporting the inactive
// app removed still disconnects it, over either transport, leaving the active
// one.
func TestActiveConnection_InactiveRemovalApplied(t *testing.T) {
	removal := func() map[string]any {
		return removalPayload("T0INACT1", "Ev-removal", time.Now(), appUninstalledInner)
	}
	assertRemoved := func(r *removalRig) {
		r.t.Helper()
		if ws := r.row("T0INACT1"); ws != nil {
			r.t.Errorf("inactive connection after the removal = %+v; want deleted", ws)
		}
		active, err := slackstore.FromStores(r.stores).Connections.ActiveForWorkspaceSystem(r.t.Context(), r.orgID, "T0INACT1")
		if err != nil || active == nil || active.APIAppID != "A0ACTIVE01" {
			r.t.Errorf("active connection after the removal = %+v (%v); want A0ACTIVE01 kept", active, err)
		}
	}

	t.Run("webhook", func(t *testing.T) {
		r := newRemovalRig(t)
		r.makeInactive("T0INACT1", transportEventsAPI)
		if rec := r.postSigned("T0INACT1", removal()); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		assertRemoved(r)
	})

	t.Run("socket", func(t *testing.T) {
		r := newRemovalRig(t)
		r.makeInactive("T0INACT1", transportSocket)
		c := &appConnection{appID: removalTestApp, orgID: r.orgID, sightings: map[string]*sightingView{}}
		env := socketEnvelope{Type: "events_api", EnvelopeID: "env-1", Payload: mustJSON(t, removal())}
		if !c.processEventsAPIEnvelope(t.Context(), r.stores, r.pipeline, env) {
			t.Fatal("envelope not acked; want acked")
		}
		assertRemoved(r)
	})
}
