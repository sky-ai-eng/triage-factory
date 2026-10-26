package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
