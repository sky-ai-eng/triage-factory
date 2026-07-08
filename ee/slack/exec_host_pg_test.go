package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ---------- fake Slack API (chat.postMessage/update, reactions.add, reads) ----------

// fakeExecSlack is a minimal fake covering the endpoints the exec verbs
// exercise. conversations.history/replies/chat.getPermalink answer a bland
// "nothing there" response — fine for these tests, since none of them assert
// on read output or on the best-effort permalink.
type fakeExecSlack struct {
	lastPostText, lastPostMarkdown, lastPostChannel, lastPostThreadTS string
	lastUpdateText, lastUpdateChannel, lastUpdateTS                   string
	lastReactionEmoji, lastReactionChannel, lastReactionTS            string
	postTS                                                            string
}

func newFakeExecSlack(t *testing.T) *fakeExecSlack {
	t.Helper()
	f := &fakeExecSlack{postTS: "1700000001.000100"}
	withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat.postMessage"):
			var body struct {
				Channel  string `json:"channel"`
				Text     string `json:"text"`
				ThreadTS string `json:"thread_ts"`
				Blocks   []struct {
					Text string `json:"text"`
				} `json:"blocks"`
			}
			_ = decodeSlackFakeBody(r, &body)
			f.lastPostChannel, f.lastPostText, f.lastPostThreadTS = body.Channel, body.Text, body.ThreadTS
			if len(body.Blocks) > 0 {
				f.lastPostMarkdown = body.Blocks[0].Text
			}
			writeSlackFakeJSON(w, map[string]any{"ok": true, "ts": f.postTS})
		case strings.HasSuffix(r.URL.Path, "/chat.update"):
			var body struct {
				Channel string `json:"channel"`
				TS      string `json:"ts"`
				Text    string `json:"text"`
			}
			_ = decodeSlackFakeBody(r, &body)
			f.lastUpdateChannel, f.lastUpdateTS, f.lastUpdateText = body.Channel, body.TS, body.Text
			writeSlackFakeJSON(w, map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/reactions.add"):
			_ = r.ParseForm()
			f.lastReactionChannel = r.FormValue("channel")
			f.lastReactionTS = r.FormValue("timestamp")
			f.lastReactionEmoji = r.FormValue("name")
			writeSlackFakeJSON(w, map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/chat.getPermalink"):
			writeSlackFakeJSON(w, map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/x"})
		case strings.HasSuffix(r.URL.Path, "/conversations.history"), strings.HasSuffix(r.URL.Path, "/conversations.replies"):
			writeSlackFakeJSON(w, map[string]any{"ok": true, "messages": []map[string]any{}})
		default:
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "unhandled_path:" + r.URL.Path})
		}
	})
	return f
}

// decodeSlackFakeBody decodes a JSON request body (chat.postMessage/
// chat.update send JSON via slackPostJSON) into v.
func decodeSlackFakeBody(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- rig ----------

type slackExecRig struct {
	t    *testing.T
	h    *pgtest.Harness
	stor db.Stores
	hdl  *slackExecHandler
	fake *fakeExecSlack
}

func newSlackExecRig(t *testing.T) *slackExecRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	fake := newFakeExecSlack(t)

	hdl := &slackExecHandler{stores: stores, client: slackHTTPClient}
	return &slackExecRig{t: t, h: h, stor: stores, hdl: hdl, fake: fake}
}

// seedWorkspace connects a Slack workspace (bot token + org_slack_workspaces
// row) for orgID, mirroring channels_handler_pg_test.go's seedWorkspace.
func (r *slackExecRig) seedWorkspace(orgID, userID, workspaceID, apiAppID, botToken string) {
	r.t.Helper()
	botTokenRef := "slack_ws_" + workspaceID + "_" + apiAppID + "_bot_token"
	if err := r.stor.Tx.WithTx(r.t.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Secrets.Put(r.t.Context(), orgID, botTokenRef, botToken, "test bot token"); err != nil {
			return err
		}
		return slackstore.FromTx(tx).Workspaces.Upsert(r.t.Context(), slackstore.Workspace{
			WorkspaceID: workspaceID, APIAppID: apiAppID, OrgID: orgID, WorkspaceName: "Acme",
			Transport: "socket", BotUserID: "U0BOT", BotTokenRef: botTokenRef, RegisteredByUserID: userID,
		})
	}); err != nil {
		r.t.Fatalf("seed workspace: %v", err)
	}
}

// seedChannel registers channelID in the org's channel registry, pointed at
// workspaceID.
func (r *slackExecRig) seedChannel(orgID, workspaceID, channelID string) {
	r.t.Helper()
	admin := slackstore.FromStores(r.stor)
	if err := admin.Channels.EnsureSystem(r.t.Context(), orgID, workspaceID, channelID, "general"); err != nil {
		r.t.Fatalf("ensure channel: %v", err)
	}
}

// trackChannel makes teamID a tracker of channelID, via the app-pool
// team-admin-gated ReplaceForTeam under userID's claims (userID must be a
// team admin — pgtest.SeedOrgWithUser's owner qualifies).
func (r *slackExecRig) trackChannel(orgID, userID, teamID, channelID string) {
	r.t.Helper()
	if err := r.stor.Tx.WithTx(r.t.Context(), orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(r.t.Context(), orgID, teamID, []string{channelID})
	}); err != nil {
		r.t.Fatalf("track channel: %v", err)
	}
}

// seedRun inserts a minimal run row the artifacts/external_actions FK can
// point at, matching RunInfo.RunID. trigger_type/creator_user_id follow the
// runs_creator_matches_trigger_type CHECK (event ⇒ NULL creator, manual ⇒
// non-NULL) — mirrors seedPgArtifactRun (internal/db/postgres/artifacts_test.go).
func (r *slackExecRig) seedRun(orgID, teamID, userID string, eventTriggered bool) string {
	r.t.Helper()
	id := uuid.New().String()
	if eventTriggered {
		if _, err := r.h.AdminDB.Exec(`
			INSERT INTO runs (id, org_id, team_id, trigger_type, origin, status, visibility)
			VALUES ($1, $2, $3, 'event', 'interactive', 'running', 'team')
		`, id, orgID, teamID); err != nil {
			r.t.Fatalf("seed event-triggered run: %v", err)
		}
		return id
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'running', 'team')
	`, id, orgID, teamID, userID); err != nil {
		r.t.Fatalf("seed manual run: %v", err)
	}
	return id
}

func (r *slackExecRig) artifactsForRun(orgID, runID string) []domain.Artifact {
	r.t.Helper()
	arts, err := r.stor.Artifacts.ListByRunSystem(r.t.Context(), orgID, runID)
	if err != nil {
		r.t.Fatalf("list artifacts: %v", err)
	}
	return arts
}

func (r *slackExecRig) actionsForRun(orgID, runID string) []domain.ExternalAction {
	r.t.Helper()
	all, err := r.stor.ExternalActions.ListByOrgSystem(r.t.Context(), orgID, domain.ExternalActionListOpts{})
	if err != nil {
		r.t.Fatalf("list external actions: %v", err)
	}
	var out []domain.ExternalAction
	for _, a := range all {
		if a.RunID == runID {
			out = append(out, a)
		}
	}
	return out
}

// ---------- send / edit / react golden paths ----------

func TestSlackExecHandler_Send_RecordsArtifactAndAction(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			r := newSlackExecRig(t)
			orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-send-"+name)
			r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
			r.seedChannel(orgID, "T1", "C1")
			r.trackChannel(orgID, owner, teamID, "C1")
			runID := r.seedRun(orgID, teamID, owner, eventTriggered)

			info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: eventTriggered}
			out, err := r.hdl.send(context.Background(), info, slackSendArgs{Channel: "C1", Body: "hello **world**"})
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if out.Channel != "C1" || out.TS == "" {
				t.Fatalf("send result = %+v", out)
			}
			if r.fake.lastPostChannel != "C1" || r.fake.lastPostMarkdown != "hello **world**" {
				t.Errorf("chat.postMessage not called as expected: channel=%q markdown=%q", r.fake.lastPostChannel, r.fake.lastPostMarkdown)
			}

			arts := r.artifactsForRun(orgID, runID)
			if len(arts) != 1 {
				t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
			}
			a := arts[0]
			wantTarget := domain.SlackSourceID("C1", out.TS)
			if a.Provider != domain.ArtifactProviderSlack || a.Kind != domain.ArtifactKindMessage ||
				a.Target != wantTarget || a.ExternalID != out.TS || a.State != domain.ArtifactStateMessagePosted ||
				a.DedupKey != domain.ArtifactDedupKey(domain.ArtifactProviderSlack, domain.ArtifactKindMessage, "C1/"+out.TS, "") {
				t.Errorf("artifact mismatch: %+v (want target=%q)", a, wantTarget)
			}
			if a.RunID != runID || a.TeamID != teamID {
				t.Errorf("attribution mismatch: run=%q team=%q", a.RunID, a.TeamID)
			}

			acts := r.actionsForRun(orgID, runID)
			if len(acts) != 1 {
				t.Fatalf("want 1 external_action, got %d: %+v", len(acts), acts)
			}
			act := acts[0]
			if act.Provider != domain.ArtifactProviderSlack || act.Action != domain.ActionSlackMessagePosted ||
				act.Credential != domain.CredentialSlackBot || act.Target != wantTarget {
				t.Errorf("external_action mismatch: %+v", act)
			}
		})
	}
}

func TestSlackExecHandler_Edit_UpsertsSameArtifactRow(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-edit")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	sendOut, err := r.hdl.send(context.Background(), info, slackSendArgs{Channel: "C1", Body: "first take"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	editOut, err := r.hdl.edit(context.Background(), info, slackEditArgs{Channel: "C1", TS: sendOut.TS, Body: "edited take"})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if editOut.TS != sendOut.TS {
		t.Errorf("edit ts = %q, want %q", editOut.TS, sendOut.TS)
	}
	if r.fake.lastUpdateChannel != "C1" || r.fake.lastUpdateTS != sendOut.TS || r.fake.lastUpdateText != "edited take" {
		t.Errorf("chat.update not called as expected: %+v", r.fake)
	}

	arts := r.artifactsForRun(orgID, runID)
	if len(arts) != 1 {
		t.Fatalf("edit should upsert the same row, got %d artifacts: %+v", len(arts), arts)
	}
	if arts[0].State != domain.ArtifactStateMessagePosted {
		t.Errorf("state = %q, want %q", arts[0].State, domain.ArtifactStateMessagePosted)
	}
	if arts[0].RunID != runID {
		t.Errorf("edit must not reassign run_id away from the creating run: got %q, want %q", arts[0].RunID, runID)
	}

	acts := r.actionsForRun(orgID, runID)
	if len(acts) != 2 {
		t.Fatalf("want 2 external_actions (post + edit), got %d: %+v", len(acts), acts)
	}
	sawPosted, sawEdited := false, false
	for _, a := range acts {
		switch a.Action {
		case domain.ActionSlackMessagePosted:
			sawPosted = true
		case domain.ActionSlackMessageEdited:
			sawEdited = true
		}
	}
	if !sawPosted || !sawEdited {
		t.Errorf("expected both slack_message_posted and slack_message_edited, got %+v", acts)
	}
}

func TestSlackExecHandler_React_RecordsActionOnly(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			r := newSlackExecRig(t)
			orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-react-"+name)
			r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
			r.seedChannel(orgID, "T1", "C1")
			r.trackChannel(orgID, owner, teamID, "C1")
			runID := r.seedRun(orgID, teamID, owner, eventTriggered)
			info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: eventTriggered}

			if _, err := r.hdl.react(context.Background(), info, slackReactArgs{Channel: "C1", TS: "1700000000.000001", Emoji: "thumbsup"}); err != nil {
				t.Fatalf("react: %v", err)
			}
			if r.fake.lastReactionChannel != "C1" || r.fake.lastReactionTS != "1700000000.000001" || r.fake.lastReactionEmoji != "thumbsup" {
				t.Errorf("reactions.add not called as expected: %+v", r.fake)
			}

			if arts := r.artifactsForRun(orgID, runID); len(arts) != 0 {
				t.Errorf("react must not write an artifact, got %+v", arts)
			}
			acts := r.actionsForRun(orgID, runID)
			if len(acts) != 1 {
				t.Fatalf("want 1 external_action, got %d: %+v", len(acts), acts)
			}
			if acts[0].Action != domain.ActionSlackReactionAdded || acts[0].Credential != domain.CredentialSlackBot {
				t.Errorf("external_action mismatch: %+v", acts[0])
			}
			if !strings.Contains(acts[0].DetailJSON, "thumbsup") {
				t.Errorf("detail_json missing emoji: %q", acts[0].DetailJSON)
			}
		})
	}
}

// ---------- authz / resolution edge cases ----------

func TestSlackExecHandler_Send_AuthzRefusal_TeamDoesNotTrackChannel(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-authz")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	// Deliberately NOT tracked by teamID.
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	_, err := r.hdl.send(context.Background(), info, slackSendArgs{Channel: "C1", Body: "hi"})
	if err == nil {
		t.Fatal("expected an authz refusal, got nil error")
	}
	if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error = %q, want it to mention the team doesn't track the channel", err)
	}
	if r.fake.lastPostChannel != "" {
		t.Error("must not call Slack when the team doesn't track the channel")
	}
}

func TestSlackExecHandler_Send_UnknownChannel(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-unknown-chan")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	// No Channels.EnsureSystem call — C1 is not in the registry at all.
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	// authorizeChannel runs before channel resolution and would itself refuse
	// (an unregistered channel is never tracked either) — track it explicitly
	// so this test isolates the channel-registry lookup, not authz.
	r.trackChannel(orgID, owner, teamID, "C1")

	_, err := r.hdl.send(context.Background(), info, slackSendArgs{Channel: "C1", Body: "hi"})
	if err == nil {
		t.Fatal("expected an unknown-channel error, got nil")
	}
	if !strings.Contains(err.Error(), "not visible") {
		t.Errorf("error = %q, want it to mention the channel isn't visible to TF", err)
	}
}

func TestSlackExecHandler_Send_WorkspaceAmbiguity_TwoAppsOneWorkspace(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-ambiguous")
	// Two apps (A1, A2) both installed into the SAME Slack workspace (T1),
	// under the same org — the exact TFAC-533 shape the ticket calls out.
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-app1")
	r.seedWorkspace(orgID, owner, "T1", "A2", "xoxb-app2")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	_, err := r.hdl.send(context.Background(), info, slackSendArgs{Channel: "C1", Body: "hi"})
	if err == nil {
		t.Fatal("expected an ambiguity refusal, got nil")
	}
	if !strings.Contains(err.Error(), "cannot determine which bot identity") {
		t.Errorf("error = %q, want it to name the ambiguity", err)
	}
	if r.fake.lastPostChannel != "" {
		t.Error("must never guess which bot identity to act as")
	}
}

// ---------- registration / entitlement gate (mirrors the TFAC-526 extension tests) ----------

func TestSlackExtension_RefusedWithoutEntitlement(t *testing.T) {
	t.Cleanup(agenthost.ResetExtensions)
	entitlements.RegisterProvider(entitlements.Static()) // nothing entitled
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	registerSlackExec(stores)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "slack-ext-gate")
	client := agenthost.NewLocal(stores, agenthost.RunInfo{OrgID: orgID, UserID: owner, TeamID: teamID, RunID: "r1"})

	_, err := client.CallExtension(context.Background(), "slack", "send", nil)
	if err == nil {
		t.Fatal("expected a 'not enabled' refusal with no Slack entitlement")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error = %q, want it to mention 'not enabled'", err)
	}
}

func TestSlackExtension_EntitledDispatchesToHandler(t *testing.T) {
	t.Cleanup(agenthost.ResetExtensions)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	registerSlackExec(stores)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "slack-ext-entitled")
	client := agenthost.NewLocal(stores, agenthost.RunInfo{OrgID: orgID, UserID: owner, TeamID: teamID, RunID: "r1"})

	// An unknown method reaching the real handler (not refused by the
	// entitlement gate) confirms dispatch happened — the gate only blocks
	// unentitled orgs, never a legitimate call into an unrecognized method.
	_, err := client.CallExtension(context.Background(), "slack", "not-a-real-method", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("error = %v, want the handler's own 'unknown method' error (proving dispatch reached it)", err)
	}
}
