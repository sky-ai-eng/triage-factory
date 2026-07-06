package slack

import (
	"context"
	"errors"
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

func mentionEvent(t *testing.T, orgID, channel string) domain.Event {
	t.Helper()
	return domain.Event{
		OrgID:        orgID,
		EventType:    domain.EventSlackMention,
		MetadataJSON: `{"channel":"` + channel + `"}`,
	}
}

// ---------- slackChannelOwner ----------

func TestSlackChannelOwner_PrimaryTeamOwns(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	owner, set := slackChannelOwner(bundle)(context.Background(), "org-1", mentionEvent(t, "org-1", "C1"), "entity-1")
	if owner != "team-x" || len(set) != 1 || set[0] != "team-x" {
		t.Errorf("owner=%q set=%v; want owner=team-x set=[team-x]", owner, set)
	}
}

func TestSlackChannelOwner_NoPrimary(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: ""}}
	owner, set := slackChannelOwner(bundle)(context.Background(), "org-1", mentionEvent(t, "org-1", "C1"), "entity-1")
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want owner=\"\" set=nil (untracked/unclaimed)", owner, set)
	}
}

func TestSlackChannelOwner_MalformedMetadata(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMention, MetadataJSON: `not json`}
	owner, set := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1")
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want owner=\"\" set=nil on malformed metadata", owner, set)
	}
}

func TestSlackChannelOwner_EmptyChannelInMetadata(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMention, MetadataJSON: `{"channel":""}`}
	owner, set := slackChannelOwner(bundle)(context.Background(), "org-1", evt, "entity-1")
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want owner=\"\" set=nil on empty channel", owner, set)
	}
}

func TestSlackChannelOwner_StoreError(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryErr: errors.New("boom")}}
	owner, set := slackChannelOwner(bundle)(context.Background(), "org-1", mentionEvent(t, "org-1", "C1"), "entity-1")
	if owner != "" || set != nil {
		t.Errorf("owner=%q set=%v; want owner=\"\" set=nil on store error", owner, set)
	}
}

// ---------- slackTeamTracksChannel ----------

func TestSlackTeamTracksChannel_Tracks(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: true}}
	if got := slackTeamTracksChannel(bundle)(context.Background(), mentionEvent(t, "org-1", "C1"), "team-x"); !got {
		t.Error("got false; want true (team tracks the channel)")
	}
}

func TestSlackTeamTracksChannel_DoesNotTrack(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: false}}
	if got := slackTeamTracksChannel(bundle)(context.Background(), mentionEvent(t, "org-1", "C1"), "team-x"); got {
		t.Error("got true; want false (team does not track the channel)")
	}
}

func TestSlackTeamTracksChannel_StoreErrorFailsOpen(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: false, tracksErr: errors.New("boom")}}
	if got := slackTeamTracksChannel(bundle)(context.Background(), mentionEvent(t, "org-1", "C1"), "team-x"); !got {
		t.Error("got false; want true (store error fails open)")
	}
}

func TestSlackTeamTracksChannel_MalformedMetadataFailsOpen(t *testing.T) {
	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{tracks: false}}
	evt := domain.Event{OrgID: "org-1", EventType: domain.EventSlackMention, MetadataJSON: `not json`}
	if got := slackTeamTracksChannel(bundle)(context.Background(), evt, "team-x"); !got {
		t.Error("got false; want true (malformed metadata fails open)")
	}
}

// ---------- registration ----------

// TestInstallShapedRegistration_RouterBound pins the flip this leaf makes:
// registering slack's SourceHooks the same way install() does marks
// "slack:" router-bound, so slack:mention is durably enqueued going
// forward. ResetSources in cleanup keeps this from leaking into other
// tests in the routing package (the TFAC-523-era pattern).
func TestInstallShapedRegistration_RouterBound(t *testing.T) {
	t.Cleanup(routing.ResetSources)

	if routing.RouterBound(domain.EventSlackMention) {
		t.Fatal("slack:mention already router-bound before registration")
	}

	bundle := &slackstore.Bundle{TeamChannels: &fakeTeamChannels{primaryTeam: "team-x"}}
	routing.RegisterSource("slack", routing.SourceHooks{
		ResolveOwner: slackChannelOwner(bundle),
		TracksScope:  slackTeamTracksChannel(bundle),
	})

	if !routing.RouterBound(domain.EventSlackMention) {
		t.Error("slack:mention not router-bound after an install-shaped registration")
	}
}
