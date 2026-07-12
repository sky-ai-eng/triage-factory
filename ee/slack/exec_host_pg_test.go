package slack

import (
	"context"
	"encoding/base64"
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

// seedNonSlackTask seeds a minimal entity + event + task chain with a
// GitHub (not slack:mention) event type, and returns the task id. Every run
// this rig seeds carries one: AgentRunStore.GetSystem's column list
// (pgRunColumns) selects task_id with no COALESCE, so scanning a run whose
// task_id is genuinely NULL errors — harmless in production (every run
// today is blueprint-origin, which requires a non-NULL task_id) but hit
// immediately here since workspaceFromRunTaskMetadata unconditionally reads
// the run first. Giving every seeded run a real, non-slack-mention task
// keeps that read clean while still exercising the intended "no Slack
// context" fallback path (task.EventType != domain.EventSlackMention) these
// tests are actually about — a GitHub/Jira-triggered run using the Slack
// verbs is exactly the "general-purpose" shape the ticket describes.
func (r *slackExecRig) seedNonSlackTask(orgID, creatorID, teamID string) string {
	r.t.Helper()
	// source_id must be unique per (org_id, source) — a fresh uuid per call
	// so a test seeding several runs (e.g. one per team) doesn't collide on
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

// seedRun inserts a minimal run row the artifacts/external_actions FK can
// point at, matching RunInfo.RunID, carrying a real task_id (seedNonSlackTask).
// trigger_type/creator_user_id follow the runs_creator_matches_trigger_type
// CHECK (event ⇒ NULL creator, manual ⇒ non-NULL) — mirrors seedPgArtifactRun
// (internal/db/postgres/artifacts_test.go).
func (r *slackExecRig) seedRun(orgID, teamID, userID string, eventTriggered bool) string {
	r.t.Helper()
	taskID := r.seedNonSlackTask(orgID, userID, teamID)
	id := uuid.New().String()
	if eventTriggered {
		if _, err := r.h.AdminDB.Exec(`
			INSERT INTO runs (id, org_id, team_id, task_id, trigger_type, origin, status, visibility)
			VALUES ($1, $2, $3, $4, 'event', 'interactive', 'running', 'team')
		`, id, orgID, teamID, taskID); err != nil {
			r.t.Fatalf("seed event-triggered run: %v", err)
		}
		return id
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, team_id, task_id, creator_user_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'interactive', 'running', 'team')
	`, id, orgID, teamID, taskID, userID); err != nil {
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

// TestSlackExecHandler_Edit_PreservesCreatingRunAndTeam_AcrossDifferentTeams
// pins the artifacts-upsert fix: both run_id and team_id stay pinned to the
// message's CREATING run, even when a different team's run (both tracking
// the same channel) later edits it. The edit's own external_action row still
// attributes to the EDITING run/team — that's the audit trail's job — but
// the artifact itself must not silently migrate to a different team's reads.
func TestSlackExecHandler_Edit_PreservesCreatingRunAndTeam_AcrossDifferentTeams(t *testing.T) {
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

	runA := r.seedRun(orgID, teamA, owner, true)
	runB := r.seedRun(orgID, teamB, owner, true)
	infoA := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runA, TeamID: teamA, IsEventTriggered: true}
	infoB := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runB, TeamID: teamB, IsEventTriggered: true}

	sendOut, err := r.hdl.send(context.Background(), infoA, slackSendArgs{Channel: "C1", Body: "posted by team A"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := r.hdl.edit(context.Background(), infoB, slackEditArgs{Channel: "C1", TS: sendOut.TS, Body: "edited by team B"}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	artsA := r.artifactsForRun(orgID, runA)
	if len(artsA) != 1 {
		t.Fatalf("want 1 artifact still attributed to the creating run A, got %d: %+v", len(artsA), artsA)
	}
	if artsA[0].RunID != runA || artsA[0].TeamID != teamA {
		t.Errorf("artifact attribution drifted: run=%q team=%q, want run=%q team=%q", artsA[0].RunID, artsA[0].TeamID, runA, teamA)
	}
	if artsB := r.artifactsForRun(orgID, runB); len(artsB) != 0 {
		t.Errorf("editing run B must not have reassigned the artifact to itself, got %+v", artsB)
	}

	actsA := r.actionsForRun(orgID, runA)
	if len(actsA) != 1 || actsA[0].Action != domain.ActionSlackMessagePosted {
		t.Errorf("run A's external_actions = %+v, want exactly one slack_message_posted", actsA)
	}
	actsB := r.actionsForRun(orgID, runB)
	if len(actsB) != 1 || actsB[0].Action != domain.ActionSlackMessageEdited {
		t.Errorf("run B's external_actions = %+v, want exactly one slack_message_edited (the audit trail still credits the editing run)", actsB)
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
			runID := r.seedRun(orgID, teamID, owner, true)
			info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

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
				out, err := r.hdl.edit(context.Background(), info, slackEditArgs{Channel: "C1", TS: replyTS, Body: "edited"})
				if err != nil {
					t.Fatalf("edit: %v", err)
				}
				if out.TS != replyTS {
					t.Errorf("edit TS = %q, want %q (the message's own ts, not the root)", out.TS, replyTS)
				}
				arts := r.artifactsForRun(orgID, runID)
				if len(arts) != 1 || arts[0].Target != wantTarget {
					t.Fatalf("artifacts = %+v, want one artifact targeting %q", arts, wantTarget)
				}
			case "react":
				if _, err := r.hdl.react(context.Background(), info, slackReactArgs{Channel: "C1", TS: replyTS, Emoji: "eyes"}); err != nil {
					t.Fatalf("react: %v", err)
				}
			}
			acts := r.actionsForRun(orgID, runID)
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.repliesMessages = []map[string]any{{"ts": ts, "user": "U1", "text": "an unthreaded message"}}

	if _, err := r.hdl.react(context.Background(), info, slackReactArgs{Channel: "C1", TS: ts, Emoji: "eyes"}); err != nil {
		t.Fatalf("react: %v", err)
	}
	wantTarget := domain.SlackSourceID("C1", ts)
	acts := r.actionsForRun(orgID, runID)
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.repliesMessages = []map[string]any{{"ts": rootTS, "user": "U1", "text": "root message"}}
	r.fake.repliesNextCursor = "dGVhbTpD" // a non-empty cursor: "there's another page"

	if _, err := r.hdl.react(context.Background(), info, slackReactArgs{Channel: "C1", TS: replyTS, Emoji: "eyes"}); err != nil {
		t.Fatalf("react: %v", err)
	}
	if r.fake.repliesCalls != 1 {
		t.Errorf("conversations.replies was called %d time(s), want exactly 1 (must not follow next_cursor)", r.fake.repliesCalls)
	}
	wantTarget := domain.SlackSourceID("C1", rootTS)
	acts := r.actionsForRun(orgID, runID)
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.attachCompleteFails = true

	out, err := r.hdl.send(context.Background(), info, slackSendArgs{
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

	arts := r.artifactsForRun(orgID, runID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact for the message that DID post, got %d: %+v", len(arts), arts)
	}
	if arts[0].ExternalID != out.TS || arts[0].State != domain.ArtifactStateMessagePosted {
		t.Errorf("artifact mismatch: %+v", arts[0])
	}
	acts := r.actionsForRun(orgID, runID)
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	const parentTS = "1700000000.000000"
	out, err := r.hdl.send(context.Background(), info, slackSendArgs{
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	out, err := r.hdl.send(context.Background(), info, slackSendArgs{
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

// ---------- read thread / read channel ----------

func TestSlackExecHandler_ReadThread_Golden(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-read-thread")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.userNames["U1"] = "Ada Lovelace"
	r.fake.repliesMessages = []map[string]any{
		{"ts": "1700000000.000000", "user": "U1", "text": "root message", "files": []map[string]any{{"id": "F1", "name": "spec.pdf"}}},
		{"ts": "1700000000.000001", "thread_ts": "1700000000.000000", "user": "U1", "text": "a reply"},
	}

	out, err := r.hdl.readThread(context.Background(), info, slackReadThreadArgs{Channel: "C1", TS: "1700000000.000000", Limit: 50})
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.historyMessages = []map[string]any{
		{"ts": "1700000002.000000", "user": "U2", "text": "third"},
		{"ts": "1700000000.000000", "user": "U2", "text": "first"},
		{"ts": "1700000001.000000", "user": "U2", "text": "second"},
	}

	out, err := r.hdl.readChannel(context.Background(), info, slackReadChannelArgs{Channel: "C1", Limit: 20})
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	if _, err := r.hdl.readChannel(context.Background(), info, slackReadChannelArgs{Channel: "C1", Limit: 10}); err == nil {
		t.Fatal("expected an authz refusal, got nil")
	} else if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error = %q, want it to mention the team doesn't track the channel", err)
	}
}

// ---------- download ----------

func TestSlackExecHandler_Download_Golden(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-download")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test")
	r.seedChannel(orgID, "T1", "C1")
	r.trackChannel(orgID, owner, teamID, "C1")
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "report.csv", []byte("a,b,c\n1,2,3\n"), []string{"C1"})

	out, err := r.hdl.download(context.Background(), info, slackDownloadArgs{FileID: "F1"})
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "secret.csv", []byte("data"), []string{"C-untracked"})

	_, err := r.hdl.download(context.Background(), info, slackDownloadArgs{FileID: "F1"})
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
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "dm-only.csv", []byte("data"), nil)

	_, err := r.hdl.download(context.Background(), info, slackDownloadArgs{FileID: "F1"})
	if err == nil {
		t.Fatal("expected a refusal — the file has no channel to authorize against")
	}
	if !strings.Contains(err.Error(), "isn't shared into any channel") {
		t.Errorf("error = %q, want it to explain there's no channel to authorize against", err)
	}
}

// TestSlackExecHandler_Download_FallbackPath_StillAuthorizes is the direct
// regression test for the gap: a run with NO Slack task context (so
// resolveWorkspaceForDownload takes the "org's sole connected workspace"
// fallback, which — before this fix — returned a token with zero channel
// authorization at all) must still be refused when the requested file
// belongs only to a channel this team doesn't track.
func TestSlackExecHandler_Download_FallbackPath_StillAuthorizes(t *testing.T) {
	r := newSlackExecRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-dl-fallback")
	r.seedWorkspace(orgID, owner, "T1", "A1", "xoxb-test") // exactly one connected workspace
	r.seedChannel(orgID, "T1", "C-untracked")
	// teamID tracks nothing — this run's seeded task is a plain GitHub task
	// (seedRun/seedNonSlackTask), so workspaceFromRunTaskMetadata reports
	// "no Slack context" and resolveWorkspaceForDownload falls through to
	// the org-wide sole-workspace path.
	runID := r.seedRun(orgID, teamID, owner, true)
	info := agenthost.RunInfo{OrgID: orgID, UserID: owner, RunID: runID, TeamID: teamID, IsEventTriggered: true}

	r.fake.setFile("F1", "leak.csv", []byte("data"), []string{"C-untracked"})

	_, err := r.hdl.download(context.Background(), info, slackDownloadArgs{FileID: "F1"})
	if err == nil {
		t.Fatal("expected the fallback (no-task-context) path to still enforce channel authorization")
	}
	if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error = %q, want it to mention the team doesn't track the file's channel(s)", err)
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
