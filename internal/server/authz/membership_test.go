package authz

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestLocalMembership_OrgFacts: the org-keyed local facts hold for the local
// org, in any spelling uuid.Parse accepts, and for no other org.
func TestLocalMembership_OrgFacts(t *testing.T) {
	ctx := context.Background()
	m := localMembership{}
	const user = "u1"

	facts := []struct {
		name string
		ask  func(orgID string) (bool, error)
	}{
		{"userHasOrgAccess", func(org string) (bool, error) { return m.userHasOrgAccess(ctx, user, org) }},
		{"userIsOrgAdmin", func(org string) (bool, error) { return m.userIsOrgAdmin(ctx, user, org) }},
		{"userOwnsOrg", func(org string) (bool, error) { return m.userOwnsOrg(ctx, user, org) }},
		{"userAdministersATeam", func(org string) (bool, error) { return m.userAdministersATeam(ctx, org, user) }},
	}
	orgs := []struct {
		org  string
		want bool
	}{
		{runmode.LocalDefaultOrgID, true},
		{strings.ReplaceAll(runmode.LocalDefaultOrgID, "-", ""), true},
		{strings.ToUpper(runmode.LocalDefaultOrgID), true},
		{"00000000-0000-0000-0000-000000000099", false},
		{"3f7c1b2e-9a41-4d5f-8b06-2c1d4e5f6a70", false},
		{"not-an-org", false},
		{"", false},
	}
	for _, f := range facts {
		for _, o := range orgs {
			got, err := f.ask(o.org)
			if err != nil {
				t.Fatalf("%s(%q): %v", f.name, o.org, err)
			}
			if got != o.want {
				t.Errorf("%s(%q) = %v, want %v", f.name, o.org, got, o.want)
			}
		}
	}
}
