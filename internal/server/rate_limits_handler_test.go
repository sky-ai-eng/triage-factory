package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func orgRateLimitsPath(orgID string) string {
	return "/api/orgs/" + orgID + "/integrations/rate-limits"
}

// TestOrgRateLimits_ServesCurrentReadingsWithNames: the org's current readings,
// an installation named by its GitHub account, a kind-id credential unnamed, an
// unstated field null rather than zero, and a reading past its reset left out.
func TestOrgRateLimits_ServesCurrentReadingsWithNames(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, false)
	seedInstallation(t, s, 7, "acme-labs")

	now := time.Now().UTC().Truncate(time.Second)
	intp := func(v int) *int { return &v }
	timep := func(v time.Time) *time.Time { return &v }
	near := false
	org := runmode.LocalDefaultOrgID
	if err := s.allStores.RateLimitReadings.UpsertSystem(context.Background(), []domain.RateLimitReading{
		{OrgID: org, Upstream: "github", Credential: "installation:7", Bucket: "core",
			Remaining: intp(3100), Limit: intp(5000), ResetAt: timep(now.Add(time.Hour)), ObservedAt: now},
		{OrgID: org, Upstream: "jira", Credential: "cloud_oauth:org", Bucket: "quota",
			NearLimit: &near, ResetAt: timep(now.Add(time.Hour)), ObservedAt: now},
		{OrgID: org, Upstream: "linear", Credential: "api_key", Bucket: "requests",
			Remaining: intp(0), Limit: intp(1500), ResetAt: timep(now.Add(-time.Second)), ObservedAt: now.Add(-time.Minute)},
	}); err != nil {
		t.Fatalf("seed readings: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, orgRateLimitsPath(org), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET rate-limits: %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Readings []map[string]any `json:"readings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Readings) != 2 {
		t.Fatalf("readings = %d, want 2 (the linear reading is past its reset): %s", len(body.Readings), rec.Body.String())
	}
	gh, jira := body.Readings[0], body.Readings[1]
	if gh["upstream"] != "github" || gh["credential_name"] != "acme-labs" || gh["remaining"] != float64(3100) || gh["limit"] != float64(5000) {
		t.Errorf("github reading = %v, want installation:7 named acme-labs, 3100 of 5000", gh)
	}
	if v, ok := gh["near_limit"]; !ok || v != nil {
		t.Errorf("github near_limit = %v (present %v), want null: the reading states a count instead", v, ok)
	}
	if jira["credential"] != "cloud_oauth:org" || jira["credential_name"] != nil || jira["near_limit"] != false {
		t.Errorf("jira reading = %v, want cloud_oauth:org unnamed with near_limit false", jira)
	}
	if v, ok := jira["remaining"]; !ok || v != nil {
		t.Errorf("jira remaining = %v (present %v), want null, not 0: the quota states no count", v, ok)
	}
}

// TestOrgRateLimits_EmptyIsAnEmptyList: an org with no readings answers 200
// with an empty list, never null, so a client renders "no recent reading"
// without a special case.
func TestOrgRateLimits_EmptyIsAnEmptyList(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, orgRateLimitsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET rate-limits: %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "{\"readings\":[]}\n" && got != `{"readings":[]}` {
		t.Errorf("body = %q, want an empty readings list", got)
	}
}

// TestOrgRateLimits_DeniesExactlyLikeOrgSettings pins the gate: the read is
// RequireOrgMember, the gate GET /api/orgs/{org_id}/settings uses, so a plain
// member reads it and every denial answers as the settings read does,
// including the 404 that keeps a foreign org's existence undisclosed.
func TestOrgRateLimits_DeniesExactlyLikeOrgSettings(t *testing.T) {
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

	for _, tc := range []struct {
		name string
		org  string
		want int
	}{
		{"a plain member of the org", orgA.String(), http.StatusOK},
		{"an org the caller is not in", orgB.String(), http.StatusNotFound},
		{"a malformed org id", "not-a-uuid", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := r.requestWithSid("GET", "/api/orgs/"+tc.org+"/settings", sid).StatusCode
			got := r.requestWithSid("GET", orgRateLimitsPath(tc.org), sid).StatusCode
			if got != settings {
				t.Errorf("rate-limits = %d, settings = %d — the two reads share a gate and owe the same answer", got, settings)
			}
			if got != tc.want {
				t.Errorf("rate-limits = %d, want %d", got, tc.want)
			}
		})
	}
}
