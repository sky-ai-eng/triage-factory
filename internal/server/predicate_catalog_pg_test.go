package server

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The catalog check reads the team's tracked projects in a read-only
// transaction under the caller's claims, and asks Jira by each project's Jira
// id, so a plain team member, with no admin role anywhere, must see the org's
// Jira site, the team's rules and the Jira ids of the registry rows they
// reference through RLS, and only their own team's. A policy that hid any of
// them would refuse every status the member writes.
func TestPredicateCatalog_ReadsTrackedProjectsUnderRLS_Postgres(t *testing.T) {
	rig := newRepoScopeRig(t)
	var site string
	if err := rig.s.tx.WithTx(t.Context(), rig.orgID, rig.orgOwner, func(tx db.TxStores) error {
		cur, err := tx.Orgs.GetSettings(t.Context(), rig.orgID)
		if err != nil {
			return err
		}
		cur.JiraBaseURL = "https://jira.example.com"
		stored, err := tx.Orgs.UpdateSettingsVersioned(t.Context(), rig.orgID, cur, cur.Version)
		site = domain.EntityScope("jira", stored)
		return err
	}); err != nil {
		t.Fatalf("set jira site: %v", err)
	}
	register := func(externalID, key string) string {
		t.Helper()
		p, err := rig.s.allStores.JiraProjects.FindOrCreateSystem(t.Context(), rig.orgID, site, externalID, key, key)
		if err != nil {
			t.Fatalf("register %s: %v", key, err)
		}
		return p.ID
	}
	sky, ops := register("10000", "SKY"), register("10001", "OPS")
	for _, tracked := range []struct{ user, team, project string }{
		{rig.orgOwner, rig.teamA, sky},
		{rig.memberB, rig.teamB, ops},
	} {
		if err := rig.s.tx.WithTx(t.Context(), rig.orgID, tracked.user, func(tx db.TxStores) error {
			return tx.JiraStatusRules.ReplaceForTeam(t.Context(), tracked.team, site, []domain.JiraProjectStatusRules{{JiraProjectID: tracked.project}})
		}); err != nil {
			t.Fatalf("track %s: %v", tracked.project, err)
		}
	}

	c := &catalogCheck{ctx: t.Context(), orgID: rig.orgID, teamID: rig.teamB}
	if err := rig.s.tx.WithReadTx(t.Context(), rig.orgID, rig.plainB, c.readTracked); err != nil {
		t.Fatalf("readTracked: %v", err)
	}
	if len(c.jiraRefs) != 1 || c.jiraRefs[0] != "10001" {
		t.Errorf("tracked projects = %v, want teamB's OPS by its Jira id", c.jiraRefs)
	}
}
