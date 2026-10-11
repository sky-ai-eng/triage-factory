package domain

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestSourceState_Usable(t *testing.T) {
	for _, tt := range []struct {
		state SourceState
		want  bool
	}{
		{SourceSealed, true},
		{SourceUnresolved, true},
		{SourceUnconfigured, false},
		{SourceDisabled, false},
		{SourceUnlicensed, false},
		{SourceWithheld, false},
		{"", false},
	} {
		if got := tt.state.Usable(); got != tt.want {
			t.Errorf("%q.Usable() = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestSourceManifest_UsableKinds(t *testing.T) {
	m := SourceManifest{
		"slack":  {State: SourceUnconfigured},
		"linear": {State: SourceUnresolved, Reason: SourceReasonTokenEndpointUnavailable},
		"jira":   {State: SourceDisabled},
		"github": {State: SourceSealed},
	}
	if got, want := m.UsableKinds(), []string{"github", "linear"}; !slices.Equal(got, want) {
		t.Errorf("UsableKinds = %v, want %v", got, want)
	}
	if got, want := m.Kinds(), []string{"github", "jira", "linear", "slack"}; !slices.Equal(got, want) {
		t.Errorf("Kinds = %v, want %v", got, want)
	}
	if m.Usable("asana") {
		t.Error("Usable(kind the manifest does not name) = true, want false")
	}
}

// TestSourceManifest_At pins the one transformation a holder of the bundle
// applies: a sealed credential past its expiry is unresolved, not sealed.
func TestSourceManifest_At(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	m := SourceManifest{
		"linear": {State: SourceSealed, ExpiresAt: &past},
		"github": {State: SourceSealed, ExpiresAt: &future},
		"jira":   {State: SourceSealed},
		"slack":  {State: SourceUnconfigured},
	}
	got := m.At(now)
	if e := got["linear"]; e.State != SourceUnresolved || e.Reason != SourceReasonTokenExpired || e.ExpiresAt != nil {
		t.Errorf("expired linear = %+v, want unresolved/token_expired", e)
	}
	for _, k := range []string{"github", "jira", "slack"} {
		if got[k].State != m[k].State {
			t.Errorf("%s = %+v, want unchanged %+v", k, got[k], m[k])
		}
	}
	if m["linear"].State != SourceSealed {
		t.Error("At modified its receiver")
	}
	if SourceManifest(nil).At(now) != nil {
		t.Error("nil.At = non-nil, want nil")
	}
}

// TestSourceManifest_WireShape pins the JSON both the claims column and the
// bundle copy carry: omitted reason and expiry stay omitted.
func TestSourceManifest_WireShape(t *testing.T) {
	exp := time.Date(2026, 10, 11, 13, 0, 0, 0, time.UTC)
	m := SourceManifest{
		"github": {State: SourceSealed},
		"linear": {State: SourceSealed, ExpiresAt: &exp},
		"jira":   {State: SourceUnresolved, Reason: SourceReasonResolveFailed},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"github":{"state":"sealed"},"jira":{"state":"unresolved","reason":"resolve_failed"},"linear":{"state":"sealed","expires_at":"2026-10-11T13:00:00Z"}}`
	if string(b) != want {
		t.Errorf("marshal = %s\nwant      %s", b, want)
	}
}
