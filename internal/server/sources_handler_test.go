package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
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

// TestOrgSources_LocalOmitsWhatCannotExist: local mode lists every source it can
// have, including ones TF has only declared (wip), and omits a multi-only source
// rather than reporting it off.
func TestOrgSources_LocalOmitsWhatCannotExist(t *testing.T) {
	s := newTestServer(t)
	unconfigureEventSources(t)
	got := readOrgSources(t, s)

	for _, kind := range []string{eventsource.KindGitHub, eventsource.KindJira, eventsource.KindLinear, eventsource.KindSchedule} {
		if _, ok := got[kind]; !ok {
			t.Errorf("source %q missing from the local read: %v", kind, got)
		}
	}
	if state, ok := got["slack"]; ok {
		t.Errorf("slack listed in local mode as %q; a Postgres-only source is omitted, not reported off", state)
	}
	if got[eventsource.KindLinear] != string(eventsource.StateUnconfigured) {
		t.Errorf("linear = %q, want unconfigured", got[eventsource.KindLinear])
	}
	if got[eventsource.KindSchedule] != string(eventsource.StateWIP) {
		t.Errorf("schedule = %q, want wip", got[eventsource.KindSchedule])
	}
}

// TestOrgSources_DeniesExactlyLikeOrgSettings: the sources read takes the org
// settings read's member gate, so a plain member reads it and every denial,
// including the 404 that hides a foreign org, matches the settings read.
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
