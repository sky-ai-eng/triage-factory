package agenthost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRecorders_NameTheObjectALandedChangeChanged pins that a row recorded for
// a landed change names the object it changed, and that domain.ObjectID
// renders an identity for it, while a refusal names none. Each case makes one
// write through the recorder that ships it and returns the stores it recorded
// into and the identity the row must carry ("" for none).
func TestRecorders_NameTheObjectALandedChangeChanged(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		action string
		record func(t *testing.T) (db.Stores, string)
	}{
		{
			name:   "branch push",
			action: domain.ActionBranchPushed,
			record: func(t *testing.T) (db.Stores, string) {
				gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !answerRepoRead(w, r) {
						t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
					}
				}))
				t.Cleanup(gh.Close)
				stores, _, client := newGithubRecordingClient(t, gh.URL, false)
				repoID := registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))
				branch, _ := domain.NewBranchDeliverable("octo/repo", "refs/heads/feat", "abc123", true)
				if _, err := client.UpsertDeliverable(ctx, branch); err != nil {
					t.Fatalf("UpsertDeliverable: %v", err)
				}
				return stores, "branch|" + repoID + "|refs/heads/feat"
			},
		},
		{
			name:   "pull request create",
			action: domain.ActionPRCreated,
			record: func(t *testing.T) (db.Stores, string) {
				gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case answerRepoRead(w, r):
					case r.Method == http.MethodPost:
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/octo/repo/pull/42","node_id":"PR_kwDOABCD"}`)
					default:
						t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
					}
				}))
				t.Cleanup(gh.Close)
				stores, _, client := newGithubRecordingClient(t, gh.URL, true)
				registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))
				if _, _, _, err := client.GithubCreatePR(ctx, "octo", "repo", "feature/x", "main", "Fix", "Body", true); err != nil {
					t.Fatalf("GithubCreatePR: %v", err)
				}
				pr, err := stores.Entities.GetByExternalIDSystem(ctx, runmode.LocalDefaultOrgID, "github", testScope("github"), "PR_kwDOABCD")
				if err != nil || pr == nil {
					t.Fatalf("the pull request's entity: %v, %v", pr, err)
				}
				return stores, "pull_request|" + pr.ID
			},
		},
		{
			name:   "gh channel review-thread reply",
			action: domain.ActionCommentPosted,
			record: func(t *testing.T) (db.Stores, string) {
				gh := startFakeGitHubComments(t)
				stores, _, client := newGithubRecordingClient(t, gh.URL, true)
				client.RecordGHChannelWrite(ctx, ghwrite.Observation{
					Method:     "POST",
					Path:       "/repos/octo/repo/pulls/1/comments/555/replies",
					Status:     201,
					ExternalID: "777",
					URL:        gh.URL + "/octo/repo/pull/1#discussion_r777",
				})
				return stores, "comment|" + testScope("github") + "|" + domain.GitHubCommentKey(domain.GitHubReviewComment, 777)
			},
		},
		{
			// The transition's row recorded alone: its issue's entity comes
			// from the row's own object key.
			name:   "Jira transition with no deliverable",
			action: domain.ActionIssueTransitioned,
			record: func(t *testing.T) (db.Stores, string) {
				stores, info := newCaptureStores(t, true)
				site := testScope("jira")
				key := domain.IssueObjectKey(domain.ProviderJira, site, "10042")
				act := jiraAction(domain.Deliverable{
					Kind:       domain.DeliverableKindIssue,
					Target:     "SKY-42",
					ExternalID: "10042",
					DedupKey:   key,
				}, domain.ActionIssueTransitioned, "", "Done")
				recordExternalWrite(ctx, stores, info, nil, act,
					jiraContainer(jiraclient.Project{ID: "10000", Key: "SKY", Name: "Sky"}), domain.GitHubIdentity{})

				if got := listConversationDeliverables(t, stores, info.ConversationID); len(got) != 0 {
					t.Fatalf("the action was recorded with deliverables %+v", got)
				}
				issue, err := stores.Entities.GetByExternalIDSystem(ctx, runmode.LocalDefaultOrgID, "jira", site, "10042")
				if err != nil || issue == nil {
					t.Fatalf("the issue's entity: %v, %v", issue, err)
				}
				for _, a := range listExternalActions(t, stores) {
					if a.EntityID != issue.ID {
						t.Errorf("the transition links entity %q, want the issue's %s", a.EntityID, issue.ID)
					}
				}
				return stores, "issue|" + site + "|" + key
			},
		},
		{
			name:   "Linear comment",
			action: domain.ActionIssueCommentPosted,
			record: func(t *testing.T) (db.Stores, string) {
				_, srv := startFakeLinear(t)
				lc, stores, _ := linearProxyClient(t, srv.URL, true)
				if _, err := lc.LinearAddComment(ctx, "ENG-7", "on it"); err != nil {
					t.Fatalf("LinearAddComment: %v", err)
				}
				key := domain.ObjectKey(domain.ProviderLinear, domain.DeliverableKindComment, fakeLinearIssueUUID, fakeLinearCommentID)
				return stores, "comment|linear-workspace-uuid|" + key
			},
		},
		{
			name:   "git denial",
			action: domain.ActionGitDenied,
			record: func(t *testing.T) (db.Stores, string) {
				stores, info := newCaptureStores(t, true)
				NewLocal(stores, info).RecordGitDenied(ctx, "octo", "repo", "refs/heads/main", "push", "off_ref")
				return stores, ""
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stores, want := c.record(t)
			var rows []domain.ExternalAction
			for _, a := range listExternalActions(t, stores) {
				if a.Action == c.action {
					rows = append(rows, a)
				}
			}
			if len(rows) != 1 {
				t.Fatalf("want one %s row, got %+v", c.action, rows)
			}
			got, ok := domain.ObjectID(rows[0])
			if want == "" {
				if ok || rows[0].ObjectKind != "" || rows[0].ObjectKey != "" {
					t.Errorf("the row names object (%q, %q) = %q, want none", rows[0].ObjectKind, rows[0].ObjectKey, got)
				}
				return
			}
			if !ok || got != want {
				t.Errorf("ObjectID = (%q, %v), want %q (row %+v)", got, ok, want, rows[0])
			}
		})
	}
}

// TestRecordExternalWrite_KeepsOnlyAnObjectTheActionNames pins the funnel's
// guard: a row reaching it with an object its action cannot name, as a relayed
// row from a sidecar can, is recorded naming none, and the row itself lands.
func TestRecordExternalWrite_KeepsOnlyAnObjectTheActionNames(t *testing.T) {
	stores, info := newCaptureStores(t, false)
	for _, act := range []*domain.ExternalAction{
		{Provider: domain.ProviderGitHub, Action: domain.ActionGitDenied, Target: "octo/repo",
			Credential: domain.CredentialGitHubApp, ObjectKind: domain.ObjectKindComment, ObjectKey: "github:comment:1"},
		{Provider: domain.ProviderGitHub, Action: domain.ActionCommentPosted, Target: "octo/repo",
			Credential: domain.CredentialGitHubApp, ObjectKind: "not_a_kind", ObjectKey: "x"},
	} {
		RecordExternalWrite(context.Background(), stores, info, nil, act)
	}
	rows := listExternalActions(t, stores)
	if len(rows) != 2 {
		t.Fatalf("want both rows recorded, got %+v", rows)
	}
	for _, a := range rows {
		if a.ObjectKind != "" || a.ObjectKey != "" {
			t.Errorf("%s row names object (%q, %q), want none", a.Action, a.ObjectKind, a.ObjectKey)
		}
	}
}

// TestGHChannelWriteAction_NamesTheObject pins the object each classified,
// accepted gh-channel write names, by the kind its action changes and the key
// off the shape, and that a shape without what its key needs names none.
func TestGHChannelWriteAction_NamesTheObject(t *testing.T) {
	rest := func(method, path, externalID string) ghwrite.Observation {
		return ghwrite.Observation{Method: method, Path: path, Status: 200, ExternalID: externalID}
	}
	graphQL := func(mutation, subject string) ghwrite.Observation {
		return ghwrite.Observation{Method: "POST", Path: "/graphql", Status: 200,
			GraphQL: &ghwrite.GraphQLFacts{Fields: []string{mutation}, Subject: subject}}
	}
	cases := []struct {
		name      string
		obs       ghwrite.Observation
		kind, key string
	}{
		{"reaction on a conversation comment", rest("POST", "/repos/octo/repo/issues/comments/5/reactions", ""),
			domain.ObjectKindComment, domain.GitHubCommentKey(domain.GitHubIssueComment, 5)},
		{"reaction on a review comment", rest("POST", "/repos/octo/repo/pulls/comments/5/reactions", ""),
			domain.ObjectKindComment, domain.GitHubCommentKey(domain.GitHubReviewComment, 5)},
		{"reaction on a pull request", rest("POST", "/repos/octo/repo/issues/7/reactions", ""),
			domain.ObjectKindPullRequest, ""},
		{"GraphQL reaction", graphQL("addReaction", "IC_kw1"), "", ""},
		{"review dismissal", rest("PUT", "/repos/octo/repo/pulls/7/reviews/4242/dismissals", ""),
			domain.ObjectKindReview, domain.GitHubReviewObjectKey("4242")},
		{"label definition edit", rest("PATCH", "/repos/octo/repo/labels/needs%20triage", ""),
			domain.ObjectKindLabel, "needs triage"},
		{"release edit", rest("PATCH", "/repos/octo/repo/releases/31", ""), domain.ObjectKindRelease, "31"},
		{"workflow dispatch", rest("POST", "/repos/octo/repo/actions/workflows/ci.yml/dispatches", ""),
			domain.ObjectKindWorkflow, "ci.yml"},
		{"workflow run cancel", rest("POST", "/repos/octo/repo/actions/runs/77/cancel", ""),
			domain.ObjectKindWorkflowRun, "77"},
		{"repository edit", rest("PATCH", "/repos/octo/repo", ""), domain.ObjectKindRepository, ""},
		{"GraphQL comment delete naming only a node id", graphQL("deleteIssueComment", "IC_kw1"), "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			act := GHChannelWriteAction(c.obs, domain.CredentialGitHubApp)
			if act.ObjectKind != c.kind || act.ObjectKey != c.key {
				t.Errorf("%s row names (%q, %q), want (%q, %q)", act.Action, act.ObjectKind, act.ObjectKey, c.kind, c.key)
			}
		})
	}
}

// TestRecordExternalWrite_GitHubObjectKindIsTheEntitys pins that a GitHub row
// about a pull request or an issue names the kind of the entity it resolves
// to, whichever endpoint the act came through: GitHub numbers the two in one
// sequence and serves both through the issue endpoints, so the endpoint does
// not say which one the act was on.
func TestRecordExternalWrite_GitHubObjectKindIsTheEntitys(t *testing.T) {
	ctx := context.Background()
	stores, info := newCaptureStores(t, true)
	registerRepo(t, stores, "octo", "repo", "1")
	object := func(nodeID, kind string) domain.GitHubIdentity {
		return domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo", NodeID: nodeID, Kind: kind}
	}
	cases := []struct {
		name, action, target, endpointKind string
		read                               domain.GitHubIdentity
		want                               string
	}{
		{"label on an issue", domain.ActionLabelAdded, "octo/repo#5", domain.ObjectKindPullRequest,
			object("I_5", domain.EntityKindIssue), domain.ObjectKindIssue},
		{"issue endpoint on a pull request", domain.ActionIssueUpdated, "octo/repo#6", domain.ObjectKindIssue,
			object("PR_6", domain.EntityKindPullRequest), domain.ObjectKindPullRequest},
	}
	for _, c := range cases {
		act := &domain.ExternalAction{
			Provider: domain.ProviderGitHub, Action: c.action, Target: c.target,
			Credential: domain.CredentialGitHubApp, DedupKey: c.name,
		}
		act.SetObject(c.endpointKind, "")
		recordExternalWrite(ctx, stores, info, nil, act, domain.EntityContainer{}, c.read)
	}
	byKey := map[string]domain.ExternalAction{}
	for _, a := range listExternalActions(t, stores) {
		byKey[a.DedupKey] = a
	}
	for _, c := range cases {
		row, ok := byKey[c.name]
		if !ok || row.EntityID == "" {
			t.Fatalf("%s: row %+v, want one linked to its entity", c.name, row)
		}
		if got, ok := domain.ObjectID(row); !ok || got != c.want+"|"+row.EntityID {
			t.Errorf("%s: ObjectID = (%q, %v), want %q", c.name, got, ok, c.want+"|"+row.EntityID)
		}
	}
}
