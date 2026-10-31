package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration that gives every ledger row its object identity, and the
// version just before it, at which the test seeds the rows a local install
// carries.
const (
	externalActionObjectsVersion = 202610310002
	externalActionObjectsPrior   = 202610310001
)

// TestMigrate_ExternalActionObjectsBackfill pins the backfill: a row names its
// object where the row, or a deliverable the same conversation recorded for the
// same object, already holds the identity, and every other row stays NULL.
func TestMigrate_ExternalActionObjectsBackfill(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, externalActionObjectsPrior)
	const (
		orgID     = runmode.LocalDefaultOrgID
		teamID    = runmode.LocalDefaultTeamID
		userID    = runmode.LocalDefaultUserID
		githubURL = "https://github.com"
		jiraSite  = "https://acme.atlassian.net"
		linearWS  = "ws-1"
	)
	mustExec(t, database, `INSERT INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, orgID)
	mustExec(t, database, `INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'default', 'Default')`, teamID, orgID)
	mustExec(t, database, `INSERT INTO users (id, display_name) VALUES (?, 'Local')`, userID)
	for _, c := range []string{"c-1", "c-2"} {
		mustExec(t, database, `INSERT INTO conversations (id, org_id, team_id, origin, status) VALUES (?, ?, ?, 'interactive', 'completed')`, c, orgID, teamID)
	}
	repoID := dbseed.GitHubRepository(t, database, orgID, githubURL, "octo", "api")
	entity := createEntityForTest(t, database, "github", "octo/api#7", "pr", "A PR", "")

	jiraIssueKey := domain.ObjectKey(domain.ProviderJira, domain.DeliverableKindIssue, domain.JiraIssueResource(jiraSite, "10042"), "")
	linearCommentKey := domain.ObjectKey(domain.ProviderLinear, domain.DeliverableKindComment, "uuid-1", "cmt-1")
	replyKey := domain.GitHubCommentKey(domain.GitHubReviewComment, 777)
	deliverable := func(id, conversation, provider, kind, scope, externalID, dedupKey string) {
		t.Helper()
		mustExec(t, database, `
			INSERT INTO deliverables (id, org_id, conversation_id, team_id, provider, kind, scope, target, external_id, state, dedup_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'x', ?, 'posted', ?)`,
			id, orgID, conversation, teamID, provider, kind, scope, externalID, dedupKey)
	}
	deliverable("d-jira", "c-1", "jira", "issue", jiraSite, "10042", jiraIssueKey)
	deliverable("d-jira-by-key", "c-1", "jira", "issue", jiraSite, "10050", "jira:issue:SKY-50")
	deliverable("d-linear", "c-1", "linear", "comment", linearWS, "cmt-1", linearCommentKey)
	deliverable("d-reply", "c-1", "github", "comment", githubURL, "777", replyKey)
	deliverable("d-review-comment", "c-1", "github", "comment", githubURL, "555", domain.GitHubCommentKey(domain.GitHubReviewComment, 555))
	// Two comments this conversation posted with one number, one in each
	// numbering: a post naming that number matches both.
	deliverable("d-issue-comment-901", "c-1", "github", "comment", githubURL, "901", domain.GitHubCommentKey(domain.GitHubIssueComment, 901))
	deliverable("d-review-comment-901", "c-1", "github", "comment", githubURL, "901", domain.GitHubCommentKey(domain.GitHubReviewComment, 901))
	slackKey := domain.ObjectKey(domain.ProviderSlack, domain.DeliverableKindMessage, domain.SlackMessageResource("chan-row", "1700000000.000100"), "")
	deliverable("d-slack", "c-1", "slack", "message", domain.SlackScope, "1700000000.000100", slackKey)

	action := func(id, provider, scope, act, conversation, externalID, entityID, repositoryID string) {
		t.Helper()
		null := func(v string) any {
			if v == "" {
				return nil
			}
			return v
		}
		mustExec(t, database, `
			INSERT INTO external_actions (id, org_id, team_id, provider, scope, action, target, external_id,
			                              conversation_id, credential, dedup_key, entity_id, repository_id)
			VALUES (?, ?, ?, ?, ?, ?, 'x', ?, ?, 'github_app', ?, ?, ?)`,
			id, orgID, teamID, provider, scope, act, null(externalID), null(conversation), id, null(entityID), null(repositoryID))
	}
	action("branch", "github", githubURL, domain.ActionBranchPushed, "c-1", "refs/heads/fix", "", repoID)
	action("branch-no-repository", "github", githubURL, domain.ActionBranchPushed, "c-1", "refs/heads/fix", "", "")
	action("pr", "github", githubURL, domain.ActionPRMarkedReady, "", "7", entity.ID, "")
	action("pr-no-entity", "github", githubURL, domain.ActionPRMerged, "c-1", "8", "", "")
	action("reaction", "github", githubURL, domain.ActionReactionAdded, "c-1", "", entity.ID, "")
	action("review", "github", githubURL, domain.ActionReviewSubmitted, "c-1", "4242", entity.ID, "")
	action("review-without-id", "github", githubURL, domain.ActionReviewSubmitted, "c-1", "0", entity.ID, "")
	action("github-issue", "github", githubURL, domain.ActionIssueClosed, "c-1", "7", entity.ID, "")
	action("repository", "github", githubURL, domain.ActionRepoEdited, "c-1", "", "", repoID)
	action("release", "github", githubURL, domain.ActionReleaseEdited, "c-1", "31", "", repoID)
	action("workflow", "github", githubURL, domain.ActionWorkflowDispatched, "c-1", "ci.yml", "", repoID)
	action("workflow-run", "github", githubURL, domain.ActionWorkflowRunCancelled, "c-1", "77", "", repoID)
	action("label", "github", githubURL, domain.ActionLabelDefinitionEdited, "c-1", "needs triage", "", repoID)
	action("slack-reaction", "slack", domain.SlackScope, domain.ActionSlackReactionAdded, "c-1", "1700000000.000100", "", "")
	action("post-in-both-numberings", "github", githubURL, domain.ActionCommentPosted, "c-1", "901", "", "")
	action("jira", "jira", jiraSite, domain.ActionIssueTransitioned, "c-1", "10042", "", "")
	action("jira-other-conversation", "jira", jiraSite, domain.ActionIssueUpdated, "c-2", "10042", "", "")
	action("jira-mirror", "jira", jiraSite, domain.ActionIssueTransitioned, "", "10042", "", "")
	action("jira-keyed-by-key", "jira", jiraSite, domain.ActionIssueUpdated, "c-1", "10050", "", "")
	action("linear", "linear", linearWS, domain.ActionIssueCommentPosted, "c-1", "cmt-1", "", "")
	action("reply", "github", githubURL, domain.ActionCommentPosted, "c-1", "777", "", "")
	action("edit-other-numbering", "github", githubURL, domain.ActionCommentEdited, "c-1", "555", "", "")
	action("denied", "github", githubURL, domain.ActionGitDenied, "c-1", "refs/heads/main", "", repoID)

	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, externalActionObjectsVersion)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo: %v", upErr)
	}

	want := map[string][2]string{
		"branch":                  {domain.ObjectKindBranch, "refs/heads/fix"},
		"branch-no-repository":    {},
		"pr":                      {domain.ObjectKindPullRequest, ""},
		"pr-no-entity":            {},
		"reaction":                {},
		"review":                  {domain.ObjectKindReview, domain.GitHubReviewObjectKey("4242")},
		"review-without-id":       {},
		"github-issue":            {domain.ObjectKindIssue, ""},
		"repository":              {domain.ObjectKindRepository, ""},
		"release":                 {domain.ObjectKindRelease, "31"},
		"workflow":                {domain.ObjectKindWorkflow, "ci.yml"},
		"workflow-run":            {domain.ObjectKindWorkflowRun, "77"},
		"label":                   {domain.ObjectKindLabel, "needs triage"},
		"slack-reaction":          {domain.ObjectKindMessage, slackKey},
		"post-in-both-numberings": {},
		"jira":                    {domain.ObjectKindIssue, jiraIssueKey},
		"jira-other-conversation": {},
		"jira-mirror":             {},
		"jira-keyed-by-key":       {},
		"linear":                  {domain.ObjectKindComment, linearCommentKey},
		"reply":                   {domain.ObjectKindComment, replyKey},
		"edit-other-numbering":    {},
		"denied":                  {},
	}
	for id, w := range want {
		var kind, key sql.NullString
		if err := database.QueryRow(`SELECT object_kind, object_key FROM external_actions WHERE id = ?`, id).Scan(&kind, &key); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if kind.String != w[0] || key.String != w[1] || kind.Valid != (w[0] != "") || key.Valid != (w[1] != "") {
			t.Errorf("%s object = (%v, %v), want (%q, %q)", id, kind, key, w[0], w[1])
		}
	}
}
