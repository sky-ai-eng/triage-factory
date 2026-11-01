package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestResolveArtifactActivityFilters is a Docker-free unit test for the
// artifacts feeds' body validation: the pass-through filters, the time window
// (RFC3339 / YYYY-MM-DD, unbounded by default, inverted rejected), and the
// closed vocabularies, each fault naming its field.
func TestResolveArtifactActivityFilters(t *testing.T) {
	var v httpx.Validation
	opts := resolveArtifactActivityFilters(&v, artifactActivityListRequest{})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("empty body was rejected")
	}
	if opts.Provider != "" || opts.ObjectKind != "" || !opts.Since.IsZero() || !opts.Until.IsZero() {
		t.Errorf("default opts not empty: %+v", opts)
	}

	v = httpx.Validation{}
	opts = resolveArtifactActivityFilters(&v, artifactActivityListRequest{
		Provider: "github", ObjectKind: domain.ObjectKindBranch,
		Since: "2026-06-01", Until: "2026-06-30T12:00:00Z",
	})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("valid body was rejected")
	}
	if opts.Provider != "github" || opts.ObjectKind != domain.ObjectKindBranch {
		t.Errorf("filters not parsed: %+v", opts)
	}
	if !opts.Since.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) || !opts.Until.Equal(time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("window not parsed: since=%v until=%v", opts.Since, opts.Until)
	}

	for name, tc := range map[string]struct {
		req   artifactActivityListRequest
		field string
	}{
		"unknown_provider":    {artifactActivityListRequest{Provider: "gitlab"}, "provider"},
		"unknown_object_kind": {artifactActivityListRequest{ObjectKind: "pull_requests"}, "object_kind"},
		"deliverable_kind":    {artifactActivityListRequest{ObjectKind: domain.DeliverableKindIssue + "s"}, "object_kind"},
		"bad_since":           {artifactActivityListRequest{Since: "yesterday"}, "since"},
		"bad_until":           {artifactActivityListRequest{Until: "tomorrow"}, "until"},
		"inverted_window":     {artifactActivityListRequest{Since: "2026-06-30", Until: "2026-06-01"}, "since"},
	} {
		var v httpx.Validation
		resolveArtifactActivityFilters(&v, tc.req)
		rec := httptest.NewRecorder()
		if !v.Flush(rec, http.StatusBadRequest) {
			t.Errorf("%s: expected a validation error, got none", name)
			continue
		}
		if !strings.Contains(rec.Body.String(), `"field":"`+tc.field+`"`) {
			t.Errorf("%s: error does not name %q: %s", name, tc.field, rec.Body.String())
		}
	}

	// Every value the ledger can store is accepted.
	for _, kind := range domain.ObjectKinds() {
		var v httpx.Validation
		resolveArtifactActivityFilters(&v, artifactActivityListRequest{ObjectKind: kind})
		if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("object_kind=%q rejected", kind)
		}
	}
	for _, provider := range domain.Providers() {
		var v httpx.Validation
		resolveArtifactActivityFilters(&v, artifactActivityListRequest{Provider: provider})
		if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("provider=%q rejected", provider)
		}
	}
}

// TestResolveActionActivityFilters mirrors the artifacts test for the actions
// feed: provider and the time window are identical; object_kind becomes
// action/actor.
func TestResolveActionActivityFilters(t *testing.T) {
	var v httpx.Validation
	opts := resolveActionActivityFilters(&v, actionActivityListRequest{})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("empty body was rejected")
	}
	if opts.Provider != "" || opts.Action != "" || opts.ActorUserID != "" {
		t.Errorf("default opts not empty: %+v", opts)
	}

	actor := "11111111-1111-1111-1111-111111111111"
	v = httpx.Validation{}
	opts = resolveActionActivityFilters(&v, actionActivityListRequest{
		Provider: "github", Action: domain.ExternalActionTypes()[0], Actor: actor,
		Since: "2026-06-01", Until: "2026-06-30",
	})
	if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
		t.Fatal("valid body was rejected")
	}
	if opts.Provider != "github" || opts.ActorUserID != actor {
		t.Errorf("filters not parsed: %+v", opts)
	}

	for name, req := range map[string]actionActivityListRequest{
		"unknown_provider": {Provider: "gitlab"},
		"unknown_action":   {Action: "pr_openedd"},
		"bad_actor":        {Actor: "not-a-uuid"},
		"bad_since":        {Since: "yesterday"},
		"inverted_window":  {Since: "2026-06-30", Until: "2026-06-01"},
	} {
		var v httpx.Validation
		resolveActionActivityFilters(&v, req)
		if !v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("%s: expected a validation error, got none", name)
		}
	}
	// Every action type the writers store must be accepted.
	for _, action := range domain.ExternalActionTypes() {
		var v httpx.Validation
		resolveActionActivityFilters(&v, actionActivityListRequest{Action: action})
		if v.Flush(httptest.NewRecorder(), http.StatusBadRequest) {
			t.Errorf("action=%q rejected", action)
		}
	}
}

// activityReq builds a POST for one of the four bot-activity list routes,
// carrying the active org + caller claims (and the team_id path value when
// set), with body as the JSON list request — "" means an empty body object.
// Mirrors usageRig.req but speaks the list contract. The handler is invoked
// directly so the test exercises the governance gate + role gates + RLS without
// the cookie-session dance.
func (r *usageRig) activityReq(caller, teamID, body string) *http.Request {
	if body == "" {
		body = "{}"
	}
	req := httptest.NewRequest(http.MethodPost, "/api/activity/list", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("org_id", r.orgID)
	if teamID != "" {
		req.SetPathValue("team_id", teamID)
	}
	ctx := httpx.WithOrgID(req.Context(), r.orgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
	return req.WithContext(ctx)
}

// artifactSeed is what seedArtifacts staged, for the assertions to name.
type artifactSeed struct {
	branch, pr, issue, review            domain.ObjectRef
	prDeliverableID, reviewDeliverableID string
}

// seedArtifacts stages the ledger the artifacts feeds read. Three runs push
// one branch, two on teamA and one on teamB; one teamA run opens a pull
// request, which has a newer and an older deliverable on teamA; one teamB
// run transitions a Jira issue; and a person's approval posts a teamA run's
// staged review, which has its review deliverable. TF's own row on the pull
// request, a refused push on teamA, and a person's dashboard toggle of the
// pull request, which names no run, must not show.
func (r *usageRig) seedArtifacts(t *testing.T) artifactSeed {
	t.Helper()
	ctx := t.Context()
	stores := pgstore.New(r.h.AdminDB, r.h.AdminDB, pgtest.SecretKey)
	const host = "https://github.com"
	repo, err := stores.Repos.UpsertSystem(ctx, r.orgID, domain.Repository{Host: host, Owner: "octo", Repo: "api", ExternalID: "31"})
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	pr, _, err := stores.Entities.FindOrCreateSystem(ctx, r.orgID, "github", host,
		"octo/api#7", "PR_usage7", domain.EntityKindPullRequest, "A PR", host+"/octo/api/pull/7")
	if err != nil {
		t.Fatalf("seed pull request entity: %v", err)
	}
	conversation := func(teamID, creator string) string {
		id := uuid.NewString()
		pgtest.MustExec(t, r.h.AdminDB, `
			INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
			VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'open', 'team')`, id, r.orgID, teamID, creator)
		return id
	}
	a1, a2, b1 := conversation(r.teamA, r.member), conversation(r.teamA, r.member), conversation(r.teamB, r.orgAdmin)

	base := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	n := 0
	record := func(teamID, conversationID string, a domain.ExternalAction) domain.ExternalAction {
		t.Helper()
		n++
		a.TeamID, a.ConversationID, a.DedupKey = teamID, conversationID, uuid.NewString()
		if a.Credential == "" {
			a.Credential = domain.CredentialGitHubApp
		}
		if a.PerformedBy == "" {
			a.PerformedBy = domain.PerformedByAgent
		}
		if err := stores.ExternalActions.RecordSystem(ctx, r.orgID, a); err != nil {
			t.Fatalf("record %s: %v", a.Action, err)
		}
		pgtest.MustExec(t, r.h.AdminDB, `UPDATE external_actions SET occurred_at = $2 WHERE org_id = $1 AND dedup_key = $3`,
			r.orgID, base.Add(time.Duration(n)*time.Hour), a.DedupKey)
		return a
	}
	push := domain.ExternalAction{
		Provider: domain.ProviderGitHub, Scope: host, Action: domain.ActionBranchPushed, Target: "octo/api",
		ExternalID: "refs/heads/fix", RepositoryID: repo.ID, ObjectKind: domain.ObjectKindBranch, ObjectKey: "refs/heads/fix",
	}
	opened := domain.ExternalAction{
		Provider: domain.ProviderGitHub, Scope: host, Action: domain.ActionPRCreated, Target: "octo/api#7",
		EntityID: pr.ID, RepositoryID: repo.ID, ObjectKind: domain.ObjectKindPullRequest,
	}
	transitioned := domain.ExternalAction{
		Provider: domain.ProviderJira, Scope: scopeForProvider("jira"), Action: domain.ActionIssueTransitioned,
		Target: "SKY-1", Credential: domain.CredentialJiraOrg, ObjectKind: domain.ObjectKindIssue, ObjectKey: usageSeedIssueKey,
	}
	branchRow := record(r.teamA, a1, push)
	record(r.teamA, a2, push)
	record(r.teamB, b1, push)
	prRow := record(r.teamA, a1, opened)
	issueRow := record(r.teamB, b1, transitioned)
	reviewRow := record(r.teamA, a2, domain.ExternalAction{
		Provider: domain.ProviderGitHub, Scope: host, Action: domain.ActionReviewSubmitted, Target: "octo/api#7",
		ExternalID: "901", EntityID: pr.ID, RepositoryID: repo.ID, ActorUserID: r.member,
		PerformedBy: domain.PerformedByPerson, ObjectKind: domain.ObjectKindReview,
		ObjectKey: domain.GitHubReviewObjectKey("901"),
	})
	system := opened
	system.Action, system.PerformedBy = domain.ActionPRClosed, domain.PerformedBySystem
	record(r.teamA, a1, system)
	record(r.teamA, a2, domain.ExternalAction{
		Provider: domain.ProviderGitHub, Scope: host, Action: domain.ActionGitDenied, Target: "octo/api",
	})
	// A person's draft toggle on the dashboard, after every other row: it
	// names the pull request but no run and no team.
	toggle := opened
	toggle.Action, toggle.PerformedBy, toggle.ActorUserID = domain.ActionPRMarkedReady, domain.PerformedByPerson, r.member
	record("", "", toggle)

	deliverable := func(state string, created time.Time) string {
		id := uuid.NewString()
		pgtest.MustExec(t, r.h.AdminDB, `
			INSERT INTO deliverables (id, org_id, team_id, conversation_id, provider, scope, kind, target, state, dedup_key, entity_id, created_at)
			VALUES ($1, $2, $3, $4, 'github', $5, 'pull_request', 'octo/api#7', $6, $7, $8, $9)
		`, id, r.orgID, r.teamA, a1, host, state, uuid.NewString(), pr.ID, created)
		return id
	}
	deliverable(domain.DeliverableStatePRClosed, base)
	newest := deliverable(domain.DeliverableStatePROpen, base.Add(time.Minute))
	posted := uuid.NewString()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO deliverables (id, org_id, team_id, conversation_id, provider, scope, kind, target, state, dedup_key, external_id, entity_id)
		VALUES ($1, $2, $3, $4, 'github', $5, 'review', 'octo/api#7', 'submitted', $6, '901', $7)
	`, posted, r.orgID, r.teamA, a2, host, uuid.NewString(), pr.ID)

	ref := func(a domain.ExternalAction) domain.ObjectRef {
		id, ok := domain.ObjectID(a)
		if !ok {
			t.Fatalf("seeded %s names no object", a.Action)
		}
		return domain.ObjectRef(id)
	}
	return artifactSeed{
		branch: ref(branchRow), pr: ref(prRow), issue: ref(issueRow), review: ref(reviewRow),
		prDeliverableID: newest, reviewDeliverableID: posted,
	}
}

// TestUsageArtifactsActivityHandler_Postgres pins the artifacts feeds: the
// governance and role gates, one item per object with its run count and (org
// feed) team names, the deliverable match, the excluded person's and refused
// rows, strict decoding and closed filters, and paging over objects.
func TestUsageArtifactsActivityHandler_Postgres(t *testing.T) {
	r := newUsageRig(t)
	seed := r.seedArtifacts(t)
	t.Cleanup(entitlements.Reset)

	t.Run("unlicensed_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("team artifacts unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("org artifacts unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

	t.Run("role_gates_403", func(t *testing.T) {
		license()
		for name, call := range map[string]func(*httptest.ResponseRecorder){
			"team_plain_member": func(rec *httptest.ResponseRecorder) {
				r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.member, r.teamA, ""))
			},
			"org_member": func(rec *httptest.ResponseRecorder) {
				r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.member, "", ""))
			},
			"org_team_admin": func(rec *httptest.ResponseRecorder) {
				r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.teamAdmin, "", ""))
			},
		} {
			rec := httptest.NewRecorder()
			call(rec)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s = %d, want 403; body=%s", name, rec.Code, rec.Body.String())
			}
		}
	})

	byObject := func(t *testing.T, items []objectActivityJSON) map[domain.ObjectRef]objectActivityJSON {
		t.Helper()
		out := map[domain.ObjectRef]objectActivityJSON{}
		for _, it := range items {
			out[it.ObjectID] = it
		}
		return out
	}

	t.Run("team_feed_groups_the_teams_rows_per_object", func(t *testing.T) {
		license()
		// The org admin is on teamB only: the team feed still shows teamA's
		// deliverable.
		for _, caller := range []string{r.teamAdmin, r.orgAdmin} {
			rec := httptest.NewRecorder()
			r.uh.handleUsageTeamArtifacts(rec, r.activityReq(caller, r.teamA, ""))
			out := decodeList[objectActivityJSON](t, rec)
			if len(out.Items) != 3 || out.Total() != 3 {
				t.Fatalf("teamA feed = %d items / total %d, want 3/3 (the branch, the PR, the review): %+v", len(out.Items), out.Total(), out.Items)
			}
			if strings.Contains(rec.Body.String(), `"teams"`) {
				t.Errorf("team feed carried teams: %s", rec.Body.String())
			}
			got := byObject(t, out.Items)
			if b := got[seed.branch]; b.ConversationCount != 2 || b.ActionCount != 2 || b.LastAction != domain.ActionBranchPushed || b.Deliverable != nil {
				t.Errorf("teamA branch = %+v, want 2 runs of 2 pushes and no deliverable", b)
			}
			p := got[seed.pr]
			if p.ConversationCount != 1 || p.ActionCount != 1 || p.LastAction != domain.ActionPRCreated {
				t.Errorf("teamA PR = %+v, want the one pr_created alone, without TF's own close", p)
			}
			if p.Deliverable == nil || p.Deliverable.ID != seed.prDeliverableID ||
				p.Deliverable.Kind != domain.DeliverableKindPullRequest || p.Deliverable.State != domain.DeliverableStatePROpen {
				t.Errorf("teamA PR deliverable = %+v, want the newest one %s (open)", p.Deliverable, seed.prDeliverableID)
			}
			// The review a person's approval posted is the run's object, and
			// it is its review deliverable.
			rv := got[seed.review]
			if rv.ObjectKind != domain.ObjectKindReview || rv.LastAction != domain.ActionReviewSubmitted || rv.ConversationCount != 1 {
				t.Errorf("teamA review = %+v, want the posted review from its one run", rv)
			}
			if rv.Deliverable == nil || rv.Deliverable.ID != seed.reviewDeliverableID ||
				rv.Deliverable.Kind != domain.DeliverableKindReview || rv.Deliverable.State != domain.DeliverableStateReviewSubmitted {
				t.Errorf("teamA review deliverable = %+v, want %s (submitted)", rv.Deliverable, seed.reviewDeliverableID)
			}
		}
	})

	t.Run("org_feed_groups_across_teams_with_names", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", ""))
		out := decodeList[orgObjectActivityJSON](t, rec)
		if len(out.Items) != 4 || out.Total() != 4 {
			t.Fatalf("org feed = %d items / total %d, want 4/4: %+v", len(out.Items), out.Total(), out.Items)
		}
		// LastAt descending: the review (6h), the issue (5h), the PR (4h), the
		// branch (3h).
		want := []domain.ObjectRef{seed.review, seed.issue, seed.pr, seed.branch}
		for i, it := range out.Items {
			if it.ObjectID != want[i] {
				t.Errorf("item %d = %q, want %q", i, it.ObjectID, want[i])
			}
		}
		teamNames := map[string]string{r.teamB: "teamB"}
		for _, it := range out.Items {
			if it.ObjectID == seed.pr && (it.ActionCount != 1 || it.LastAction != domain.ActionPRCreated || len(it.Teams) != 1) {
				t.Errorf("org PR = %+v, want the run's one pr_created on teamA, without the dashboard toggle", it)
			}
			if it.ObjectID != seed.branch {
				continue
			}
			if it.ConversationCount != 3 || it.ActionCount != 3 || len(it.Teams) != 2 {
				t.Fatalf("org branch = %+v, want 3 runs across 2 teams", it)
			}
			for _, team := range it.Teams {
				if team.ID != r.teamA && team.ID != r.teamB {
					t.Errorf("org branch team %+v is neither seeded team", team)
				}
				if want, ok := teamNames[team.ID]; ok && team.Name != want {
					t.Errorf("team %s name = %q, want %q", team.ID, team.Name, want)
				}
				if team.Name == "" {
					t.Errorf("team %s has no name", team.ID)
				}
			}
			if it.Teams[0].ID > it.Teams[1].ID {
				t.Errorf("teams = %+v, want sorted by id", it.Teams)
			}
		}
		// The wire spelling of object_id is the one the actions filter takes.
		wire, _ := seed.branch.MarshalText()
		if !strings.Contains(rec.Body.String(), `"object_id":"`+string(wire)+`"`) {
			t.Errorf("org feed body carries no object_id %q: %s", wire, rec.Body.String())
		}
	})

	t.Run("filters", func(t *testing.T) {
		license()
		for body, want := range map[string]domain.ObjectRef{
			`{"object_kind":"branch"}`:                           seed.branch,
			`{"provider":"jira"}`:                                seed.issue,
			`{"since":"2026-06-10T14:30:00Z"}`:                   seed.review,
			`{"until":"2026-06-10T12:30:00Z"}`:                   seed.branch,
			`{"object_kind":"pull_request","provider":"github"}`: seed.pr,
		} {
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", body))
			out := decodeList[orgObjectActivityJSON](t, rec)
			if len(out.Items) != 1 || out.Total() != 1 || out.Items[0].ObjectID != want {
				t.Errorf("%s = %d items / total %d (%+v), want %q alone", body, len(out.Items), out.Total(), out.Items, want)
			}
		}
		// The window selects rows: before 12:30 only the first two pushes.
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"until":"2026-06-10T11:30:00Z"}`))
		if out := decodeList[orgObjectActivityJSON](t, rec); len(out.Items) != 1 || out.Items[0].ConversationCount != 2 || len(out.Items[0].Teams) != 1 {
			t.Errorf("windowed branch = %+v, want 2 runs on one team", out.Items)
		}
	})

	t.Run("bad_bodies_400", func(t *testing.T) {
		license()
		for _, body := range []string{
			`{"state":"open"}`,
			`{"kind":"pull_request"}`,
			`{"object_kind":"pull_requests"}`,
			`{"object_kind":7}`,
			`{"provider":"gitlab"}`,
			`{"since":"last week"}`,
			`{"page_size":100000}`,
		} {
			rec := httptest.NewRecorder()
			r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.teamAdmin, r.teamA, body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("team %s = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("org %s = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("paging_walks_objects", func(t *testing.T) {
		license()
		seen := map[domain.ObjectRef]bool{}
		token := ""
		for page := 0; page < 4; page++ {
			body := `{"page_size":2}`
			if token != "" {
				body = `{"page_size":2,"page_token":"` + token + `"}`
			}
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", body))
			out := decodeList[orgObjectActivityJSON](t, rec)
			if out.Total() != 4 {
				t.Fatalf("page %d total = %d, want 4", page, out.Total())
			}
			for _, it := range out.Items {
				if seen[it.ObjectID] {
					t.Errorf("object %q served twice", it.ObjectID)
				}
				seen[it.ObjectID] = true
			}
			if token = out.NextPageToken; token == "" {
				break
			}
		}
		if len(seen) != 4 {
			t.Errorf("paged %d objects, want 4", len(seen))
		}
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"page_size":0}`))
		if out := decodeList[orgObjectActivityJSON](t, rec); len(out.Items) != 0 || out.Total() != 4 {
			t.Errorf("count-only read = %d items / total %d, want 0/4", len(out.Items), out.Total())
		}
	})
}

// seedExternalActions stages the Actions-lens rows: two on teamA (a bot
// pr_created with no actor + a human-authorized pr_marked_ready by orgAdmin) and
// one system row on teamB (a Jira transition, no actor). orgAdmin's display name
// is pinned so the org feed's resolved actor_name is deterministic.
// scopeForProvider is the namespace a seeded audit row is recorded in.
func scopeForProvider(provider string) string {
	if provider == "jira" {
		return "https://acme.atlassian.net"
	}
	return "https://github.com"
}

func (r *usageRig) seedExternalActions(t *testing.T) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE users SET display_name = 'Org Admin' WHERE id = $1`, r.orgAdmin)
	seed := func(teamID, provider, action, target, cred, key, actor, from, to, performedBy string) {
		var actorArg any
		if actor != "" {
			actorArg = actor
		}
		pgtest.MustExec(t, r.h.AdminDB, `
			INSERT INTO external_actions (org_id, team_id, provider, scope, action, target, credential, dedup_key, actor_user_id, from_state, to_state, performed_by)
			VALUES ($1, $2, $3, $11, $4, $5, $6, $7, $8, NULLIF($9,''), NULLIF($10,''), $12)
		`, r.orgID, teamID, provider, action, target, cred, key, actorArg, from, to, scopeForProvider(provider), performedBy)
	}
	seed(r.teamA, "github", "pr_created", "octo/repo#1", "github_app", "tA-created", "", "", "draft", domain.PerformedByAgent)
	seed(r.teamA, "github", "pr_marked_ready", "octo/repo#1", "github_app", "tA-ready", r.orgAdmin, "draft", "open", domain.PerformedByPerson)
	seed(r.teamB, "jira", "issue_transitioned", "SKY-1", "jira_org", "tB-trans", "", "To Do", "Done", domain.PerformedBySystem)
	// The transition names its issue, so the feed carries an object.
	pgtest.MustExec(t, r.h.AdminDB, `
		UPDATE external_actions SET object_kind = 'issue', object_key = $2
		 WHERE org_id = $1 AND dedup_key = 'tB-trans'`, r.orgID, usageSeedIssueKey)
}

// usageSeedIssueKey is the object_key seedExternalActions gives its Jira row.
const usageSeedIssueKey = "jira:issue:acme-10001"

// TestUsageActionsActivityHandler_Postgres pins the actions feeds: the
// governance gate, the cross-team org read with resolved team and actor names (a
// human row names its authorizer, a system row none), the team-scoped read, and
// the action filter.
func TestUsageActionsActivityHandler_Postgres(t *testing.T) {
	r := newUsageRig(t)
	r.seedExternalActions(t)
	t.Cleanup(entitlements.Reset)

	// Unlicensed → 404, exactly like the artifacts feed (the gate is the shared
	// caller resolver, so it runs identically on both resources).
	t.Run("unlicensed_404", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("org actions unlicensed = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

	t.Run("team_admin_200_scoped", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActions(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		out := decodeList[actionJSON](t, rec)
		if len(out.Items) != 2 || out.Total() != 2 {
			t.Fatalf("teamA actions = %d rows / total %d, want 2/2 (created + marked-ready)", len(out.Items), out.Total())
		}
		// The team feed omits the team chip (already one team) but DOES resolve the
		// authorizing actor's name — a team admin shouldn't see raw UUIDs.
		var sawHumanName bool
		for _, a := range out.Items {
			if a.TeamID != "" || a.TeamName != "" {
				t.Errorf("team feed row carried a team chip: %+v", a)
			}
			switch a.Action {
			case "pr_marked_ready":
				if a.ActorUserID != r.orgAdmin || a.ActorName != "Org Admin" {
					t.Errorf("team feed human row actor = %q/%q, want orgAdmin/'Org Admin' (resolved)", a.ActorUserID, a.ActorName)
				}
				sawHumanName = true
			case "pr_created":
				if a.ActorName != "" {
					t.Errorf("bot row carried an actor name: %+v", a)
				}
			}
		}
		if !sawHumanName {
			t.Error("team feed did not resolve the human-authorized row's actor name")
		}
	})

	t.Run("org_admin_200_cross_team_with_names", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", ""))
		out := decodeList[actionJSON](t, rec)
		if len(out.Items) != 3 || out.Total() != 3 {
			t.Fatalf("org actions = %d rows / total %d, want 3/3 (cross-team)", len(out.Items), out.Total())
		}
		// The org feed shares the action projection: the Jira row's object
		// rides it in its wire spelling, which decodes back to the raw id, and
		// the rows that name no object carry neither key.
		wantObject := domain.ObjectRef("issue|" + scopeForProvider("jira") + "|" + usageSeedIssueKey)
		wire, _ := wantObject.MarshalText()
		if !strings.Contains(rec.Body.String(), `"object_id":"`+string(wire)+`"`) {
			t.Errorf("org feed body carries no object_id %q: %s", wire, rec.Body.String())
		}
		for _, a := range out.Items {
			want := domain.ObjectRef("")
			if a.Action == "issue_transitioned" {
				want = wantObject
			}
			if a.ObjectID != want || (want != "") != (a.ObjectKind == domain.ObjectKindIssue) {
				t.Errorf("%s row object = (%q, %q), want %q", a.Action, a.ObjectKind, a.ObjectID, want)
			}
		}
		var sawTeamB, sawHumanActor, sawSystem bool
		wantPerformer := map[string]string{
			"pr_created":         domain.PerformedByAgent,
			"pr_marked_ready":    domain.PerformedByPerson,
			"issue_transitioned": domain.PerformedBySystem,
		}
		for _, a := range out.Items {
			if a.TeamID == "" || a.TeamName == "" {
				t.Errorf("org feed action missing team id/name: %+v", a)
			}
			if a.TeamID == r.teamB {
				sawTeamB = true
			}
			if a.PerformedBy != wantPerformer[a.Action] {
				t.Errorf("%s row performed_by = %q, want %q", a.Action, a.PerformedBy, wantPerformer[a.Action])
			}
			switch a.Action {
			case "pr_marked_ready":
				// The human-authorized row names its authorizer + carries the transition.
				if a.ActorUserID != r.orgAdmin || a.ActorName != "Org Admin" {
					t.Errorf("human row actor = %q/%q, want orgAdmin/'Org Admin'", a.ActorUserID, a.ActorName)
				}
				if a.FromState != "draft" || a.ToState != "open" {
					t.Errorf("human row transition = %q→%q, want draft→open", a.FromState, a.ToState)
				}
				sawHumanActor = true
			case "pr_created", "issue_transitioned":
				// System/bot rows have no actor.
				if a.ActorUserID != "" || a.ActorName != "" {
					t.Errorf("system row carried an actor: %+v", a)
				}
				sawSystem = true
			}
		}
		if !sawTeamB || !sawHumanActor || !sawSystem {
			t.Errorf("missing coverage: teamB=%v human=%v system=%v", sawTeamB, sawHumanActor, sawSystem)
		}
	})

	t.Run("filter_passthrough_action", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", `{"action":"pr_created"}`))
		out := decodeList[actionJSON](t, rec)
		if len(out.Items) != 1 || out.Items[0].Action != "pr_created" {
			t.Errorf("action=pr_created returned %d rows (%+v), want exactly the one", len(out.Items), out.Items)
		}
		if out.Total() != 1 {
			t.Errorf("filtered total = %d, want 1", out.Total())
		}
	})

	t.Run("actions_body_rejects_other_filters", func(t *testing.T) {
		// The two resources have different filter vocabularies, which is why
		// they are two routes: the other's `state` is an unknown field here.
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", `{"state":"open"}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("actions list with another resource's filter = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}
