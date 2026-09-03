package server

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The rule most likely to be broken by a well-meaning filter: nothing on the
// GitHub settings surface is derived from the viewer's own GitHub access. Two
// admins of one workspace, bound to different GitHub identities — one of them
// to a login that can see none of the repositories — receive byte-identical
// payloads for the status read and both findings.
func TestGitHubGrantFindings_MultiMode_AdminsSeeTheSameBytes(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	org, team := rig.seedOrg(alice, "grant-org-"+uuid.NewString()[:8])
	bob := rig.seedUser()
	pgtest.MustExec(t, rig.h.AdminDB,
		`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`,
		bob.String(), org.String())

	// Two different GitHub identities: alice is an owner of acme, bob is a
	// stranger to it. Neither fact may reach the payload.
	for _, id := range []struct {
		user  uuid.UUID
		login string
	}{{alice, "alice-acme-owner"}, {bob, "bob-outsider"}} {
		pgtest.MustExec(t, rig.h.AdminDB, `
			INSERT INTO user_github_identities (user_id, github_base_url, login, source, verified_at)
			VALUES ($1, 'https://github.com', $2, 'connect_oauth', now())
		`, id.user.String(), id.login)
	}

	seedPGBYOAppCredentialClass(t, rig, org.String())
	ctx := context.Background()
	if _, err := rig.srv.githubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
		InstallationID:      "4242",
		OrgID:               org.String(),
		AccountType:         "Organization",
		AccountLogin:        "acme",
		GitHubHost:          "https://github.com",
		RepositorySelection: domain.RepositorySelectionSelected,
	}); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	if err := rig.srv.reachableRepos.ReplaceForInstallationSystem(ctx, org.String(), domain.GitHubCredentialClassBYOApp, "4242", []domain.ReachableRepository{
		{OrgID: org.String(), InstallationID: "4242", Owner: "acme", Repo: "api", ExternalID: "10"},
		{OrgID: org.String(), InstallationID: "4242", Owner: "acme", Repo: "secrets", ExternalID: "11", Private: true},
	}); err != nil {
		t.Fatalf("replace mirror: %v", err)
	}
	for _, repo := range []string{"api", "legacy"} {
		var repoID string
		if err := rig.h.AdminDB.QueryRow(`
			INSERT INTO repositories (org_id, source, owner, repo) VALUES ($1, 'github', 'acme', $2)
			RETURNING id::text
		`, org.String(), repo).Scan(&repoID); err != nil {
			t.Fatalf("seed repository acme/%s: %v", repo, err)
		}
		pgtest.MustExec(t, rig.h.AdminDB,
			`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`,
			team.String(), repoID, org.String())
	}

	sidA := rig.signIn(alice)
	sidB := rig.signIn(bob)
	base := "/api/orgs/" + org.String() + "/github"
	reads := []struct {
		method, path string
		body         any
	}{
		{"GET", base + "/app", nil},
		{"POST", base + "/grant/reach-without-purpose/list", map[string]any{}},
		{"POST", base + "/grant/scope-drift/list", map[string]any{}},
	}
	for _, read := range reads {
		var bodies []string
		for _, sid := range []string{sidA, sidB} {
			var resp *http.Response
			if read.body == nil {
				resp = rig.requestWithSid(read.method, read.path, sid)
			} else {
				resp = rig.postJSONWithSid(read.method, read.path, sid, read.body)
			}
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("%s %s: read body: %v", read.method, read.path, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: status=%d body=%s", read.method, read.path, resp.StatusCode, raw)
			}
			bodies = append(bodies, string(raw))
		}
		if bodies[0] != bodies[1] {
			t.Errorf("%s %s differs between two admins:\nalice: %s\nbob:   %s", read.method, read.path, bodies[0], bodies[1])
		}
		if bodies[0] == "" {
			t.Errorf("%s %s: empty body", read.method, read.path)
		}
	}
}
