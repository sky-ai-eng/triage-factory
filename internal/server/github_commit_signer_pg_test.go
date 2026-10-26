package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCommitSigner_Gates pins every commit-signer route's gate: the two reads
// are any member's, every write is an org admin's, a plain member is refused a
// write with 403 because the signer is in an org they can see, and a caller
// outside the org is told nothing at all.
func TestCommitSigner_Gates(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	founder := r.seedUser()
	orgA, teamA := r.seedOrg(founder, "acme")
	member := r.seedUser()
	pgtest.AddOrgMember(t, r.h, member.String(), orgA.String(), teamA.String(), "member", "member")
	outsider := r.seedUser()
	orgB, _ := r.seedOrg(outsider, "other-co")

	// The signer the routes act on, written on the admin pool as a
	// registration would have left it.
	if _, err := pgstore.New(r.h.AdminDB, r.h.AdminDB, pgtest.SecretKey).OrgCommitSigners.Create(t.Context(), domain.OrgCommitSigner{
		OrgID: orgA.String(), Host: "https://github.com", GitHubUserID: 583231, Login: "octocat",
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedBy: founder.String(),
	}); err != nil {
		t.Fatalf("seed signer: %v", err)
	}

	adminResp, _ := r.driveCallback(founder)
	adminSid := r.sidFromResp(adminResp)
	memberResp, _ := r.driveCallback(member)
	memberSid := r.sidFromResp(memberResp)

	type call struct {
		method, suffix string
		body           any
	}
	calls := []call{
		{http.MethodGet, "/commit-signer", nil},
		{http.MethodGet, "/commit-identity", nil},
		{http.MethodPatch, "/commit-signer", map[string]any{"email": "bot@acme.example"}},
		{http.MethodPost, "/commit-signer/rotate", nil},
		{http.MethodPost, "/commit-signer/verify", nil},
		{http.MethodPost, "/commit-signer", map[string]any{"login": "octocat"}},
		{http.MethodDelete, "/commit-signer", nil},
	}
	cases := []struct {
		name string
		sid  string
		org  string
		want []int // one per call, in order
	}{
		// 404 throughout: disclose nothing about an org the caller cannot see.
		{"outside the org", memberSid, orgB.String(), []int{404, 404, 404, 404, 404, 404, 404}},
		// 403, not 404, on the writes: this org IS in the member's own list.
		{"plain member", memberSid, orgA.String(), []int{200, 200, 403, 403, 403, 403, 403}},
		// Past the gate every write does its work: verify and the second
		// registration stop at the org's state (no GitHub connected, a signer
		// already there), and the removal is last so the others have a signer
		// to act on.
		{"org admin", adminSid, orgA.String(), []int{200, 200, 200, 200, 409, 409, 204}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, c := range calls {
				path := "/api/orgs/" + tc.org + "/github" + c.suffix
				var got int
				if c.method == http.MethodGet {
					got = r.requestWithSid(c.method, path, tc.sid).StatusCode
				} else {
					got = r.postJSONWithSid(c.method, path, tc.sid, c.body).StatusCode
				}
				if got != tc.want[i] {
					t.Errorf("%s %s = %d, want %d", c.method, c.suffix, got, tc.want[i])
				}
			}
		})
	}
}

// TestCommitSigner_HostOrderIndependent_Postgres: the token's disconnect keeps
// the org's GitHub host while a signer exists, and removing the signer
// afterwards clears it, with every read in both requests running under the
// admin's claims.
func TestCommitSigner_HostOrderIndependent_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	founder := r.seedUser()
	org, _ := r.seedOrg(founder, "acme")
	admin := pgstore.New(r.h.AdminDB, r.h.AdminDB, pgtest.SecretKey)
	ctx := t.Context()

	if _, err := admin.Orgs.SetSourceBaseURL(ctx, org.String(), eventsource.KindGitHub, "https://github.com"); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if err := admin.Secrets.Put(ctx, org.String(), integrations.KeyGitHubPAT, "ghp_bound", ""); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if _, err := admin.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
		OrgID: org.String(), Host: "https://github.com", GitHubUserID: 583231, Login: "octocat",
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedBy: founder.String(),
	}); err != nil {
		t.Fatalf("seed signer: %v", err)
	}
	host := func() string {
		t.Helper()
		set, err := admin.Orgs.GetSettingsSystem(ctx, org.String())
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		return set.GitHubBaseURL
	}

	resp, _ := r.driveCallback(founder)
	sid := r.sidFromResp(resp)
	base := "/api/orgs/" + org.String() + "/github"
	if got := r.postJSONWithSid(http.MethodDelete, base+"/pat", sid, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("DELETE pat = %d, want 200", got)
	}
	if got := host(); got != "https://github.com" {
		t.Errorf("host after the token's disconnect = %q, want it kept for the signer", got)
	}
	if got := r.postJSONWithSid(http.MethodDelete, base+"/commit-signer", sid, nil).StatusCode; got != http.StatusNoContent {
		t.Fatalf("DELETE commit-signer = %d, want 204", got)
	}
	if got := host(); got != "" {
		t.Errorf("host after the signer's removal = %q, want it cleared: nothing is connected", got)
	}
}
