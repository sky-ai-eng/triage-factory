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
}

func (f *fakeTeamChannels) PrimaryTeamForChannelSystem(_ context.Context, _, _ string) (string, error) {
	return f.primaryTeam, f.primaryErr
}

func (f *fakeTeamChannels) TracksChannelSystem(_ context.Context, _, _, _ string) (bool, error) {
	return f.tracks, f.tracksErr
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
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	owner, set, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C1"), "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if owner != "team-x" || len(set) != 1 || set[0] != "team-x" {
		t.Errorf("owner=%q set=%v; want owner=team-x set=[team-x]", owner, set)
	}
}

// The three data states below resolve to ("", nil, nil): a retry cannot
// produce a different answer, so the mention is recorded taskless and the
// event consumed. They are the cases the error arm must NOT swallow along
// with the real failure — the whole point of splitting them.

func TestSlackChannelOwner_NoPrimary(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: ""}}
	owner, set, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C1"), "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil — a channel with no primary team is a resolved answer, not a failure", err)
	}
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want owner=\"\" set=nil (untracked/unclaimed)", owner, set)
	}
}

func TestSlackChannelOwner_MalformedMetadata(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMessage, MetadataJSON: `not json`}
	owner, set, err := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil — unreadable metadata is a data state a replay would only repeat", err)
	}
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want owner=\"\" set=nil on malformed metadata", owner, set)
	}
}

func TestSlackChannelOwner_EmptyChannelInMetadata(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMessage, MetadataJSON: `{"channel":""}`}
	owner, set, err := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1")
	if err != nil {
		t.Fatalf("err = %v; want nil — a mention with no channel is a data state", err)
	}
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want owner=\"\" set=nil on empty channel", owner, set)
	}
}

// TestSlackChannelOwner_StoreErrorPropagates is the one that changed: the
// resolver stands in for the whole owner ladder, so a failed primary-team read
// has nowhere else to be caught. Returning the empty owner here reads
// downstream as "nobody owns this channel" — which either loses the mention
// (Slack ingest has no snapshot-diff to re-derive it from) or lets an
// applies_to_unowned watcher answer, and then own, another team's channel.
func TestSlackChannelOwner_StoreErrorPropagates(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryErr: errors.New("boom")}}
	owner, set, err := slackChannelOwner(bundle)(context.Background(), "org-1", messageEvent(t, "org-1", "C1"), "entity-1")
	if err == nil {
		t.Fatal("err = nil; want the store error propagated — a failed lookup is not a resolved no-owner")
	}
	if !strings.Contains(err.Error(), "C1") {
		t.Errorf("err = %v; want it to name the channel whose lookup failed", err)
	}
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want no partial owner alongside the error", owner, set)
	}
}

// ---------- slackTeamTracksChannel ----------

func TestSlackTeamTracksChannel_Tracks(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: true}}
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C1"), "team-x"); !got {
		t.Error("got false; want true (team tracks the channel)")
	}
}

func TestSlackTeamTracksChannel_DoesNotTrack(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: false}}
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C1"), "team-x"); got {
		t.Error("got true; want false (team does not track the channel)")
	}
}

func TestSlackTeamTracksChannel_StoreErrorFailsOpen(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: false, tracksErr: errors.New("boom")}}
	if got := slackTeamTracksChannel(bundle)(context.Background(), messageEvent(t, "org-1", "C1"), "team-x"); !got {
		t.Error("got false; want true (store error fails open)")
	}
}

func TestSlackTeamTracksChannel_MalformedMetadataFailsOpen(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: false}}
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
	bundle := &slackstore.Bundle{TeamChannels: down}
	evt := messageEvent(t, "org-1", "C1")

	if _, _, err := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1"); err == nil {
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

	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	routing.RegisterSource("slack", routing.SourceHooks{
		ResolveOwner: slackChannelOwner(bundle),
		TracksScope:  slackTeamTracksChannel(bundle),
	})

	if !routing.RouterBound(domain.EventSlackMessage) {
		t.Error("slack:message not router-bound after an install-shaped registration")
	}
}
