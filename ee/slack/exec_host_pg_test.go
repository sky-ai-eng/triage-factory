package slack

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// fakeSlackFile is one file fakeExecSlack.files knows about, backing
// files.info + the raw byte download the download verb drives.
type fakeSlackFile struct {
	Name     string
	Content  []byte
	Channels []string
}

// fakeExecSlack is a minimal fake covering the endpoints the exec verbs
// exercise. chat.getPermalink answers a bland "there's a link" response —
// fine, since no test asserts on the best-effort permalink.
type fakeExecSlack struct {
	lastPostText, lastPostMarkdown, lastPostChannel, lastPostThreadTS string
	lastUpdateText, lastUpdateChannel, lastUpdateTS                   string
	lastReactionEmoji, lastReactionChannel, lastReactionTS            string
	postTS                                                            string

	// historyMessages / repliesMessages are returned verbatim for every
	// conversations.history / conversations.replies call in a test — good
	// enough to prove a read verb's request/response wiring end-to-end
	// without modeling per-call window slicing (covered structurally by the
	// CLI-level anchored-argument tests instead).
	historyMessages []map[string]any
	repliesMessages []map[string]any

	// historyCalls / repliesCalls / lastRepliesTS back the root-resolution
	// tests: proving conversations.replies (not conversations.history) is the
	// endpoint resolveMessageRootTS calls, and with which ts.
	historyCalls  int
	repliesCalls  int
	lastRepliesTS string
	// repliesNextCursor, when set, makes every conversations.replies response
	// claim a further page exists (a non-empty next_cursor) — simulates a
	// long thread with more replies beyond page 1, for the pagination-
	// avoidance regression test: resolveMessageRootTS must never follow it.
	repliesNextCursor string

	// userNames maps a Slack user id to the display name users.info answers
	// with — backs the sender-name resolution test.
	userNames map[string]string

	// files backs files.info + the raw download endpoint; keyed by file id.
	files map[string]fakeSlackFile

	// attachCompleteFails makes files.completeUploadExternal (the last leg of
	// the v2 upload flow uploadAttachment drives) answer not-ok, simulating an
	// attach failure after a successful chat.postMessage.
	attachCompleteFails bool
	// lastAttachThreadTS captures the thread_ts files.completeUploadExternal
	// was called with — the field the wrong-thread_ts regression test pins.
	lastAttachThreadTS string
}

func newFakeExecSlack(t *testing.T) *fakeExecSlack {
	t.Helper()
	f := &fakeExecSlack{postTS: "1700000001.000100", userNames: map[string]string{}, files: map[string]fakeSlackFile{}}
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
		case strings.HasSuffix(r.URL.Path, "/conversations.history"):
			f.historyCalls++
			writeSlackFakeJSON(w, map[string]any{"ok": true, "messages": f.historyMessages})
		case strings.HasSuffix(r.URL.Path, "/conversations.replies"):
			f.repliesCalls++
			f.lastRepliesTS = r.URL.Query().Get("ts")
			resp := map[string]any{"ok": true, "messages": f.repliesMessages}
			if f.repliesNextCursor != "" {
				resp["response_metadata"] = map[string]any{"next_cursor": f.repliesNextCursor}
			}
			writeSlackFakeJSON(w, resp)
		case strings.HasSuffix(r.URL.Path, "/files.getUploadURLExternal"):
			writeSlackFakeJSON(w, map[string]any{"ok": true, "upload_url": slackAPIBase + "/fake-upload-target", "file_id": "F-ATTACH"})
		case strings.HasSuffix(r.URL.Path, "/fake-upload-target"):
			writeSlackFakeJSON(w, map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/files.completeUploadExternal"):
			var body struct {
				ThreadTS string `json:"thread_ts"`
			}
			_ = decodeSlackFakeBody(r, &body)
			f.lastAttachThreadTS = body.ThreadTS
			if f.attachCompleteFails {
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "fake_attach_failure"})
				return
			}
			writeSlackFakeJSON(w, map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/users.info"):
			userID := r.URL.Query().Get("user")
			name := f.userNames[userID]
			writeSlackFakeJSON(w, map[string]any{"ok": true, "user": map[string]any{
				"profile": map[string]any{"display_name": name, "real_name": name},
			}})
		case strings.HasSuffix(r.URL.Path, "/files.info"):
			fileID := r.URL.Query().Get("file")
			file, ok := f.files[fileID]
			if !ok {
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "file_not_found"})
				return
			}
			writeSlackFakeJSON(w, map[string]any{"ok": true, "file": map[string]any{
				"id": fileID, "name": file.Name, "mimetype": "application/octet-stream",
				"size": len(file.Content), "url_private": slackAPIBase + "/fake-file-download/" + fileID,
				"channels": file.Channels,
			}})
		case strings.Contains(r.URL.Path, "/fake-file-download/"):
			fileID := strings.TrimPrefix(r.URL.Path, "/fake-file-download/")
			file, ok := f.files[fileID]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(file.Content)
		default:
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "unhandled_path:" + r.URL.Path})
		}
	})
	return f
}

// setFile registers a file's metadata + bytes for files.info/download.
func (f *fakeExecSlack) setFile(fileID, name string, content []byte, channels []string) {
	f.files[fileID] = fakeSlackFile{Name: name, Content: content, Channels: channels}
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

	hdl := &slackExecHandler{client: slackHTTPClient}
	return &slackExecRig{t: t, h: h, stor: stores, hdl: hdl, fake: fake}
}

// rt builds the direct runtime the handler methods run over in these
// all/local-shaped tests: its relays dispatch the Slack policy ops locally
// against the rig's real stores, and ProviderCredential live-resolves the
// sealed bot-token set from those stores — the same path an all/local
// conversation takes (the sidecar path relays instead, exercised in agenthost's relay
// tests).
func (r *slackExecRig) rt(info agenthost.ConversationInfo) agenthost.ExtensionRuntime {
	return agenthost.NewDirectRuntime(r.stor, info)
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

// seedNonSlackTask seeds a minimal entity + event + task chain with a
// GitHub (not slack:message) event type, and returns the task id. Every
// conversation this rig seeds carries one: ConversationStore.GetSystem's column list
// (pgConversationColumns) selects task_id with no COALESCE, so scanning a conversation whose
// task_id is genuinely NULL errors — harmless in production (every conversation
// today is blueprint-origin, which requires a non-NULL task_id) but hit
// immediately here since workspaceFromConversationTaskMetadata unconditionally reads
// the conversation first. Giving every seeded conversation a real, non-slack-mention task
// keeps that read clean while still exercising the intended "no Slack
// context" fallback path (task.EventType != domain.EventSlackMessage) these
// tests are actually about — a GitHub/Jira-triggered conversation using the Slack
// verbs is exactly the "general-purpose" shape the ticket describes.
func (r *slackExecRig) seedNonSlackTask(orgID, creatorID, teamID string) string {
	r.t.Helper()
	// source_id must be unique per (org_id, source) — a fresh uuid per call
	// so a test seeding several conversations (e.g. one per team) doesn't collide on
	// entities_org_id_source_source_id_key.
	entityID := uuid.New().String()
	sourceID := "octo/repo#" + uuid.New().String()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title)
		VALUES ($1, $2, 'github', $3, 'pull_request', 'test pr')
	`, entityID, orgID, sourceID); err != nil {
		r.t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, metadata_json)
		VALUES ($1, $2, $3, $4, '{}')
	`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
		r.t.Fatalf("seed event: %v", err)
	}
	taskID := uuid.New().String()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, taskID, orgID, creatorID, teamID, entityID, domain.EventGitHubPRCICheckFailed, eventID); err != nil {
		r.t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// seedConversation inserts a minimal conversations row the artifacts/external_actions FK can
// point at, matching ConversationInfo.ConversationID, carrying a real task_id (seedNonSlackTask).
// trigger_type/creator_user_id follow the conversations_creator_matches_trigger_type
// CHECK (event ⇒ NULL creator, manual ⇒ non-NULL) — mirrors seedPgArtifactConversation
// (internal/db/postgres/artifacts_test.go).
func (r *slackExecRig) seedConversation(orgID, teamID, userID string, eventTriggered bool) string {
	r.t.Helper()
	taskID := r.seedNonSlackTask(orgID, userID, teamID)
	id := uuid.New().String()
	if eventTriggered {
		if _, err := r.h.AdminDB.Exec(`
			INSERT INTO conversations (id, org_id, team_id, task_id, trigger_type, origin, status, visibility)
			VALUES ($1, $2, $3, $4, 'event', 'interactive', 'running', 'team')
		`, id, orgID, teamID, taskID); err != nil {
			r.t.Fatalf("seed event-triggered conversation: %v", err)
		}
		return id
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, team_id, task_id, creator_user_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'interactive', 'running', 'team')
	`, id, orgID, teamID, taskID, userID); err != nil {
		r.t.Fatalf("seed manual conversation: %v", err)
	}
	return id
}

func (r *slackExecRig) artifactsForConversation(orgID, conversationID string) []domain.Artifact {
	r.t.Helper()
	arts, err := r.stor.Artifacts.ListByConversationSystem(r.t.Context(), orgID, conversationID)
	if err != nil {
		r.t.Fatalf("list artifacts: %v", err)
	}
	return arts
}

func (r *slackExecRig) actionsForConversation(orgID, conversationID string) []domain.ExternalAction {
	r.t.Helper()
	all, _, err := r.stor.ExternalActions.ListByOrgSystem(r.t.Context(), orgID, domain.ExternalActionListOpts{})
	if err != nil {
		r.t.Fatalf("list external actions: %v", err)
	}
	var out []domain.ExternalAction
	for _, a := range all {
		if a.ConversationID == conversationID {
			out = append(out, a)
		}
	}
	return out
}

// touchRole reads conversation_memory_entities.role for (conversationID, entityID) directly — the
// store interface exposes no role-returning read. "" when no row exists.
func (r *slackExecRig) touchRole(conversationID, entityID string) string {
	r.t.Helper()
	var role string
	err := r.h.AdminDB.QueryRow(
		`SELECT role FROM conversation_memory_entities WHERE conversation_id = $1 AND entity_id = $2`, conversationID, entityID,
	).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		r.t.Fatalf("read conversation_memory_entities role: %v", err)
	}
	return role
}

// touchRowCount counts every conversation_memory_entities row for a
// conversation — a set-returning read must leave this at 0.
func (r *slackExecRig) touchRowCount(conversationID string) int {
	r.t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM conversation_memory_entities WHERE conversation_id = $1`, conversationID).Scan(&n); err != nil {
		r.t.Fatalf("count conversation_memory_entities: %v", err)
	}
	return n
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
			conversationID := r.seedConversation(orgID, teamID, owner, eventTriggered)

			info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: eventTriggered}
			out, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{Channel: "C1", Body: "hello **world**"})
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if out.Channel != "C1" || out.TS == "" {
				t.Fatalf("send result = %+v", out)
			}
			if r.fake.lastPostChannel != "C1" || r.fake.lastPostMarkdown != "hello **world**" {
				t.Errorf("chat.postMessage not called as expected: channel=%q markdown=%q", r.fake.lastPostChannel, r.fake.lastPostMarkdown)
			}

			arts := r.artifactsForConversation(orgID, conversationID)
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
			if a.ConversationID != conversationID || a.TeamID != teamID {
				t.Errorf("attribution mismatch: conversation=%q team=%q", a.ConversationID, a.TeamID)
			}

			acts := r.actionsForConversation(orgID, conversationID)
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

// TestSlackExecHandler_Send_RootPostMintsThreadKind pins exec slack send's
// thread-engagement half: a channel-root post (no --thread-ts) is the
// conversation's own thread, so its entity is minted kind="thread" —
// titled from the posted text — the same engagement signal a root mention
// gets at ingest.
func TestSlackExecHandler_Send_RootPostMintsThreadKind(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-send-root-kind")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	out, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{Channel: "C1", Body: "kicking off a new thread"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	sourceID := domain.SlackSourceID("C1", out.TS)
	ent, err := r.stor.Entities.GetBySourceSystem(context.Background(), orgID, "slack", sourceID)
	if err != nil {
		t.Fatalf("GetBySourceSystem: %v", err)
	}
	if ent == nil {
		t.Fatalf("no entity found for source_id %q", sourceID)
	}
	if ent.Kind != "thread" {
		t.Errorf("kind = %q, want thread", ent.Kind)
	}
	if ent.Title != "kicking off a new thread" {
		t.Errorf("title = %q, want the posted text", ent.Title)
	}
}

// TestSlackExecHandler_Send_ThreadedReplyMintsMessageKind pins the negative
// case: a reply into an existing thread (--thread-ts set) is a summons into
// a thread this conversation did not root, so its entity falls to the generic
// touched-entity default of kind="message" — recordThreadRoot never fires
// for it.
func TestSlackExecHandler_Send_ThreadedReplyMintsMessageKind(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-send-reply-kind")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	const rootTS = "1699999999.000001"
	if _, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{
		Channel: "C1", ThreadTS: rootTS, Body: "a reply into someone else's thread",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	sourceID := domain.SlackSourceID("C1", rootTS)
	ent, err := r.stor.Entities.GetBySourceSystem(context.Background(), orgID, "slack", sourceID)
	if err != nil {
		t.Fatalf("GetBySourceSystem: %v", err)
	}
	if ent == nil {
		t.Fatalf("no entity found for source_id %q", sourceID)
	}
	if ent.Kind != "message" {
		t.Errorf("kind = %q, want message", ent.Kind)
	}
}

// TestSlackExecHandler_Send_RootPostThenEdit_PreservesThreadKind pins the
// "find never rewrites kind" invariant end-to-end: editing (and thereby
// re-touching) a root post's own message must not flip its entity back to
// the generic kind="message" default.
func TestSlackExecHandler_Send_RootPostThenEdit_PreservesThreadKind(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-send-root-edit-kind")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	out, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{Channel: "C1", Body: "root message"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := r.hdl.edit(context.Background(), r.rt(info), slackEditArgs{Channel: "C1", TS: out.TS, Body: "edited root message"}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	sourceID := domain.SlackSourceID("C1", out.TS)
	ent, err := r.stor.Entities.GetBySourceSystem(context.Background(), orgID, "slack", sourceID)
	if err != nil {
		t.Fatalf("GetBySourceSystem: %v", err)
	}
	if ent == nil || ent.Kind != "thread" {
		t.Fatalf("entity = %+v, want kind=thread (unchanged by the later edit)", ent)
	}
}

// TestSlackExecHandler_Send_FileOnlyRootPost_TitlesFromAttachmentName pins a
// channel-root send with no --body (a pure file upload): mentionTitle("")
// would otherwise leave the freshly-minted kind="thread" entity's title
// blank, so send() falls back to the attachment's name.
func TestSlackExecHandler_Send_FileOnlyRootPost_TitlesFromAttachmentName(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-send-file-root-title")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	// findRecentMessageTSForFile (the file-only path's ts resolution) scans
	// conversations.history for a message carrying the uploaded file id —
	// "F-ATTACH" is what fakeExecSlack's files.getUploadURLExternal always
	// answers with.
	const postedTS = "1700000005.000100"
	r.fake.historyMessages = []map[string]any{
		{"ts": postedTS, "files": []map[string]any{{"id": "F-ATTACH", "name": "report.pdf"}}},
	}

	out, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{
		Channel: "C1", AttachName: "report.pdf", AttachBase64: base64.StdEncoding.EncodeToString([]byte("file contents")),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if out.TS != postedTS {
		t.Fatalf("send ts = %q, want %q", out.TS, postedTS)
	}

	sourceID := domain.SlackSourceID("C1", out.TS)
	ent, err := r.stor.Entities.GetBySourceSystem(context.Background(), orgID, "slack", sourceID)
	if err != nil {
		t.Fatalf("GetBySourceSystem: %v", err)
	}
	if ent == nil {
		t.Fatalf("no entity found for source_id %q", sourceID)
	}
	if ent.Kind != "thread" {
		t.Errorf("kind = %q, want thread", ent.Kind)
	}
	if ent.Title != "report.pdf" {
		t.Errorf("title = %q, want the attachment name (mentionTitle(\"\") would otherwise leave this blank)", ent.Title)
	}
}

func TestSlackExecHandler_Edit_UpsertsSameArtifactRow(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-edit")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	sendOut, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{Channel: "C1", Body: "first take"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	editOut, err := r.hdl.edit(context.Background(), r.rt(info), slackEditArgs{Channel: "C1", TS: sendOut.TS, Body: "edited take"})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if editOut.TS != sendOut.TS {
		t.Errorf("edit ts = %q, want %q", editOut.TS, sendOut.TS)
	}
	if r.fake.lastUpdateChannel != "C1" || r.fake.lastUpdateTS != sendOut.TS || r.fake.lastUpdateText != "edited take" {
		t.Errorf("chat.update not called as expected: %+v", r.fake)
	}

	arts := r.artifactsForConversation(orgID, conversationID)
	if len(arts) != 1 {
		t.Fatalf("edit should upsert the same row, got %d artifacts: %+v", len(arts), arts)
	}
	if arts[0].State != domain.ArtifactStateMessagePosted {
		t.Errorf("state = %q, want %q", arts[0].State, domain.ArtifactStateMessagePosted)
	}
	if arts[0].ConversationID != conversationID {
		t.Errorf("edit must not reassign conversation_id away from the creating conversation: got %q, want %q", arts[0].ConversationID, conversationID)
	}

	acts := r.actionsForConversation(orgID, conversationID)
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

// TestSlackExecHandler_Edit_PreservesCreatingConversationAndTeam_AcrossDifferentTeams
// pins the artifacts-upsert fix: both conversation_id and team_id stay pinned
// to the message's CREATING conversation, even when a different team's
// conversation (both tracking the same channel) later edits it. The edit's own
// external_action row still attributes to the EDITING conversation/team —
// that's the audit trail's job — but the artifact itself must not silently
// migrate to a different team's reads.
func TestSlackExecHandler_Edit_PreservesCreatingConversationAndTeam_AcrossDifferentTeams(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, r.h, "slack-cross-team")
	teamB := pgtest.SeedTeam(t, r.h, orgID, "slack-cross-team-b")
	// owner is already an org_memberships row (from SeedOrgWithUser) — only
	// the per-team `memberships` row is needed to make them admin of teamB
	// too (AddOrgMember would re-insert org_memberships and violate its PK).
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, owner, teamB)

	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamA, "C1")
	r.trackChannel(orgID, owner, teamB, "C1")

	convA := r.seedConversation(orgID, teamA, owner, true)
	convB := r.seedConversation(orgID, teamB, owner, true)
	infoA := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: convA, TeamID: teamA, IsEventTriggered: true}
	infoB := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: convB, TeamID: teamB, IsEventTriggered: true}

	sendOut, err := r.hdl.send(context.Background(), r.rt(infoA), slackSendArgs{Channel: "C1", Body: "posted by team A"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := r.hdl.edit(context.Background(), r.rt(infoB), slackEditArgs{Channel: "C1", TS: sendOut.TS, Body: "edited by team B"}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	artsA := r.artifactsForConversation(orgID, convA)
	if len(artsA) != 1 {
		t.Fatalf("want 1 artifact still attributed to the creating conversation A, got %d: %+v", len(artsA), artsA)
	}
	if artsA[0].ConversationID != convA || artsA[0].TeamID != teamA {
		t.Errorf("artifact attribution drifted: conversation=%q team=%q, want conversation=%q team=%q", artsA[0].ConversationID, artsA[0].TeamID, convA, teamA)
	}
	if artsB := r.artifactsForConversation(orgID, convB); len(artsB) != 0 {
		t.Errorf("editing conversation B must not have reassigned the artifact to itself, got %+v", artsB)
	}

	actsA := r.actionsForConversation(orgID, convA)
	if len(actsA) != 1 || actsA[0].Action != domain.ActionSlackMessagePosted {
		t.Errorf("conversation A's external_actions = %+v, want exactly one slack_message_posted", actsA)
	}
	actsB := r.actionsForConversation(orgID, convB)
	if len(actsB) != 1 || actsB[0].Action != domain.ActionSlackMessageEdited {
		t.Errorf("conversation B's external_actions = %+v, want exactly one slack_message_edited (the audit trail still credits the editing conversation)", actsB)
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
			conversationID := r.seedConversation(orgID, teamID, owner, eventTriggered)
			info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: eventTriggered}

			if _, err := r.hdl.react(context.Background(), r.rt(info), slackReactArgs{Channel: "C1", TS: "1700000000.000001", Emoji: "thumbsup"}); err != nil {
				t.Fatalf("react: %v", err)
			}
			if r.fake.lastReactionChannel != "C1" || r.fake.lastReactionTS != "1700000000.000001" || r.fake.lastReactionEmoji != "thumbsup" {
				t.Errorf("reactions.add not called as expected: %+v", r.fake)
			}

			if arts := r.artifactsForConversation(orgID, conversationID); len(arts) != 0 {
				t.Errorf("react must not write an artifact, got %+v", arts)
			}
			acts := r.actionsForConversation(orgID, conversationID)
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

// ---------- thread-root resolution (TFAC-621 part 1) ----------

// TestSlackExecHandler_ResolvesRootViaThreadReplies pins the exec_host.go
// fix: resolveMessageRootTS must resolve a reply's thread root via
// conversations.replies — the only Slack endpoint that can, since a reply is
// never returned by conversations.history (channel-level messages only).
// The fake's conversations.replies answers with the thread rooted at rootTS
// (root message first, matching Slack's real ordering when ts names a
// reply); conversations.history answers with an unrelated message carrying
// a DIFFERENT thread_ts, so the pre-fix code (which read history's
// thread_ts) would produce a distinguishably wrong Target if it regressed.
func TestSlackExecHandler_ResolvesRootViaThreadReplies(t *testing.T) {
	const rootTS = "1700000000.000000"
	const replyTS = "1700000005.000200"

	for _, verb := range []string{"edit", "react"} {
		t.Run(verb, func(t *testing.T) {
			r := newSlackExecRig(t)
			orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-root-"+verb)
			r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
			r.seedChannel(orgID, "T1", "C1")
			r.trackChannel(orgID, owner, teamID, "C1")
			conversationID := r.seedConversation(orgID, teamID, owner, true)
			info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

			r.fake.repliesMessages = []map[string]any{
				{"ts": rootTS, "user": "U1", "text": "root message"},
				{"ts": replyTS, "thread_ts": rootTS, "user": "U1", "text": "a reply"},
			}
			r.fake.historyMessages = []map[string]any{
				{"ts": "1699999999.000000", "thread_ts": "1699999999.000000", "user": "U2", "text": "unrelated channel message"},
			}

			wantTarget := domain.SlackSourceID("C1", rootTS)
			switch verb {
			case "edit":
				out, err := r.hdl.edit(context.Background(), r.rt(info), slackEditArgs{Channel: "C1", TS: replyTS, Body: "edited"})
				if err != nil {
					t.Fatalf("edit: %v", err)
				}
				if out.TS != replyTS {
					t.Errorf("edit TS = %q, want %q (the message's own ts, not the root)", out.TS, replyTS)
				}
				arts := r.artifactsForConversation(orgID, conversationID)
				if len(arts) != 1 || arts[0].Target != wantTarget {
					t.Fatalf("artifacts = %+v, want one artifact targeting %q", arts, wantTarget)
				}
			case "react":
				if _, err := r.hdl.react(context.Background(), r.rt(info), slackReactArgs{Channel: "C1", TS: replyTS, Emoji: "eyes"}); err != nil {
					t.Fatalf("react: %v", err)
				}
			}
			acts := r.actionsForConversation(orgID, conversationID)
			if len(acts) != 1 || acts[0].Target != wantTarget {
				t.Fatalf("external_actions = %+v, want one action targeting %q", acts, wantTarget)
			}

			if r.fake.historyCalls != 0 {
				t.Errorf("conversations.history was called %d time(s), want 0 — resolveMessageRootTS must not use it", r.fake.historyCalls)
			}
			if r.fake.repliesCalls != 1 {
				t.Errorf("conversations.replies was called %d time(s), want 1", r.fake.repliesCalls)
			}
			if r.fake.lastRepliesTS != replyTS {
				t.Errorf("conversations.replies ts = %q, want the reply's own ts %q", r.fake.lastRepliesTS, replyTS)
			}
		})
	}
}

// TestSlackExecHandler_ResolvesRootViaThreadReplies_ChannelRootFallback pins
// the channel-root case: a ts with no thread (conversations.replies returns
// just that one message, itself) resolves to itself, unchanged.
func TestSlackExecHandler_ResolvesRootViaThreadReplies_ChannelRootFallback(t *testing.T) {
	const ts = "1700000000.000000"
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-root-fallback")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.repliesMessages = []map[string]any{{"ts": ts, "user": "U1", "text": "an unthreaded message"}}

	if _, err := r.hdl.react(context.Background(), r.rt(info), slackReactArgs{Channel: "C1", TS: ts, Emoji: "eyes"}); err != nil {
		t.Fatalf("react: %v", err)
	}
	wantTarget := domain.SlackSourceID("C1", ts)
	acts := r.actionsForConversation(orgID, conversationID)
	if len(acts) != 1 || acts[0].Target != wantTarget {
		t.Fatalf("external_actions = %+v, want one action targeting %q", acts, wantTarget)
	}
}

// TestSlackExecHandler_ResolvesRootViaThreadReplies_NeverFollowsCursor pins
// the perf-regression fix on top of the endpoint fix: resolveMessageRootTS
// must make exactly ONE conversations.replies request, never following
// Slack's cursor even when the fake reports more pages exist (a non-empty
// next_cursor — standing in for a real thread with hundreds of replies).
// Calling the full-pagination slackConversationsReplies with limit=1 (the
// first cut of this fix) would have cost one HTTP request per reply just to
// reach cursor exhaustion, for a root ts that's already known after page 1
// — resolveMessageRootTS only ever reads msgs[0]. A fake that ignores
// cursors entirely (as the other root-resolution tests' fakes do) can't
// catch that regression, since it would also report only 1 call; this test
// exists specifically to make a second (or Nth) call observable.
func TestSlackExecHandler_ResolvesRootViaThreadReplies_NeverFollowsCursor(t *testing.T) {
	const rootTS = "1700000000.000000"
	const replyTS = "1700000005.000200"
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-root-nocursor")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.repliesMessages = []map[string]any{{"ts": rootTS, "user": "U1", "text": "root message"}}
	r.fake.repliesNextCursor = "dGVhbTpD" // a non-empty cursor: "there's another page"

	if _, err := r.hdl.react(context.Background(), r.rt(info), slackReactArgs{Channel: "C1", TS: replyTS, Emoji: "eyes"}); err != nil {
		t.Fatalf("react: %v", err)
	}
	if r.fake.repliesCalls != 1 {
		t.Errorf("conversations.replies was called %d time(s), want exactly 1 (must not follow next_cursor)", r.fake.repliesCalls)
	}
	wantTarget := domain.SlackSourceID("C1", rootTS)
	acts := r.actionsForConversation(orgID, conversationID)
	if len(acts) != 1 || acts[0].Target != wantTarget {
		t.Fatalf("external_actions = %+v, want one action targeting %q", acts, wantTarget)
	}
}

// ---------- send-path hardening (TFAC-621 part 5) ----------

// TestSlackExecHandler_Send_AttachFailure_StillRecordsPostedMessage pins the
// fix: an uploadAttachment failure after a successful chat.postMessage must
// not leave the already-posted message unrecorded. The verb still returns
// the attach error (loudly, non-nil) but the artifact/audit row for the post
// itself must exist.
func TestSlackExecHandler_Send_AttachFailure_StillRecordsPostedMessage(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-attach-fail")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.attachCompleteFails = true

	out, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{
		Channel: "C1", Body: "here's the report", AttachName: "report.csv",
		AttachBase64: base64.StdEncoding.EncodeToString([]byte("a,b,c")),
	})
	if err == nil {
		t.Fatal("expected the attach failure to surface as an error")
	}
	if !strings.Contains(err.Error(), "attach failed") {
		t.Errorf("error = %q, want it to mention the attach failure", err)
	}
	if out.TS == "" || out.Channel != "C1" {
		t.Errorf("result = %+v, want the posted message's channel/ts even on attach failure", out)
	}

	arts := r.artifactsForConversation(orgID, conversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact for the message that DID post, got %d: %+v", len(arts), arts)
	}
	if arts[0].ExternalID != out.TS || arts[0].State != domain.ArtifactStateMessagePosted {
		t.Errorf("artifact mismatch: %+v", arts[0])
	}
	acts := r.actionsForConversation(orgID, conversationID)
	if len(acts) != 1 || acts[0].Action != domain.ActionSlackMessagePosted {
		t.Fatalf("external_actions = %+v, want one slack_message_posted row despite the attach failure", acts)
	}
}

// TestSlackExecHandler_Send_ThreadedAttach_UsesParentThreadTS pins the fix:
// a threaded send+attach must upload against the PARENT thread ts, never the
// just-posted reply's own ts (Slack's files.completeUploadExternal docs warn
// against passing a reply's ts as thread_ts).
func TestSlackExecHandler_Send_ThreadedAttach_UsesParentThreadTS(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-attach-threaded")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	const parentTS = "1700000000.000000"
	out, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{
		Channel: "C1", ThreadTS: parentTS, Body: "here's the report", AttachName: "report.csv",
		AttachBase64: base64.StdEncoding.EncodeToString([]byte("a,b,c")),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// The fake's postTS is the just-posted REPLY's ts — distinct from parentTS
	// — so a wrong-thread_ts regression (passing the reply's ts) is
	// distinguishable from the fix (passing parentTS).
	if out.TS == parentTS {
		t.Fatalf("test setup: reply ts must differ from parentTS to be a meaningful assertion")
	}
	if r.fake.lastAttachThreadTS != parentTS {
		t.Errorf("files.completeUploadExternal thread_ts = %q, want the parent thread ts %q (not the reply's own ts %q)",
			r.fake.lastAttachThreadTS, parentTS, out.TS)
	}
}

// TestSlackExecHandler_Send_ChannelRootAttach_UsesOwnTS pins the companion
// case: a non-threaded send+attach (the new message IS the root) must
// upload against the message's own ts, since there is no parent to use.
func TestSlackExecHandler_Send_ChannelRootAttach_UsesOwnTS(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-attach-root")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	out, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{
		Channel: "C1", Body: "here's the report", AttachName: "report.csv",
		AttachBase64: base64.StdEncoding.EncodeToString([]byte("a,b,c")),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if r.fake.lastAttachThreadTS != out.TS {
		t.Errorf("files.completeUploadExternal thread_ts = %q, want the posted message's own ts %q", r.fake.lastAttachThreadTS, out.TS)
	}
}

// ---------- authz / resolution edge cases ----------

func TestSlackExecHandler_Send_AuthzRefusal_TeamDoesNotTrackChannel(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-authz")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	// Deliberately NOT tracked by teamID.
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	_, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{Channel: "C1", Body: "hi"})
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
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	// authorizeChannel runs before channel resolution and would itself refuse
	// (an unregistered channel is never tracked either) — track it explicitly
	// so this test isolates the channel-registry lookup, not authz.
	r.trackChannel(orgID, owner, teamID, "C1")

	_, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{Channel: "C1", Body: "hi"})
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
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	_, err := r.hdl.send(context.Background(), r.rt(info), slackSendArgs{Channel: "C1", Body: "hi"})
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

// ---------- read thread / read channel ----------

func TestSlackExecHandler_ReadThread_Golden(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-read-thread")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.userNames["U1"] = "Ada Lovelace"
	r.fake.repliesMessages = []map[string]any{
		{"ts": "1700000000.000000", "user": "U1", "text": "root message", "files": []map[string]any{{"id": "F1", "name": "spec.pdf"}}},
		{"ts": "1700000000.000001", "thread_ts": "1700000000.000000", "user": "U1", "text": "a reply"},
	}

	out, err := r.hdl.readThread(context.Background(), r.rt(info), slackReadThreadArgs{Channel: "C1", TS: "1700000000.000000", Limit: 50})
	if err != nil {
		t.Fatalf("readThread: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(out), out)
	}
	if out[0].SenderName != "Ada Lovelace" {
		t.Errorf("SenderName = %q, want Ada Lovelace (users.info resolution)", out[0].SenderName)
	}
	if len(out[0].Files) != 1 || out[0].Files[0].ID != "F1" || out[0].Files[0].Name != "spec.pdf" {
		t.Errorf("Files = %+v, want [{F1 spec.pdf}]", out[0].Files)
	}
	if out[1].ThreadTS != "1700000000.000000" {
		t.Errorf("reply ThreadTS = %q, want the root ts", out[1].ThreadTS)
	}
}

func TestSlackExecHandler_ReadChannel_LimitOnly_Golden(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-read-channel")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.historyMessages = []map[string]any{
		{"ts": "1700000002.000000", "user": "U2", "text": "third"},
		{"ts": "1700000000.000000", "user": "U2", "text": "first"},
		{"ts": "1700000001.000000", "user": "U2", "text": "second"},
	}

	out, err := r.hdl.readChannel(context.Background(), r.rt(info), slackReadChannelArgs{Channel: "C1", Limit: 20})
	if err != nil {
		t.Fatalf("readChannel: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(out), out)
	}
	wantOrder := []string{"first", "second", "third"}
	for i, m := range out {
		if m.Text != wantOrder[i] {
			t.Errorf("position %d = %q, want %q (sortMessagesByTS should have reordered): %+v", i, m.Text, wantOrder[i], out)
		}
	}
}

func TestSlackExecHandler_ReadChannel_AuthzRefusal(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-read-authz")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	// Deliberately NOT tracked.
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	if _, err := r.hdl.readChannel(context.Background(), r.rt(info), slackReadChannelArgs{Channel: "C1", Limit: 10}); err == nil {
		t.Fatal("expected an authz refusal, got nil")
	} else if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error = %q, want it to mention the team doesn't track the channel", err)
	}
}

// TestSlackExecHandler_ReadThread_RecordsTouch pins the addressed-read half of
// the touched-entity rule for Slack: `read thread` targets one thread by
// (channel, root ts), so it records a durable conversation→entity touch keyed on the
// same SlackSourceID the ingest pipeline uses — a bot read and a human mention
// resolve to the identical entity.
func TestSlackExecHandler_ReadThread_RecordsTouch(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-read-thread-touch")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	const rootTS = "1700000000.000100"
	r.fake.repliesMessages = []map[string]any{
		{"ts": rootTS, "user": "U1", "text": "root message"},
	}

	if _, err := r.hdl.readThread(context.Background(), r.rt(info), slackReadThreadArgs{Channel: "C1", TS: rootTS, Limit: 50}); err != nil {
		t.Fatalf("readThread: %v", err)
	}

	sourceID := domain.SlackSourceID("C1", rootTS)
	ent, err := r.stor.Entities.GetBySourceSystem(context.Background(), orgID, "slack", sourceID)
	if err != nil {
		t.Fatalf("GetBySourceSystem: %v", err)
	}
	if ent == nil {
		t.Fatalf("read thread recorded no touched entity for source_id %q", sourceID)
	}
	if ent.Kind != "message" {
		t.Errorf("entity kind = %q, want message", ent.Kind)
	}
	if role := r.touchRole(conversationID, ent.ID); role != domain.MemoryRoleTouched {
		t.Errorf("touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}
}

// TestSlackExecHandler_ReadChannel_RecordsNoTouch is the negative: `read
// channel` returns a set, so it never records a touch — the rule is addressed →
// touch, returned-in-a-set → never.
func TestSlackExecHandler_ReadChannel_RecordsNoTouch(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-read-channel-notouch")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.historyMessages = []map[string]any{
		{"ts": "1700000000.000000", "user": "U2", "text": "hello"},
	}
	if _, err := r.hdl.readChannel(context.Background(), r.rt(info), slackReadChannelArgs{Channel: "C1", Limit: 20}); err != nil {
		t.Fatalf("readChannel: %v", err)
	}

	if n := r.touchRowCount(conversationID); n != 0 {
		t.Errorf("read channel recorded %d touch row(s), want 0", n)
	}
}

// ---------- download ----------

func TestSlackExecHandler_Download_Golden(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-download")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "report.csv", []byte("a,b,c\n1,2,3\n"), []string{"C1"})

	out, err := r.hdl.download(context.Background(), r.rt(info), slackDownloadArgs{FileID: "F1"})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if out.Name != "report.csv" {
		t.Errorf("Name = %q, want report.csv", out.Name)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.Base64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(decoded) != "a,b,c\n1,2,3\n" {
		t.Errorf("decoded content = %q, want the seeded file bytes", decoded)
	}
}

// TestSlackExecHandler_Download_UntrackedChannel_Refused pins the fix for
// the gap Copilot's review missed and a later human review caught: download
// has no --channel of its own, so it must authorize against the FILE's own
// channel membership (files.info) rather than skip authorization entirely.
// The file here is shared only into a channel the team does NOT track.
func TestSlackExecHandler_Download_UntrackedChannel_Refused(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-dl-untracked")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.seedChannel(orgID, "T1", "C-untracked")
	r.trackChannel(orgID, owner, teamID, "C1") // tracks C1, NOT C-untracked
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "secret.csv", []byte("data"), []string{"C-untracked"})

	_, err := r.hdl.download(context.Background(), r.rt(info), slackDownloadArgs{FileID: "F1"})
	if err == nil {
		t.Fatal("expected a refusal — the file is shared only into an untracked channel")
	}
	if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error = %q, want it to mention the team doesn't track the file's channel(s)", err)
	}
}

// TestSlackExecHandler_Download_NoChannels_Refused pins that a file shared
// nowhere channel-scoped (DM-only, or Slack simply reports none) has nothing
// to authorize against and is refused rather than allowed by default.
func TestSlackExecHandler_Download_NoChannels_Refused(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-dl-nochan")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	// The team tracks a home channel in T1 so the claim's bundle holds a
	// sealed token for that workspace (a conversation with NO tracked channel
	// has no Slack authorization at all and is refused before files.info — a
	// separate, stricter path). The file below is shared NOWHERE
	// channel-scoped, so the file-channel gate is what must refuse it.
	r.seedChannel(orgID, "T1", "C-home")
	r.trackChannel(orgID, owner, teamID, "C-home")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "dm-only.csv", []byte("data"), nil)

	_, err := r.hdl.download(context.Background(), r.rt(info), slackDownloadArgs{FileID: "F1"})
	if err == nil {
		t.Fatal("expected a refusal — the file has no channel to authorize against")
	}
	if !strings.Contains(err.Error(), "isn't shared into any channel") {
		t.Errorf("error = %q, want it to explain there's no channel to authorize against", err)
	}
}

// TestSlackExecHandler_Download_FallbackPath_StillAuthorizes is the direct
// regression test for the gap: a conversation with NO Slack task context (so
// opResolveWorkspaceForDownload takes the "org's sole connected workspace"
// fallback, which — before this fix — returned a token with zero channel
// authorization at all) must still be refused when the requested file
// belongs only to a channel this team doesn't track.
func TestSlackExecHandler_Download_FallbackPath_StillAuthorizes(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-dl-fallback")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test") // exactly one connected workspace
	r.seedChannel(orgID, "T1", "C-untracked")
	// The team tracks a DIFFERENT (home) channel in T1, so the claim's bundle
	// holds a sealed token for that workspace — but NOT for the file's channel.
	// This conversation's seeded task is a plain GitHub task
	// (seedNonSlackTask), so the resolve-workspace-for-download op reports "no
	// Slack context" and falls through to the org-wide sole-workspace path; the
	// file-channel gate is what must then refuse the download of a file in the
	// untracked channel.
	r.seedChannel(orgID, "T1", "C-home")
	r.trackChannel(orgID, owner, teamID, "C-home")
	conversationID := r.seedConversation(orgID, teamID, owner, true)
	info := agenthost.ConversationInfo{OrgID: orgID, UserID: owner, ConversationID: conversationID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "leak.csv", []byte("data"), []string{"C-untracked"})

	_, err := r.hdl.download(context.Background(), r.rt(info), slackDownloadArgs{FileID: "F1"})
	if err == nil {
		t.Fatal("expected the fallback (no-task-context) path to still enforce channel authorization")
	}
	if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error = %q, want it to mention the team doesn't track the file's channel(s)", err)
	}
}

// ---------- registration / entitlement gate (mirrors the TFAC-526 extension tests) ----------

func TestSlackExtension_RefusedWithoutEntitlement(t *testing.T) {
	// The "slack" provider is registered from this package's init(); the gate
	// checks the conversation's org entitlement (here: none), so the call is refused.
	entitlements.RegisterProvider(entitlements.Static()) // nothing entitled
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "slack-ext-gate")
	client := agenthost.NewLocal(stores, agenthost.ConversationInfo{OrgID: orgID, UserID: owner, TeamID: teamID, ConversationID: "r1"})

	_, err := client.CallExtension(context.Background(), "slack", "send", nil)
	if err == nil {
		t.Fatal("expected a 'not enabled' refusal with no Slack entitlement")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error = %q, want it to mention 'not enabled'", err)
	}
}

func TestSlackExtension_EntitledDispatchesToHandler(t *testing.T) {
	// The "slack" provider is registered from this package's init(); with the
	// feature entitled, the gate passes and dispatch reaches the handler.
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "slack-ext-entitled")
	client := agenthost.NewLocal(stores, agenthost.ConversationInfo{OrgID: orgID, UserID: owner, TeamID: teamID, ConversationID: "r1"})

	// An unknown method reaching the real handler (not refused by the
	// entitlement gate) confirms dispatch happened — the gate only blocks
	// unentitled orgs, never a legitimate call into an unrecognized method.
	_, err := client.CallExtension(context.Background(), "slack", "not-a-real-method", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("error = %v, want the handler's own 'unknown method' error (proving dispatch reached it)", err)
	}
}

// TestSlackProviderCredential_SealsOnlyTrackedWorkspaces is the sealed-set
// scoping guarantee: the brain seals a bot token ONLY for the workspaces of the
// conversation's team's tracked channels — the authorizable set. A connected workspace
// the team does not track never enters the claim's bundle, so a compromised
// sidecar cannot reach it (there is no token to select).
func TestSlackProviderCredential_SealsOnlyTrackedWorkspaces(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-sealed-set")

	// Two connected workspaces; the team tracks a channel in the first only.
	r.seedWorkspace(orgID, owner, "T-tracked", "A-tracked", "xoxb-tracked")
	r.seedWorkspace(orgID, owner, "T-other", "A-other", "xoxb-other")
	r.seedChannel(orgID, "T-tracked", "C-tracked")
	r.seedChannel(orgID, "T-other", "C-other") // exists, but this team tracks it NOT
	r.trackChannel(orgID, owner, teamID, "C-tracked")

	raw, err := slackProviderCredential(context.Background(), r.stor, agenthost.ProvisionScope{OrgID: orgID, TeamID: teamID})
	if err != nil {
		t.Fatalf("resolve slack credentials: %v", err)
	}
	var creds slackBundleCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("unmarshal sealed set: %v", err)
	}
	if len(creds.Workspaces) != 1 {
		t.Fatalf("sealed %d workspaces, want exactly 1 (the tracked one): %+v", len(creds.Workspaces), creds.Workspaces)
	}
	if got := creds.Workspaces[0]; got.WorkspaceID != "T-tracked" || got.APIAppID != "A-tracked" || got.BotToken != "xoxb-tracked" {
		t.Fatalf("sealed the wrong workspace: %+v", got)
	}
	// The untracked workspace's token must be unreachable from the sealed set.
	if tok, ok := creds.tokenFor("T-other", "A-other"); ok || tok != "" {
		t.Fatalf("untracked workspace T-other leaked into the sealed set: tok=%q ok=%v", tok, ok)
	}
}
