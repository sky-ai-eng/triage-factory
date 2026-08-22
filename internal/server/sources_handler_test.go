package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func orgSourcesPath() string { return "/api/orgs/" + runmode.LocalDefaultOrgID + "/sources" }

// readOrgSources reads the availability resource as a kind→state map.
func readOrgSources(t *testing.T, s *Server) map[string]string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSourcesPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSourcesPath(), rec.Code, rec.Body.String())
	}
	var body struct {
		Sources []struct {
			Kind  string `json:"kind"`
			State string `json:"state"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode sources: %v", err)
	}
	out := make(map[string]string, len(body.Sources))
	for _, src := range body.Sources {
		if _, dup := out[src.Kind]; dup {
			t.Fatalf("source %q listed twice", src.Kind)
		}
		out[src.Kind] = src.State
	}
	return out
}

// TestOrgSources_ReflectsCredentialState: the read is derived, so binding a
// credential moves a source without anything else being written.
func TestOrgSources_ReflectsCredentialState(t *testing.T) {
	s := newTestServer(t)
	unconfigureEventSources(t)

	before := readOrgSources(t, s)
	if got := before[eventsource.KindGitHub]; got != string(eventsource.StateUnconfigured) {
		t.Errorf("github on a fresh org = %q, want unconfigured", got)
	}
	if got := before[eventsource.KindJira]; got != string(eventsource.StateUnconfigured) {
		t.Errorf("jira on a fresh org = %q, want unconfigured", got)
	}

	configureEventSources(t, s)

	after := readOrgSources(t, s)
	if got := after[eventsource.KindGitHub]; got != string(eventsource.StateAvailable) {
		t.Errorf("github with a bound PAT = %q, want available", got)
	}
	if got := after[eventsource.KindJira]; got != string(eventsource.StateAvailable) {
		t.Errorf("jira with a bound credential = %q, want available", got)
	}
}

// TestOrgSources_LocalOmitsWhatCannotExist pins D6. Local mode reports the
// sources it can have — including the ones TF has only declared, which is a
// fact the UI renders — and simply does not list a source whose store is
// multi-mode-only. A state there would be a claim about setup or licensing in
// a mode where the source cannot run at all.
func TestOrgSources_LocalOmitsWhatCannotExist(t *testing.T) {
	s := newTestServer(t)
	got := readOrgSources(t, s)

	for _, kind := range []string{eventsource.KindGitHub, eventsource.KindJira, eventsource.KindLinear, eventsource.KindSchedule} {
		if _, ok := got[kind]; !ok {
			t.Errorf("source %q missing from the local read: %v", kind, got)
		}
	}
	if state, ok := got["slack"]; ok {
		t.Errorf("slack listed in local mode as %q; a Postgres-only source is omitted, not reported off", state)
	}
	if got[eventsource.KindLinear] != string(eventsource.StateWIP) {
		t.Errorf("linear = %q, want wip", got[eventsource.KindLinear])
	}
	if got[eventsource.KindSchedule] != string(eventsource.StateWIP) {
		t.Errorf("schedule = %q, want wip", got[eventsource.KindSchedule])
	}
}

// TestOrgSources_DeniesExactlyLikeOrgSettings pins D2's gate. The availability
// read is RequireOrgMember — the same gate GET /api/orgs/{org_id}/settings
// uses — so a plain member reads it, and every denial has to answer the way
// the settings read answers, including the 404 that keeps a foreign org's
// existence undisclosed.
func TestOrgSources_DeniesExactlyLikeOrgSettings(t *testing.T) {
	// Multi mode, because that is where the gate has anything to do: local
	// mode is one tenant and RequireOrgMember admits every caller.
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	founder := r.seedUser()
	orgA, teamA := r.seedOrg(founder, "acme")
	member := r.seedUser()
	pgtest.AddOrgMember(t, r.h, member.String(), orgA.String(), teamA.String(), "member", "member")

	outsider := r.seedUser()
	orgB, _ := r.seedOrg(outsider, "other-co")

	resp, _ := r.driveCallback(member)
	sid := r.sidFromResp(resp)

	cases := []struct {
		name string
		org  string
		want int
	}{
		{"a plain member of the org", orgA.String(), http.StatusOK},
		{"an org the caller is not in", orgB.String(), http.StatusNotFound},
		{"a malformed org id", "not-a-uuid", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := r.requestWithSid("GET", "/api/orgs/"+tc.org+"/settings", sid).StatusCode
			sources := r.requestWithSid("GET", "/api/orgs/"+tc.org+"/sources", sid).StatusCode
			if sources != settings {
				t.Errorf("sources = %d, settings = %d — the two reads share a gate and owe the same answer", sources, settings)
			}
			// Named outright as well: parity alone would also hold if both
			// routes were broken in the same direction.
			if sources != tc.want {
				t.Errorf("sources = %d, want %d", sources, tc.want)
			}
		})
	}
}
