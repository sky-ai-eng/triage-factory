package pg_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The channel a move takes away, the one it lands on, and a neighbour whose
// id starts with the old one: "G0MOVE00019/…" starts with "G0MOVE0001", so
// only the "<id>/" boundary keeps it out of the move.
const (
	moveOld       = "G0MOVE0001"
	moveNew       = "C0MOVE0001"
	moveNeighbour = "G0MOVE00019"
)

func movePermalink(channel, ts string) string {
	return "https://acme.slack.com/archives/" + channel + "/p" + ts
}

func moveReplyPermalink(channel, ts, threadTS string) string {
	return movePermalink(channel, ts) + "?thread_ts=" + threadTS + "&cid=" + channel
}

func seedTracker(t *testing.T, h *pgtest.Harness, orgID, teamID, channelID string, primary bool, createdAt time.Time) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO team_slack_channels (org_id, team_id, channel_id, is_primary, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, orgID, teamID, channelID, primary, createdAt); err != nil {
		t.Fatalf("seed tracker %s on %s: %v", teamID, channelID, err)
	}
}

func trackersOf(t *testing.T, h *pgtest.Harness, orgID, channelID string) map[string]slackstore.TeamChannel {
	t.Helper()
	rows, err := h.AdminDB.Query(`
		SELECT team_id::text, is_primary, created_at FROM team_slack_channels
		WHERE org_id = $1 AND channel_id = $2
	`, orgID, channelID)
	if err != nil {
		t.Fatalf("list trackers of %s: %v", channelID, err)
	}
	defer rows.Close()
	out := map[string]slackstore.TeamChannel{}
	for rows.Next() {
		tc := slackstore.TeamChannel{OrgID: orgID, ChannelID: channelID}
		if err := rows.Scan(&tc.TeamID, &tc.IsPrimary, &tc.CreatedAt); err != nil {
			t.Fatalf("scan tracker: %v", err)
		}
		out[tc.TeamID] = tc
	}
	return out
}

func seedThreadEntity(t *testing.T, stores db.Stores, orgID, sourceID, kind, url string) domain.Entity {
	t.Helper()
	e, _, err := stores.Entities.FindOrCreateSystem(context.Background(), orgID, "slack", domain.SlackScope, sourceID, "", kind, "thread", url)
	if err != nil {
		t.Fatalf("seed entity %s: %v", sourceID, err)
	}
	return *e
}

func seedMessageArtifact(t *testing.T, stores db.Stores, orgID, teamID, channel, rootTS, ts string) string {
	t.Helper()
	a, err := stores.Artifacts.UpsertSystem(context.Background(), orgID, domain.Artifact{
		TeamID: teamID, Provider: domain.ArtifactProviderSlack, Kind: domain.ArtifactKindMessage,
		Target: domain.SlackSourceID(channel, rootTS), ExternalID: ts,
		URL:      moveReplyPermalink(channel, ts, rootTS),
		State:    domain.ArtifactStateMessagePosted,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderSlack, domain.ArtifactKindMessage, channel+"/"+ts, ""),
	})
	if err != nil {
		t.Fatalf("seed artifact %s/%s: %v", channel, ts, err)
	}
	return a.ID
}

type artifactRow struct{ target, key, url string }

func readArtifact(t *testing.T, h *pgtest.Harness, id string) artifactRow {
	t.Helper()
	var r artifactRow
	if err := h.AdminDB.QueryRow(`SELECT target, dedup_key, COALESCE(url, '') FROM artifacts WHERE id = $1`, id).
		Scan(&r.target, &r.key, &r.url); err != nil {
		t.Fatalf("read artifact %s: %v", id, err)
	}
	return r
}

func seedSlackAction(t *testing.T, stores db.Stores, orgID, teamID, target, url, dedupKey string) {
	t.Helper()
	if err := stores.ExternalActions.RecordSystem(context.Background(), orgID, domain.ExternalAction{
		TeamID: teamID, Provider: domain.ArtifactProviderSlack, Action: domain.ActionSlackMessagePosted,
		Target: target, URL: url, Credential: domain.CredentialSlackBot, DedupKey: dedupKey,
	}); err != nil {
		t.Fatalf("seed action %s: %v", dedupKey, err)
	}
}

type actionRow struct{ target, url, currentURL string }

func readAction(t *testing.T, h *pgtest.Harness, orgID, dedupKey string) actionRow {
	t.Helper()
	var r actionRow
	if err := h.AdminDB.QueryRow(`
		SELECT target, COALESCE(url, ''), COALESCE(current_url, '') FROM external_actions
		WHERE org_id = $1 AND dedup_key = $2
	`, orgID, dedupKey).Scan(&r.target, &r.url, &r.currentURL); err != nil {
		t.Fatalf("read action %s: %v", dedupKey, err)
	}
	return r
}

func seedSlackHandler(t *testing.T, h *pgtest.Harness, orgID, teamID, userID, name, predicate string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, event_type, scope_predicate_json,
			source, name, default_priority, sort_order)
		VALUES ($1, $2, $3, 'rule', $4, $5::jsonb, 'user', $6, 0.5, 0)
		RETURNING id
	`, orgID, userID, teamID, domain.EventSlackMessage, predicate, name).Scan(&id); err != nil {
		t.Fatalf("seed handler %s: %v", name, err)
	}
	return id
}

func readPredicate(t *testing.T, h *pgtest.Harness, id string) map[string]any {
	t.Helper()
	var raw string
	if err := h.AdminDB.QueryRow(`SELECT scope_predicate_json::text FROM event_handlers WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read handler %s: %v", id, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode handler %s predicate: %v", id, err)
	}
	return out
}

// TestChannelMove_Postgres_MovesEveryRowNamingTheChannel: one move rewrites
// the registry row, the tracking rows, every thread entity under the old id
// in any state, the Slack artifacts and action pointers that name it, and the
// handler filters that list it — and nothing under a channel whose id merely
// starts with the old one. A redelivery of the same change finds nothing
// left to move.
func TestChannelMove_Postgres_MovesEveryRowNamingTheChannel(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamA := pgtest.SeedOrgWithUser(t, h, "chan-move")
	teamB := pgtest.SeedTeam(t, h, orgID, "chan-move-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	if err := channels.EnsureSystem(ctx, orgID, "T0MOVE001", moveOld, "launch-plans"); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := channels.EnsureSystem(ctx, orgID, "T0MOVE001", moveNeighbour, "neighbour"); err != nil {
		t.Fatalf("seed neighbour registry: %v", err)
	}
	created := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	seedTracker(t, h, orgID, teamA, moveOld, true, created)
	seedTracker(t, h, orgID, teamB, moveOld, false, created.Add(time.Hour))
	seedTracker(t, h, orgID, teamA, moveNeighbour, true, created)

	open := seedThreadEntity(t, stores, orgID, domain.SlackSourceID(moveOld, "1700000000.000100"), "thread", movePermalink(moveOld, "1700000000000100"))
	done := seedThreadEntity(t, stores, orgID, domain.SlackSourceID(moveOld, "1700000000.000200"), "message", "")
	if _, err := stores.Entities.CloseSystem(ctx, orgID, done.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}
	neighbour := seedThreadEntity(t, stores, orgID, domain.SlackSourceID(moveNeighbour, "1700000000.000100"), "thread", movePermalink(moveNeighbour, "1700000000000100"))

	artifact := seedMessageArtifact(t, stores, orgID, teamA, moveOld, "1700000000.000100", "1700000000.000300")
	neighbourArtifact := seedMessageArtifact(t, stores, orgID, teamA, moveNeighbour, "1700000000.000100", "1700000000.000300")

	oldLink := moveReplyPermalink(moveOld, "1700000000000300", "1700000000.000100")
	seedSlackAction(t, stores, orgID, teamA, domain.SlackSourceID(moveOld, "1700000000.000100"), oldLink, "action-moved")
	seedSlackAction(t, stores, orgID, teamA, domain.SlackSourceID(moveNeighbour, "1700000000.000100"),
		moveReplyPermalink(moveNeighbour, "1700000000000300", "1700000000.000100"), "action-neighbour")

	handler := seedSlackHandler(t, h, orgID, teamA, userID, "launch", `{"channel_in":["`+moveOld+`","C0OTHER01"],"mentioned_only":true}`)
	neighbourHandler := seedSlackHandler(t, h, orgID, teamA, userID, "neighbour", `{"channel_in":["`+moveNeighbour+`"]}`)

	move, err := channels.MoveSystem(ctx, orgID, moveOld, moveNew)
	if err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}
	want := slackstore.ChannelMove{To: moveNew, Entities: 2, Trackers: 2, Artifacts: 1, Actions: 1, Handlers: 1}
	if !reflect.DeepEqual(move, want) {
		t.Errorf("MoveSystem = %+v, want %+v", move, want)
	}

	// Registry.
	if got, err := channels.GetSystem(ctx, orgID, moveOld); err != nil || got != nil {
		t.Errorf("registry row under the old id = %+v err=%v, want gone", got, err)
	}
	if got, err := channels.GetSystem(ctx, orgID, moveNew); err != nil || got == nil || got.Name != "launch-plans" || got.WorkspaceID != "T0MOVE001" {
		t.Errorf("registry row under the new id = %+v err=%v, want the old row's name and workspace", got, err)
	}

	// Tracking.
	if old := trackersOf(t, h, orgID, moveOld); len(old) != 0 {
		t.Errorf("trackers left on the old id: %+v", old)
	}
	moved := trackersOf(t, h, orgID, moveNew)
	if a, ok := moved[teamA]; !ok || !a.IsPrimary || !a.CreatedAt.Equal(created) {
		t.Errorf("team A on the new id = %+v (present %v), want primary with created_at %v", a, ok, created)
	}
	if b, ok := moved[teamB]; !ok || b.IsPrimary {
		t.Errorf("team B on the new id = %+v (present %v), want a non-primary tracker", b, ok)
	}
	if n := trackersOf(t, h, orgID, moveNeighbour); len(n) != 1 || !n[teamA].IsPrimary {
		t.Errorf("neighbour trackers = %+v, want untouched", n)
	}

	// Entities.
	for _, tc := range []struct {
		id, key, url, state string
	}{
		{open.ID, domain.SlackSourceID(moveNew, "1700000000.000100"), movePermalink(moveNew, "1700000000000100"), "active"},
		{done.ID, domain.SlackSourceID(moveNew, "1700000000.000200"), "", "closed"},
		{neighbour.ID, domain.SlackSourceID(moveNeighbour, "1700000000.000100"), movePermalink(moveNeighbour, "1700000000000100"), "active"},
	} {
		got, err := stores.Entities.GetSystem(ctx, orgID, tc.id)
		if err != nil || got == nil {
			t.Fatalf("GetSystem(%s) = %v, %v", tc.id, got, err)
		}
		if got.SourceID != tc.key || got.URL != tc.url || got.State != tc.state {
			t.Errorf("entity %s = (%s, %q, %s), want (%s, %q, %s)", tc.id, got.SourceID, got.URL, got.State, tc.key, tc.url, tc.state)
		}
	}
	if got, err := stores.Entities.GetBySourceSystem(ctx, orgID, "slack", domain.SlackScope, domain.SlackSourceID(moveNew, "1700000000.000100")); err != nil || got == nil || got.ID != open.ID {
		t.Errorf("thread under its new key = %+v err=%v, want %s", got, err, open.ID)
	}

	// Artifacts.
	if got := readArtifact(t, h, artifact); got != (artifactRow{
		target: domain.SlackSourceID(moveNew, "1700000000.000100"),
		key:    domain.ArtifactDedupKey(domain.ArtifactProviderSlack, domain.ArtifactKindMessage, moveNew+"/1700000000.000300", ""),
		url:    moveReplyPermalink(moveNew, "1700000000.000300", "1700000000.000100"),
	}) {
		t.Errorf("moved artifact = %+v", got)
	}
	if got := readArtifact(t, h, neighbourArtifact); got.target != domain.SlackSourceID(moveNeighbour, "1700000000.000100") ||
		got.key != domain.ArtifactDedupKey(domain.ArtifactProviderSlack, domain.ArtifactKindMessage, moveNeighbour+"/1700000000.000300", "") {
		t.Errorf("neighbour artifact = %+v, want untouched", got)
	}

	// Action pointers: the record of the act keeps its target and link.
	if got := readAction(t, h, orgID, "action-moved"); got != (actionRow{
		target:     domain.SlackSourceID(moveOld, "1700000000.000100"),
		url:        oldLink,
		currentURL: moveReplyPermalink(moveNew, "1700000000000300", "1700000000.000100"),
	}) {
		t.Errorf("moved action = %+v", got)
	}
	if got := readAction(t, h, orgID, "action-neighbour"); got.currentURL != "" {
		t.Errorf("neighbour action current_url = %q, want untouched", got.currentURL)
	}

	// Handler filters.
	if got := readPredicate(t, h, handler); !reflect.DeepEqual(got, map[string]any{
		"channel_in": []any{moveNew, "C0OTHER01"}, "mentioned_only": true,
	}) {
		t.Errorf("moved handler predicate = %v", got)
	}
	if got := readPredicate(t, h, neighbourHandler); !reflect.DeepEqual(got, map[string]any{"channel_in": []any{moveNeighbour}}) {
		t.Errorf("neighbour handler predicate = %v, want untouched", got)
	}

	// The change is recorded, and the gates read the old id as the new one.
	if got, err := channels.CurrentIDSystem(ctx, orgID, moveOld); err != nil || got != moveNew {
		t.Errorf("CurrentIDSystem(old) = %q, %v; want %q", got, err, moveNew)
	}
	if got, err := channels.CurrentIDSystem(ctx, orgID, moveNeighbour); err != nil || got != moveNeighbour {
		t.Errorf("CurrentIDSystem(neighbour) = %q, %v; want itself", got, err)
	}
	teamChannels := slackstore.FromStores(stores).TeamChannels
	if ok, err := teamChannels.TracksChannelSystem(ctx, orgID, teamB, moveOld); err != nil || !ok {
		t.Errorf("TracksChannelSystem(team B, old id) = %v, %v; want true", ok, err)
	}
	if got, err := teamChannels.PrimaryTeamForChannelSystem(ctx, orgID, moveOld); err != nil || got != teamA {
		t.Errorf("PrimaryTeamForChannelSystem(old id) = %q, %v; want team A", got, err)
	}

	// A redelivery has nothing left to move.
	again, err := channels.MoveSystem(ctx, orgID, moveOld, moveNew)
	if err != nil {
		t.Fatalf("MoveSystem (redelivery): %v", err)
	}
	if !reflect.DeepEqual(again, slackstore.ChannelMove{To: moveNew}) {
		t.Errorf("MoveSystem (redelivery) = %+v, want nothing moved", again)
	}
}

// TestChannelMove_Postgres_MergesWhatTheNewIDGotFirst covers deliveries that
// reached TF under the new id before the change did. The thread's original
// takes the key and the entity a mention minted under the new id is closed
// beside it; the registry and tracking rows merge, the old id's primary team
// staying primary; an artifact key the new id already holds stays put.
func TestChannelMove_Postgres_MergesWhatTheNewIDGotFirst(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamA := pgtest.SeedOrgWithUser(t, h, "chan-merge")
	teamB := pgtest.SeedTeam(t, h, orgID, "chan-merge-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	firstSeen := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	if _, err := channels.UpsertSightingSystem(ctx, orgID, "T0OLDWS01", moveOld, firstSeen); err != nil {
		t.Fatalf("seed old sighting: %v", err)
	}
	if err := channels.SetNameSystem(ctx, orgID, moveOld, "launch-plans"); err != nil {
		t.Fatalf("name old row: %v", err)
	}
	lastMention := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	if _, err := channels.UpsertSightingSystem(ctx, orgID, "T0NEWWS01", moveNew, lastMention); err != nil {
		t.Fatalf("seed new sighting: %v", err)
	}

	created := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	seedTracker(t, h, orgID, teamA, moveOld, true, created)
	seedTracker(t, h, orgID, teamA, moveNew, false, created.Add(47*time.Hour))
	seedTracker(t, h, orgID, teamB, moveNew, true, created.Add(47*time.Hour))

	original := seedThreadEntity(t, stores, orgID, domain.SlackSourceID(moveOld, "1700000000.000100"), "thread", "")
	newcomer := seedThreadEntity(t, stores, orgID, domain.SlackSourceID(moveNew, "1700000000.000100"), "message", "")

	kept := seedMessageArtifact(t, stores, orgID, teamA, moveOld, "1700000000.000100", "1700000000.000500")
	held := seedMessageArtifact(t, stores, orgID, teamA, moveNew, "1700000000.000100", "1700000000.000500")

	move, err := channels.MoveSystem(ctx, orgID, moveOld, moveNew)
	if err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}
	if move.Entities != 1 || !reflect.DeepEqual(move.Superseded, []string{newcomer.ID}) {
		t.Errorf("MoveSystem = %+v, want one entity moved and %s superseded", move, newcomer.ID)
	}

	// Registry: one row, the old row's name and first sighting, the new
	// row's workspace and latest mention.
	got, err := channels.GetSystem(ctx, orgID, moveNew)
	if err != nil || got == nil {
		t.Fatalf("registry row under the new id = %+v, %v", got, err)
	}
	if got.Name != "launch-plans" || got.WorkspaceID != "T0NEWWS01" || !got.FirstSeenAt.Equal(firstSeen) ||
		got.LastMentionAt == nil || !got.LastMentionAt.Equal(lastMention) {
		t.Errorf("merged registry row = %+v", got)
	}
	if old, _ := channels.GetSystem(ctx, orgID, moveOld); old != nil {
		t.Errorf("registry row under the old id = %+v, want gone", old)
	}

	// Tracking: team A keeps one row, primary, with its original created_at;
	// team B's primary on the new id is demoted.
	trackers := trackersOf(t, h, orgID, moveNew)
	if len(trackers) != 2 {
		t.Fatalf("trackers on the new id = %+v, want teams A and B", trackers)
	}
	if a := trackers[teamA]; !a.IsPrimary || !a.CreatedAt.Equal(created) {
		t.Errorf("team A = %+v, want primary with created_at %v", a, created)
	}
	if trackers[teamB].IsPrimary {
		t.Error("team B still primary; want the old id's primary to stay primary")
	}

	// Entities: the original holds the key; the newcomer is closed beside it.
	if got, err := stores.Entities.GetBySourceSystem(ctx, orgID, "slack", domain.SlackScope, domain.SlackSourceID(moveNew, "1700000000.000100")); err != nil || got == nil || got.ID != original.ID || got.Kind != "thread" {
		t.Errorf("thread under its new key = %+v err=%v, want the original %s", got, err, original.ID)
	}
	if got, err := stores.Entities.GetSystem(ctx, orgID, newcomer.ID); err != nil || got == nil || got.State != "closed" || got.SourceID != domain.SlackSourceID(moveNew, "1700000000.000100") {
		t.Errorf("newcomer = %+v err=%v, want closed under the new key", got, err)
	}

	// Artifacts: the key the new id already holds is not taken twice.
	if got := readArtifact(t, h, kept); got.key != domain.ArtifactDedupKey(domain.ArtifactProviderSlack, domain.ArtifactKindMessage, moveOld+"/1700000000.000500", "") ||
		got.target != domain.SlackSourceID(moveNew, "1700000000.000100") {
		t.Errorf("artifact whose key the new id holds = %+v, want its key kept and its target moved", got)
	}
	if got := readArtifact(t, h, held); got.key != domain.ArtifactDedupKey(domain.ArtifactProviderSlack, domain.ArtifactKindMessage, moveNew+"/1700000000.000500", "") {
		t.Errorf("artifact under the new id = %+v, want untouched", got)
	}
}

// TestChannelMove_Postgres_ChainsAndReversals: every recorded change resolves
// in one read to the id the channel has now, through a second move and
// through a move back.
func TestChannelMove_Postgres_ChainsAndReversals(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-chain")
	otherOrg, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-chain-other")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	resolves := func(step string, want map[string]string) {
		t.Helper()
		for from, to := range want {
			if got, err := channels.CurrentIDSystem(ctx, orgID, from); err != nil || got != to {
				t.Errorf("%s: CurrentIDSystem(%s) = %q, %v; want %q", step, from, got, err, to)
			}
		}
	}

	if _, err := channels.MoveSystem(ctx, orgID, "G0CHAIN01", "C0CHAIN01"); err != nil {
		t.Fatalf("first move: %v", err)
	}
	if _, err := channels.MoveSystem(ctx, orgID, "C0CHAIN01", "C0CHAIN02"); err != nil {
		t.Fatalf("second move: %v", err)
	}
	resolves("after a second move", map[string]string{"G0CHAIN01": "C0CHAIN02", "C0CHAIN01": "C0CHAIN02", "C0CHAIN02": "C0CHAIN02"})

	if _, err := channels.MoveSystem(ctx, orgID, "C0CHAIN02", "C0CHAIN01"); err != nil {
		t.Fatalf("move back: %v", err)
	}
	resolves("after a move back", map[string]string{"G0CHAIN01": "C0CHAIN01", "C0CHAIN01": "C0CHAIN01", "C0CHAIN02": "C0CHAIN01"})

	// A change is the org's own: another org's rows answer to the old id.
	if got, err := channels.CurrentIDSystem(ctx, otherOrg, "G0CHAIN01"); err != nil || got != "G0CHAIN01" {
		t.Errorf("CurrentIDSystem in another org = %q, %v; want the id itself", got, err)
	}

	if _, err := channels.MoveSystem(ctx, orgID, "", "C0CHAIN01"); err == nil {
		t.Error("MoveSystem with an empty old id succeeded; want an error")
	}
	if move, err := channels.MoveSystem(ctx, orgID, "C0CHAIN01", "C0CHAIN01"); err != nil || !reflect.DeepEqual(move, slackstore.ChannelMove{To: "C0CHAIN01"}) {
		t.Errorf("MoveSystem onto the same id = %+v, %v; want a no-op", move, err)
	}

	var n int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM slack_channel_id_changes WHERE org_id = $1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if n != 2 {
		t.Errorf("recorded changes = %d, want 2 (G0CHAIN01 and C0CHAIN02, both to C0CHAIN01)", n)
	}
}

// TestChannelMove_Postgres_StaleChangesDoNotRegress: a change that arrives
// after a later one — a redelivery, or the two processed out of order —
// leaves every recorded change pointing at the id the channel has now, and
// moves what it finds to that id.
func TestChannelMove_Postgres_StaleChangesDoNotRegress(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-stale")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	resolves := func(step string, want map[string]string) {
		t.Helper()
		for from, to := range want {
			if got, err := channels.CurrentIDSystem(ctx, orgID, from); err != nil || got != to {
				t.Errorf("%s: CurrentIDSystem(%s) = %q, %v; want %q", step, from, got, err, to)
			}
		}
	}
	move := func(oldID, newID, wantTo string) {
		t.Helper()
		got, err := channels.MoveSystem(ctx, orgID, oldID, newID)
		if err != nil {
			t.Fatalf("move %s -> %s: %v", oldID, newID, err)
		}
		if got.To != wantTo {
			t.Errorf("move %s -> %s: To = %q, want %q", oldID, newID, got.To, wantTo)
		}
	}

	move("G0STALE01", "C0STALE01", "C0STALE01")
	move("C0STALE01", "C0STALE02", "C0STALE02")
	move("G0STALE01", "C0STALE01", "C0STALE02")
	resolves("after a redelivered first change", map[string]string{"G0STALE01": "C0STALE02", "C0STALE01": "C0STALE02"})

	// Out of order: the second change first. A thread minted under the first
	// id goes straight to the id the channel has now.
	thread := seedThreadEntity(t, stores, orgID, "G0STALE11/1700000000.000100", "thread", "")
	move("C0STALE11", "C0STALE12", "C0STALE12")
	move("G0STALE11", "C0STALE11", "C0STALE12")
	resolves("after changes processed out of order", map[string]string{"G0STALE11": "C0STALE12", "C0STALE11": "C0STALE12"})
	if got, err := stores.Entities.GetBySourceSystem(ctx, orgID, "slack", domain.SlackScope, "C0STALE12/1700000000.000100"); err != nil || got == nil || got.ID != thread.ID {
		t.Errorf("thread under the current id = %+v, %v; want entity %s", got, err, thread.ID)
	}

	var n int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM slack_channel_id_changes WHERE org_id = $1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if n != 4 {
		t.Errorf("recorded changes = %d, want 4", n)
	}
}

// TestChannelMove_Postgres_SettleMovesWhatLandedUnderARetiredID: rows written
// under an id after its move committed — a writer that resolved the id just
// before — move on when the writer settles, and settling a current id is a
// no-op.
func TestChannelMove_Postgres_SettleMovesWhatLandedUnderARetiredID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "chan-settle")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	if _, err := channels.MoveSystem(ctx, orgID, moveOld, moveNew); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := channels.UpsertSightingSystem(ctx, orgID, "T0SETTLE1", moveOld, time.Now()); err != nil {
		t.Fatalf("late sighting: %v", err)
	}
	thread := seedThreadEntity(t, stores, orgID, moveOld+"/1700000000.000100", "thread", "")
	seedTracker(t, h, orgID, teamID, moveOld, true, time.Now())

	if got, err := channels.SettleSystem(ctx, orgID, moveNew); err != nil || got != moveNew {
		t.Fatalf("SettleSystem(current id) = %q, %v; want %q", got, err, moveNew)
	}
	if got, err := stores.Entities.GetBySourceSystem(ctx, orgID, "slack", domain.SlackScope, moveOld+"/1700000000.000100"); err != nil || got == nil {
		t.Fatalf("settling the current id moved the late thread: %+v, %v", got, err)
	}

	if got, err := channels.SettleSystem(ctx, orgID, moveOld); err != nil || got != moveNew {
		t.Fatalf("SettleSystem(retired id) = %q, %v; want %q", got, err, moveNew)
	}
	if got, err := stores.Entities.GetBySourceSystem(ctx, orgID, "slack", domain.SlackScope, moveNew+"/1700000000.000100"); err != nil || got == nil || got.ID != thread.ID {
		t.Errorf("late thread under the current id = %+v, %v; want entity %s", got, err, thread.ID)
	}
	if row, err := channels.GetSystem(ctx, orgID, moveOld); err != nil || row != nil {
		t.Errorf("registry row under the retired id = %+v, %v; want none", row, err)
	}
	if row, err := channels.GetSystem(ctx, orgID, moveNew); err != nil || row == nil {
		t.Errorf("registry row under the current id = %+v, %v; want one", row, err)
	}
	if got := trackersOf(t, h, orgID, moveNew); !got[teamID].IsPrimary {
		t.Errorf("trackers of the current id = %+v; want team %s primary", got, teamID)
	}
}

// TestChannelMove_Postgres_SettleWaitsForAMoveInProgress: a settle that starts
// while a move holds its lock waits for the commit and reads the change, so a
// write made before the settle cannot fall between the move's reads and its
// record.
func TestChannelMove_Postgres_SettleWaitsForAMoveInProgress(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-settle-wait")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	move, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin move: %v", err)
	}
	defer func() { _ = move.Rollback() }()
	if _, err := move.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1, $2))`, orgID, int64(0x43484944)); err != nil {
		t.Fatalf("take move lock: %v", err)
	}
	if _, err := move.Exec(`
		INSERT INTO slack_channel_id_changes (org_id, old_channel_id, new_channel_id) VALUES ($1, $2, $3)
	`, orgID, moveOld, moveNew); err != nil {
		t.Fatalf("record change: %v", err)
	}

	type result struct {
		id  string
		err error
	}
	settled := make(chan result, 1)
	go func() {
		id, err := channels.SettleSystem(ctx, orgID, moveOld)
		settled <- result{id, err}
	}()
	select {
	case r := <-settled:
		t.Fatalf("SettleSystem returned %q, %v while the move held its lock; want it to wait", r.id, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := move.Commit(); err != nil {
		t.Fatalf("commit move: %v", err)
	}
	select {
	case r := <-settled:
		if r.err != nil || r.id != moveNew {
			t.Errorf("SettleSystem = %q, %v; want %q", r.id, r.err, moveNew)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SettleSystem still waiting after the move committed")
	}
}
