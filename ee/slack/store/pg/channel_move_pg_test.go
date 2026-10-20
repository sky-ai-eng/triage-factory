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

func seedTracker(t *testing.T, h *pgtest.Harness, orgID, teamID, row string, primary bool, createdAt time.Time) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id, is_primary, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, orgID, teamID, row, primary, createdAt); err != nil {
		t.Fatalf("seed tracker %s on %s: %v", teamID, row, err)
	}
}

func trackersOf(t *testing.T, h *pgtest.Harness, orgID, row string) map[string]slackstore.TeamChannel {
	t.Helper()
	rows, err := h.AdminDB.Query(`
		SELECT team_id::text, is_primary, created_at FROM team_slack_channels
		WHERE org_id = $1 AND slack_channel_id = $2
	`, orgID, row)
	if err != nil {
		t.Fatalf("list trackers of %s: %v", row, err)
	}
	defer rows.Close()
	out := map[string]slackstore.TeamChannel{}
	for rows.Next() {
		tc := slackstore.TeamChannel{OrgID: orgID, SlackChannelID: row}
		if err := rows.Scan(&tc.TeamID, &tc.IsPrimary, &tc.CreatedAt); err != nil {
			t.Fatalf("scan tracker: %v", err)
		}
		out[tc.TeamID] = tc
	}
	return out
}

func seedThreadEntity(t *testing.T, stores db.Stores, orgID, channelID, threadTS, kind, url string) domain.Entity {
	t.Helper()
	ctx := context.Background()
	thread, err := slackstore.FromStores(stores).Channels.FindOrCreateThreadSystem(ctx, orgID, channelID, threadTS, kind, "thread", url)
	if err != nil {
		t.Fatalf("seed thread %s in %s: %v", threadTS, channelID, err)
	}
	e, err := stores.Entities.GetSystem(ctx, orgID, thread.EntityID)
	if err != nil || e == nil {
		t.Fatalf("read seeded thread %s: %+v, %v", thread.EntityID, e, err)
	}
	return *e
}

func seedMessageArtifact(t *testing.T, stores db.Stores, orgID, teamID, channel, rootTS, ts string) string {
	t.Helper()
	a, err := stores.Artifacts.UpsertSystem(context.Background(), orgID, domain.Artifact{
		TeamID: teamID, Provider: domain.ArtifactProviderSlack, Scope: domain.SlackScope, Kind: domain.ArtifactKindMessage,
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
		TeamID: teamID, Provider: domain.ArtifactProviderSlack, Scope: domain.SlackScope, Action: domain.ActionSlackMessagePosted,
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

// resolvesTo asserts each id resolves to row, which carries want as the id
// the channel has now.
func resolvesTo(t *testing.T, channels slackstore.ChannelRegistryStore, orgID, row, want string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := channels.ResolveSystem(context.Background(), orgID, id)
		if err != nil || got == nil || got.ID != row || got.ChannelID != want {
			t.Errorf("ResolveSystem(%s) = %+v, %v; want row %s carrying %s", id, got, err, row, want)
		}
	}
}

// TestChannelMove_Postgres_MovesTheChannel: references to the channel hold its
// registry row, so a move rewrites the row's channel id and the display
// values that spell it — every thread entity's key and permalink under the old
// id in any state, the Slack artifacts and action pointers that name it — and
// leaves the tracking rows and handler filters as they were. Nothing under a
// channel whose id merely starts with the old one moves. A redelivery of the
// same change finds nothing left to move.
func TestChannelMove_Postgres_MovesTheChannel(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamA := pgtest.SeedOrgWithUser(t, h, "chan-move")
	teamB := pgtest.SeedTeam(t, h, orgID, "chan-move-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row := channelRowNamed(t, channels, orgID, moveOld, "launch-plans")
	neighbourRow := channelRowNamed(t, channels, orgID, moveNeighbour, "neighbour")
	created := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	seedTracker(t, h, orgID, teamA, row, true, created)
	seedTracker(t, h, orgID, teamB, row, false, created.Add(time.Hour))
	seedTracker(t, h, orgID, teamA, neighbourRow, true, created)

	open := seedThreadEntity(t, stores, orgID, moveOld, "1700000000.000100", "thread", movePermalink(moveOld, "1700000000000100"))
	done := seedThreadEntity(t, stores, orgID, moveOld, "1700000000.000200", "message", "")
	if _, err := stores.Entities.CloseSystem(ctx, orgID, done.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}
	neighbour := seedThreadEntity(t, stores, orgID, moveNeighbour, "1700000000.000100", "thread", movePermalink(moveNeighbour, "1700000000000100"))

	artifact := seedMessageArtifact(t, stores, orgID, teamA, moveOld, "1700000000.000100", "1700000000.000300")
	neighbourArtifact := seedMessageArtifact(t, stores, orgID, teamA, moveNeighbour, "1700000000.000100", "1700000000.000300")

	oldLink := moveReplyPermalink(moveOld, "1700000000000300", "1700000000.000100")
	seedSlackAction(t, stores, orgID, teamA, domain.SlackSourceID(moveOld, "1700000000.000100"), oldLink, "action-moved")
	seedSlackAction(t, stores, orgID, teamA, domain.SlackSourceID(moveNeighbour, "1700000000.000100"),
		moveReplyPermalink(moveNeighbour, "1700000000000300", "1700000000.000100"), "action-neighbour")

	predicate := `{"channel_in":["` + row + `"],"mentioned_only":true}`
	handler := seedSlackHandler(t, h, orgID, teamA, userID, "launch", predicate)

	move, err := channels.MoveSystem(ctx, orgID, moveOld, moveNew)
	if err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}
	want := slackstore.ChannelMove{To: moveNew, Entities: 2, Artifacts: 1, Actions: 1}
	if !reflect.DeepEqual(move, want) {
		t.Errorf("MoveSystem = %+v, want %+v", move, want)
	}

	// Registry: the same row, carrying the new id, keeps its name and
	// workspace; the old id resolves to it.
	resolvesTo(t, channels, orgID, row, moveNew, moveOld, moveNew)
	if got, _ := channels.GetSystem(ctx, orgID, row); got.Name != "launch-plans" || got.WorkspaceID != "T0MOVE001" {
		t.Errorf("registry row = %+v, want its name and workspace kept", got)
	}
	resolvesTo(t, channels, orgID, neighbourRow, moveNeighbour, moveNeighbour)

	// Tracking: nothing to move; the gates read the row.
	tracked := trackersOf(t, h, orgID, row)
	if a, ok := tracked[teamA]; !ok || !a.IsPrimary || !a.CreatedAt.Equal(created) {
		t.Errorf("team A = %+v (present %v), want primary with created_at %v", a, ok, created)
	}
	if b, ok := tracked[teamB]; !ok || b.IsPrimary {
		t.Errorf("team B = %+v (present %v), want a non-primary tracker", b, ok)
	}
	teamChannels := slackstore.FromStores(stores).TeamChannels
	if got, err := teamChannels.PrimaryTeamForChannelSystem(ctx, orgID, row); err != nil || got != teamA {
		t.Errorf("PrimaryTeamForChannelSystem = %q, %v; want team A", got, err)
	}

	// Entities: keys and permalinks move; each keeps its channel row.
	for _, tc := range []struct {
		id, key, url, state, row string
	}{
		{open.ID, domain.SlackSourceID(moveNew, "1700000000.000100"), movePermalink(moveNew, "1700000000000100"), "active", row},
		{done.ID, domain.SlackSourceID(moveNew, "1700000000.000200"), "", "closed", row},
		{neighbour.ID, domain.SlackSourceID(moveNeighbour, "1700000000.000100"), movePermalink(moveNeighbour, "1700000000000100"), "active", neighbourRow},
	} {
		got, err := stores.Entities.GetSystem(ctx, orgID, tc.id)
		if err != nil || got == nil {
			t.Fatalf("GetSystem(%s) = %v, %v", tc.id, got, err)
		}
		if got.SourceID != tc.key || got.URL != tc.url || got.State != tc.state {
			t.Errorf("entity %s = (%s, %q, %s), want (%s, %q, %s)", tc.id, got.SourceID, got.URL, got.State, tc.key, tc.url, tc.state)
		}
		if r := entityChannelRow(t, h, tc.id); r != tc.row {
			t.Errorf("entity %s channel row = %s, want %s", tc.id, r, tc.row)
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

	// The handler filter names the row, which did not change.
	if got := readPredicate(t, h, handler); !reflect.DeepEqual(got, map[string]any{"channel_in": []any{row}, "mentioned_only": true}) {
		t.Errorf("handler predicate = %v, want it as stored", got)
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

// channelRowNamed registers channelID under workspace T0MOVE001 with name and
// returns its row.
func channelRowNamed(t *testing.T, channels slackstore.ChannelRegistryStore, orgID, channelID, name string) string {
	t.Helper()
	ch, err := channels.EnsureSystem(context.Background(), orgID, "T0MOVE001", channelID, name)
	if err != nil {
		t.Fatalf("ensure %s: %v", channelID, err)
	}
	return ch.ID
}

// TestChannelMove_Postgres_AbsorbsWhatTheNewIDGotFirst covers deliveries that
// reached TF under the new id before the change did, which registered the new
// id as a channel of its own. The move absorbs that row into the channel's:
// the registry fields merge; its tracking rows join the channel's, the
// channel's primary team staying primary; its thread entity moves onto the
// channel and is closed beside the thread's original, which takes the key;
// the handler filters naming it name the channel instead, one that cannot be
// decoded being reported and left as stored; and an artifact key the new id
// already holds stays put.
func TestChannelMove_Postgres_AbsorbsWhatTheNewIDGotFirst(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamA := pgtest.SeedOrgWithUser(t, h, "chan-merge")
	teamB := pgtest.SeedTeam(t, h, orgID, "chan-merge-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	firstSeen := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	channel, err := channels.UpsertSightingSystem(ctx, orgID, "T0OLDWS01", moveOld, firstSeen)
	if err != nil {
		t.Fatalf("seed old sighting: %v", err)
	}
	if err := channels.SetNameSystem(ctx, orgID, channel.ID, "launch-plans"); err != nil {
		t.Fatalf("name old row: %v", err)
	}
	lastMention := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	absorbed, err := channels.UpsertSightingSystem(ctx, orgID, "T0NEWWS01", moveNew, lastMention)
	if err != nil {
		t.Fatalf("seed new sighting: %v", err)
	}

	created := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	seedTracker(t, h, orgID, teamA, channel.ID, true, created)
	seedTracker(t, h, orgID, teamA, absorbed.ID, false, created.Add(47*time.Hour))
	seedTracker(t, h, orgID, teamB, absorbed.ID, true, created.Add(47*time.Hour))

	original := seedThreadEntity(t, stores, orgID, moveOld, "1700000000.000100", "thread", "")
	newcomer := seedThreadEntity(t, stores, orgID, moveNew, "1700000000.000100", "message", "")

	kept := seedMessageArtifact(t, stores, orgID, teamA, moveOld, "1700000000.000100", "1700000000.000500")
	held := seedMessageArtifact(t, stores, orgID, teamA, moveNew, "1700000000.000100", "1700000000.000500")

	onAbsorbed := seedSlackHandler(t, h, orgID, teamA, userID, "absorbed", `{"channel_in":["`+absorbed.ID+`","C0OTHER01"],"mentioned_only":true}`)
	onBoth := seedSlackHandler(t, h, orgID, teamA, userID, "both", `{"channel_in":["`+channel.ID+`","`+absorbed.ID+`"]}`)
	onChannel := seedSlackHandler(t, h, orgID, teamA, userID, "channel", `{"channel_in":["`+channel.ID+`"]}`)
	unreadable := seedSlackHandler(t, h, orgID, teamA, userID, "unreadable", `{"channel_in":["`+absorbed.ID+`", 7]}`)

	move, err := channels.MoveSystem(ctx, orgID, moveOld, moveNew)
	if err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}
	want := slackstore.ChannelMove{
		To: moveNew, Absorbed: absorbed.ID, Entities: 1, Superseded: []string{newcomer.ID},
		Artifacts: 1, Trackers: 2, Handlers: 2, SkippedHandlers: []string{unreadable},
	}
	if !reflect.DeepEqual(move, want) {
		t.Errorf("MoveSystem = %+v, want %+v", move, want)
	}

	// Registry: one row, the channel's, with its name and first sighting and
	// the absorbed row's workspace and latest mention.
	resolvesTo(t, channels, orgID, channel.ID, moveNew, moveOld, moveNew)
	got, err := channels.GetSystem(ctx, orgID, channel.ID)
	if err != nil || got == nil {
		t.Fatalf("channel row = %+v, %v", got, err)
	}
	if got.Name != "launch-plans" || got.WorkspaceID != "T0NEWWS01" || !got.FirstSeenAt.Equal(firstSeen) ||
		got.LastMentionAt == nil || !got.LastMentionAt.Equal(lastMention) {
		t.Errorf("merged registry row = %+v", got)
	}
	if gone, _ := channels.GetSystem(ctx, orgID, absorbed.ID); gone != nil {
		t.Errorf("absorbed row = %+v, want gone", gone)
	}

	// Tracking: team A keeps one row, primary, with its original created_at;
	// team B's primary on the absorbed row is demoted.
	trackers := trackersOf(t, h, orgID, channel.ID)
	if len(trackers) != 2 {
		t.Fatalf("trackers of the channel = %+v, want teams A and B", trackers)
	}
	if a := trackers[teamA]; !a.IsPrimary || !a.CreatedAt.Equal(created) {
		t.Errorf("team A = %+v, want primary with created_at %v", a, created)
	}
	if trackers[teamB].IsPrimary {
		t.Error("team B still primary; want the channel's primary to stay primary")
	}

	// Entities: the original holds the key; the newcomer is closed beside it,
	// on the channel's row.
	if got, err := stores.Entities.GetBySourceSystem(ctx, orgID, "slack", domain.SlackScope, domain.SlackSourceID(moveNew, "1700000000.000100")); err != nil || got == nil || got.ID != original.ID || got.Kind != "thread" {
		t.Errorf("thread under its new key = %+v err=%v, want the original %s", got, err, original.ID)
	}
	if got, err := stores.Entities.GetSystem(ctx, orgID, newcomer.ID); err != nil || got == nil || got.State != "closed" || got.SourceID != domain.SlackSourceID(moveNew, "1700000000.000100") {
		t.Errorf("newcomer = %+v err=%v, want closed under the new key", got, err)
	}
	if r := entityChannelRow(t, h, newcomer.ID); r != channel.ID {
		t.Errorf("newcomer's channel row = %s, want the channel's %s", r, channel.ID)
	}

	// Handler filters: those naming the absorbed row name the channel; the
	// unreadable one is left as stored.
	if got := readPredicate(t, h, onAbsorbed); !reflect.DeepEqual(got, map[string]any{
		"channel_in": []any{channel.ID, "C0OTHER01"}, "mentioned_only": true,
	}) {
		t.Errorf("handler on the absorbed row = %v", got)
	}
	if got := readPredicate(t, h, onBoth)["channel_in"]; !reflect.DeepEqual(got, []any{channel.ID}) {
		t.Errorf("handler on both rows = %v, want the channel once", got)
	}
	if got := readPredicate(t, h, onChannel)["channel_in"]; !reflect.DeepEqual(got, []any{channel.ID}) {
		t.Errorf("handler on the channel = %v, want it as stored", got)
	}
	if got := readPredicate(t, h, unreadable)["channel_in"]; !reflect.DeepEqual(got, []any{absorbed.ID, float64(7)}) {
		t.Errorf("unreadable handler = %v, want it as stored", got)
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

// TestChannelMove_Postgres_ThreadAfterTheSightingsRowIsAbsorbed is ingest's
// mention path with a move landing between its two writes: a mention under
// the new id registers a row of its own, the move absorbs and deletes that
// row, and the thread created next by the delivered id lands on the
// surviving row.
func TestChannelMove_Postgres_ThreadAfterTheSightingsRowIsAbsorbed(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-absorb-thread")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row := channelRowNamed(t, channels, orgID, moveOld, "")
	sighting, err := channels.UpsertSightingSystem(ctx, orgID, "T0MOVE001", moveNew, time.Now())
	if err != nil {
		t.Fatalf("sighting under the new id: %v", err)
	}
	if sighting.ID == row {
		t.Fatalf("sighting under the new id resolved to the channel's row %s; want a row of its own", row)
	}
	if _, err := channels.MoveSystem(ctx, orgID, moveOld, moveNew); err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}

	thread, err := channels.FindOrCreateThreadSystem(ctx, orgID, moveNew, "1700000000.000100", "thread", "", "")
	if err != nil {
		t.Fatalf("FindOrCreateThreadSystem by the delivered id: %v", err)
	}
	if !thread.Created || thread.Channel.ID != row || thread.Channel.ChannelID != moveNew {
		t.Errorf("thread = %+v; want created on the surviving row %s under %s", thread, row, moveNew)
	}
	if got := entityChannelRow(t, h, thread.EntityID); got != row {
		t.Errorf("slack_channel_id = %q; want the surviving row %s", got, row)
	}
}

// TestChannelMove_Postgres_ChainsAndReversals: every recorded change resolves
// in one read to the channel's row, carrying the id it has now, through a
// second move and through a move back.
func TestChannelMove_Postgres_ChainsAndReversals(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-chain")
	otherOrg, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-chain-other")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row := channelRowNamed(t, channels, orgID, "G0CHAIN01", "")
	otherRow := channelRowNamed(t, channels, otherOrg, "G0CHAIN01", "")

	if _, err := channels.MoveSystem(ctx, orgID, "G0CHAIN01", "C0CHAIN01"); err != nil {
		t.Fatalf("first move: %v", err)
	}
	if _, err := channels.MoveSystem(ctx, orgID, "C0CHAIN01", "C0CHAIN02"); err != nil {
		t.Fatalf("second move: %v", err)
	}
	resolvesTo(t, channels, orgID, row, "C0CHAIN02", "G0CHAIN01", "C0CHAIN01", "C0CHAIN02")

	if _, err := channels.MoveSystem(ctx, orgID, "C0CHAIN02", "C0CHAIN01"); err != nil {
		t.Fatalf("move back: %v", err)
	}
	resolvesTo(t, channels, orgID, row, "C0CHAIN01", "G0CHAIN01", "C0CHAIN01", "C0CHAIN02")

	// A change is the org's own: another org's row answers to the old id.
	resolvesTo(t, channels, otherOrg, otherRow, "G0CHAIN01", "G0CHAIN01")

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
// moves the channel to that id.
func TestChannelMove_Postgres_StaleChangesDoNotRegress(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-stale")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

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

	row := channelRowNamed(t, channels, orgID, "G0STALE01", "")
	move("G0STALE01", "C0STALE01", "C0STALE01")
	move("C0STALE01", "C0STALE02", "C0STALE02")
	move("G0STALE01", "C0STALE01", "C0STALE02")
	resolvesTo(t, channels, orgID, row, "C0STALE02", "G0STALE01", "C0STALE01")

	// Out of order: the second change first. A thread under the first id goes
	// straight to the id the channel has now.
	outOfOrder := channelRowNamed(t, channels, orgID, "G0STALE11", "")
	thread := seedThreadEntity(t, stores, orgID, "G0STALE11", "1700000000.000100", "thread", "")
	move("C0STALE11", "C0STALE12", "C0STALE12")
	move("G0STALE11", "C0STALE11", "C0STALE12")
	resolvesTo(t, channels, orgID, outOfOrder, "C0STALE12", "G0STALE11", "C0STALE11")
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

// TestChannelMove_Postgres_WritesWaitForAMoveInProgress: a write that spells
// a channel id — a thread created, a channel ensured — waits while a move
// holds its lock, then writes under the id the move left: a move cannot
// commit between the write's resolve and its write.
func TestChannelMove_Postgres_WritesWaitForAMoveInProgress(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-move-wait")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row := channelRowNamed(t, channels, orgID, moveOld, "")

	// A move in progress: the lock taken, the change recorded and the row
	// moved, not yet committed.
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
	if _, err := move.Exec(`UPDATE slack_channels SET channel_id = $2 WHERE id = $1`, row, moveNew); err != nil {
		t.Fatalf("move row: %v", err)
	}

	type result struct {
		thread, ensured string
		err             error
	}
	wrote := make(chan result, 1)
	go func() {
		var r result
		var thread slackstore.Thread
		if thread, r.err = channels.FindOrCreateThreadSystem(ctx, orgID, moveOld, "1700000000.000100", "thread", "", ""); r.err == nil {
			r.thread = thread.EntityID
			var ch slackstore.Channel
			ch, r.err = channels.EnsureSystem(ctx, orgID, "", moveOld, "")
			r.ensured = ch.ID
		}
		wrote <- r
	}()
	select {
	case r := <-wrote:
		t.Fatalf("writes returned %+v while the move held its lock; want them to wait", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := move.Commit(); err != nil {
		t.Fatalf("commit move: %v", err)
	}
	select {
	case r := <-wrote:
		if r.err != nil {
			t.Fatalf("writes after the move: %v", r.err)
		}
		e, err := stores.Entities.GetSystem(ctx, orgID, r.thread)
		if err != nil || e == nil || e.SourceID != moveNew+"/1700000000.000100" {
			t.Errorf("thread = %+v, %v; want it keyed under the new id", e, err)
		}
		if r.ensured != row {
			t.Errorf("ensure under the old id = row %s; want the channel's %s", r.ensured, row)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writes still waiting after the move committed")
	}
}
