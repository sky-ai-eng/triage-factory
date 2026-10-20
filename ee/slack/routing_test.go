package slack

import (
	"context"
	"errors"
	"strings"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
)

// fakeTeamChannels is a minimal slackstore.TeamChannelStore fake: embeds
// the interface (nil) so it satisfies the type structurally, and overrides
// only the two methods the routing hooks call.
type fakeTeamChannels struct {
	slackstore.TeamChannelStore
	primaryTeam string
	primaryErr  error
	tracks      bool
	tracksErr   error
	// sawChannel is the registry row the last call asked about.
	sawChannel string
}

func (f *fakeTeamChannels) PrimaryTeamForChannelSystem(_ context.Context, _, slackChannelID string) (string, error) {
	f.sawChannel = slackChannelID
	return f.primaryTeam, f.primaryErr
}

func (f *fakeTeamChannels) TracksChannelSystem(_ context.Context, _, _, slackChannelID string) (bool, error) {
	f.sawChannel = slackChannelID
	return f.tracks, f.tracksErr
}

// hookBundle is the store bundle the routing hooks read: tracking from tc,
// and a registry holding org-1's channel C1, which Slack moved there from G1.
func hookBundle(tc *fakeTeamChannels) *slackstore.Bundle {
	channels := newFakeChannelRegistry()
	channels.register("org-1", "", "G1")
	if _, err := channels.MoveSystem(context.Background(), "org-1", "G1", "C1"); err != nil {
		panic(err)
	}
	return &slackstore.Bundle{Channels: channels, TeamChannels: tc}
}

func messageEvent(t *testing.T, orgID, channel string) domain.Event {
	t.Helper()
	return domain.Event{
		OrgID:        orgID,
		EventType:    domain.EventSlackMessage,
		MetadataJSON: `{"channel":"` + channel + `"}`,
	}
}

// ---------- slackChannelOwner ----------

func TestSlackChannelOwner_PrimaryTeamOwns(t *testing.T) {
	tc := &fakeTeamChannels{primaryTeam: "team-x"}
	bundle := hookBundle(tc)
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C1"), "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if res.Owner != "team-x" || len(res.OwnerSet) != 1 || res.OwnerSet[0] != "team-x" {
		t.Errorf("res = %+v; want owner=team-x set=[team-x]", res)
	}
	if want := bundle.Channels.(*fakeChannelRegistry).row("org-1", "C1").ID; tc.sawChannel != want {
		t.Errorf("primary-team lookup asked about %q; want the channel's registry row %q", tc.sawChannel, want)
	}
}

// TestSlackChannelOwner_RetiredChannelID: a message recorded before Slack
// moved its channel names the old id, which resolves to the channel's row.
func TestSlackChannelOwner_RetiredChannelID(t *testing.T) {
	tc := &fakeTeamChannels{primaryTeam: "team-x"}
	bundle := hookBundle(tc)
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "G1"), "entity-1")
	if err != nil || res.Owner != "team-x" {
		t.Fatalf("res = %+v, err = %v; want owner team-x", res, err)
	}
	if want := bundle.Channels.(*fakeChannelRegistry).row("org-1", "C1").ID; tc.sawChannel != want {
		t.Errorf("primary-team lookup asked about %q; want the channel's registry row %q", tc.sawChannel, want)
	}
}

// TestSlackChannelOwner_UnregisteredChannel: a channel TF holds no registry
// row for has no tracker, so no owner — a data state, like no primary team.
func TestSlackChannelOwner_UnregisteredChannel(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{primaryTeam: "team-x"})
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C404"), "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	assertResolvedUnowned(t, res, "an unregistered channel")
}

// TestSlackChannelOwner_ChannelLookupErrorPropagates: a failed registry read is
// "could not find out", like a failed primary-team read.
func TestSlackChannelOwner_ChannelLookupErrorPropagates(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{primaryTeam: "team-x"})
	bundle.Channels.(*fakeChannelRegistry).resolveErr = errors.New("boom")
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C1"), "entity-1")
	if err == nil {
		t.Fatal("err = nil; want the registry error propagated")
	}
	if res.Owner != "" || res.Unowned {
		t.Errorf("res = %+v; want the zero resolution alongside the error", res)
	}
}

// The three data states below resolve to routing.Unowned(): a retry cannot
// produce a different answer, so the mention is recorded taskless and the
// event consumed. Each must CLAIM the no-owner answer — an empty resolution
// that didn't is refused at the seam as an unreported failure, which is
// exactly the outcome these three must not get.

func TestSlackChannelOwner_NoPrimary(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{primaryTeam: ""})
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C1"), "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil — a channel with no primary team is a resolved answer, not a failure", err)
	}
	assertResolvedUnowned(t, res, "a channel with no primary team")
}

func TestSlackChannelOwner_MalformedMetadata(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{primaryTeam: "team-x"})
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMessage, MetadataJSON: `not json`}
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil — unreadable metadata is a data state a replay would only repeat", err)
	}
	assertResolvedUnowned(t, res, "unreadable metadata")
}

func TestSlackChannelOwner_EmptyChannelInMetadata(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{primaryTeam: "team-x"})
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMessage, MetadataJSON: `{"channel":""}`}
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil — a mention with no channel is a data state", err)
	}
	assertResolvedUnowned(t, res, "a mention with no channel")
}

// assertResolvedUnowned pins both halves of the no-owner answer: no owner, and
// the resolution SAYS it resolved. The second half is what keeps these three
// cases from landing in the seam's refusal arm alongside a swallowed failure.
func assertResolvedUnowned(t *testing.T, res routing.OwnerResolution, what string) {
	t.Helper()
	if res.Owner != "" || len(res.OwnerSet) != 0 {
		t.Errorf("res = %+v; want no owner for %s", res, what)
	}
	if !res.Unowned {
		t.Errorf("res.Unowned = false for %s; an unclaimed empty resolution is refused as an unreported failure", what)
	}
}

// TestSlackChannelOwner_StoreErrorPropagates is the one that changed: the
// resolver stands in for the whole owner ladder, so a failed primary-team read
// has nowhere else to be caught. Returning the empty owner here reads
// downstream as "nobody owns this channel" — which either loses the mention
// (Slack ingest has no snapshot-diff to re-derive it from) or lets an
// applies_to_unowned watcher answer, and then own, another team's channel.
func TestSlackChannelOwner_StoreErrorPropagates(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{primaryErr: errors.New("boom")})
	res, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C1"), "entity-1")
	if err == nil {
		t.Fatal("err = nil; want the store error propagated — a failed lookup is not a resolved no-owner")
	}
	if !strings.Contains(err.Error(), "C1") {
		t.Errorf("err = %v; want it to name the channel whose lookup failed", err)
	}
	if res.Owner != "" || len(res.OwnerSet) != 0 || res.Unowned {
		t.Errorf("res = %+v; want the zero resolution alongside the error — never a claimed answer", res)
	}
}

// ---------- slackTeamTracksChannel ----------

func TestSlackTeamTracksChannel_Tracks(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{tracks: true})
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C1"), "team-x"); !got {
		t.Error("got false; want true (team tracks the channel)")
	}
}

func TestSlackTeamTracksChannel_DoesNotTrack(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{tracks: false})
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C1"), "team-x"); got {
		t.Error("got true; want false (team does not track the channel)")
	}
}

func TestSlackTeamTracksChannel_StoreErrorFailsOpen(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{tracks: false, tracksErr: errors.New("boom")})
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C1"), "team-x"); !got {
		t.Error("got false; want true (store error fails open)")
	}
}

func TestSlackTeamTracksChannel_UnregisteredChannel(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{tracks: true})
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C404"), "team-x"); got {
		t.Error("got true; want false (no team tracks a channel TF holds no row for)")
	}
}

func TestSlackTeamTracksChannel_ChannelLookupErrorFailsOpen(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{tracks: false})
	bundle.Channels.(*fakeChannelRegistry).resolveErr = errors.New("boom")
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C1"), "team-x"); !got {
		t.Error("got false; want true (a registry error fails open)")
	}
}

func TestSlackTeamTracksChannel_MalformedMetadataFailsOpen(t *testing.T) {
	bundle := hookBundle(&fakeTeamChannels{tracks: false})
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMessage, MetadataJSON: `not json`}
	if got := slackTeamTracksChannel(bundle)(context.Background(), evt, "team-x"); !got {
		t.Error("got false; want true (malformed metadata fails open)")
	}
}

// ---------- the posture split ----------

// TestSlackHooks_StoreErrorPostureSplit pins both halves against ONE outage,
// so the split can't erode by drift in either direction. The two hooks sit in
// the same file, read the same store, and answer a store failure in opposite
// ways — which is only defensible because of what each does with the answer.
// ResolveOwner DECIDES the owner, and a wrong decision is a mention answered
// by the wrong team, in public, permanently. TracksScope only NARROWS a set
// someone else computed, so a briefly-too-wide gate is corrected by the next
// message. Making either one match the other is the bug.
func TestSlackHooks_StoreErrorPostureSplit(t *testing.T) {
	down := &fakeTeamChannels{
		primaryErr: errors.New("boom"),
		tracks:     false, // what a healthy store would have said
		tracksErr:  errors.New("boom"),
	}
	bundle := hookBundle(down)
	evt := messageEvent(t, "org-1", "C1")

	if _, err := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1"); err == nil {
		t.Error("ResolveOwner swallowed the store error; a hook that decides ownership must propagate")
	}
	if !slackTeamTracksChannel(bundle)(context.Background(), evt, "team-x") {
		t.Error("TracksScope dropped the team on a store error; a gate that only narrows must fail open")
	}
}

// ---------- registration ----------

// TestInstallShapedRegistration_RouterBound pins the flip this leaf makes:
// registering slack's SourceHooks the same way install() does marks
// "slack:" router-bound, so slack:message is durably enqueued going
// forward. ResetSources in cleanup keeps this from leaking into other
// tests in the routing package (the TFAC-523-era pattern).
func TestInstallShapedRegistration_RouterBound(t *testing.T) {
	t.Cleanup(routing.ResetSources)

	if routing.RouterBound(domain.EventSlackMessage) {
		t.Fatal("slack:message already router-bound before registration")
	}

	bundle := hookBundle(&fakeTeamChannels{primaryTeam: "team-x"})
	routing.RegisterSource("slack", routing.SourceHooks{
		ResolveOwner: slackChannelOwner(bundle),
		TracksScope:  slackTeamTracksChannel(bundle),
	})

	if !routing.RouterBound(domain.EventSlackMessage) {
		t.Error("slack:message not router-bound after an install-shaped registration")
	}
}
