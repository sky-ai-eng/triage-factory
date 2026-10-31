package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// artifactTestScope is the GitHub host seedSteerConversation's entity lives on.
const artifactTestScope = "https://github.com"

// artifactRow is an agent's GitHub ledger row naming an object on the seeded
// pull request's host.
func artifactRow(action, kind, key string) domain.ExternalAction {
	return domain.ExternalAction{
		Provider: domain.ProviderGitHub, Scope: artifactTestScope, Action: action,
		Target: "owner/repo", Credential: domain.CredentialGitHubApp, PerformedBy: domain.PerformedByAgent,
		ObjectKind: kind, ObjectKey: key,
	}
}

// TestHandleAgentArtifacts_MatchesDeliverables pins the artifact list's item
// shape and its deliverable match: the pull request's two rows are one
// artifact matched to the PR deliverable on the entity, the posted review
// matches the review deliverable carrying its id, and a branch and a review
// no deliverable carries match nothing. A staged review has no artifact. A
// pull request displays its entity's key, not the row's.
func TestHandleAgentArtifacts_MatchesDeliverables(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "artmatch", dbtest.SeedConcluded)
	entityID := fixtureUUID("e_artmatch")

	pr := domain.NewPullRequestDeliverable("owner/repo", 3, "PR_artmatch", "fix", "main",
		"https://github.com/owner/repo/pull/3", "T", "B", false)
	pr.EntityID, pr.Scope = entityID, artifactTestScope
	pr = seedConversationDeliverable(t, s, conversationID, pr)
	posted := domain.NewReviewDeliverable("owner/repo", 3, "PR_artmatch", "sha", conversationID)
	posted.EntityID, posted.Scope = entityID, artifactTestScope
	posted.State, posted.ExternalID = domain.DeliverableStateReviewSubmitted, "4242"
	posted = seedConversationDeliverable(t, s, conversationID, posted)
	staged := domain.NewReviewDeliverable("owner/repo", 3, "PR_other", "sha", conversationID)
	staged.EntityID, staged.Scope = entityID, artifactTestScope
	seedConversationDeliverable(t, s, conversationID, staged)

	created := artifactRow(domain.ActionPRCreated, domain.ObjectKindPullRequest, "")
	created.EntityID, created.Target = entityID, "owner/repo#old"
	ready := created
	ready.Action = domain.ActionPRMarkedReady
	created.ID = fixtureUUID("a_created")
	seedConversationAction(t, s, conversationID, created)
	seedConversationAction(t, s, conversationID, ready)
	// occurred_at has one-second resolution here; the create came first.
	execSQL(t, s.db, `UPDATE external_actions SET occurred_at = datetime('now', '-60 seconds') WHERE id = ?`, created.ID)
	seedConversationAction(t, s, conversationID,
		artifactRow(domain.ActionReviewSubmitted, domain.ObjectKindReview, domain.GitHubReviewObjectKey("4242")))
	seedConversationAction(t, s, conversationID,
		artifactRow(domain.ActionReviewSubmitted, domain.ObjectKindReview, domain.GitHubReviewObjectKey("9999")))
	push := artifactRow(domain.ActionBranchPushed, domain.ObjectKindBranch, "refs/heads/fix")
	push.RepositoryID = repositoryIDOfEntity(t, s, entityID)
	seedConversationAction(t, s, conversationID, push)

	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/"+conversationID+"/artifacts/list", map[string]any{})
	page := decodeList[map[string]any](t, rec)
	if len(page.Items) != 4 || page.Total() != 4 {
		t.Fatalf("returned %d artifacts (total %d), want 4: %s", len(page.Items), page.Total(), rec.Body.String())
	}
	byID := map[string]map[string]any{}
	for _, m := range page.Items {
		id, _ := m["object_id"].(string)
		byID[id] = m
	}
	objectID := func(a domain.ExternalAction) string {
		t.Helper()
		a.Scope = artifactTestScope
		id, ok := domain.ObjectID(a)
		if !ok {
			t.Fatalf("%s names no object", a.Action)
		}
		return wireObjectID(id)
	}
	deliverable := func(m map[string]any) map[string]any {
		d, _ := m["deliverable"].(map[string]any)
		return d
	}

	prItem := byID[objectID(created)]
	if prItem == nil {
		t.Fatalf("no pull request artifact: %s", rec.Body.String())
	}
	if prItem["object_kind"] != domain.ObjectKindPullRequest || prItem["provider"] != domain.ProviderGitHub ||
		prItem["scope"] != artifactTestScope || prItem["entity_id"] != entityID ||
		prItem["action_count"] != float64(2) || prItem["last_action"] != domain.ActionPRMarkedReady {
		t.Errorf("pull request artifact = %v", prItem)
	}
	if prItem["target"] != "owner/repo#artmatch" {
		t.Errorf("pull request target = %v, want the entity's key owner/repo#artmatch", prItem["target"])
	}
	for _, key := range []string{"url", "first_at", "last_at"} {
		if _, ok := prItem[key]; !ok {
			t.Errorf("pull request artifact has no %q: %v", key, prItem)
		}
	}
	if d := deliverable(prItem); d["id"] != pr.ID || d["kind"] != domain.DeliverableKindPullRequest || d["state"] != pr.State {
		t.Errorf("pull request deliverable = %v, want {%s pull_request %s}", prItem["deliverable"], pr.ID, pr.State)
	}

	reviewItem := byID[objectID(artifactRow(domain.ActionReviewSubmitted, domain.ObjectKindReview, domain.GitHubReviewObjectKey("4242")))]
	if d := deliverable(reviewItem); d["id"] != posted.ID || d["kind"] != domain.DeliverableKindReview || d["state"] != domain.DeliverableStateReviewSubmitted {
		t.Errorf("posted review deliverable = %v, want {%s review submitted}", reviewItem["deliverable"], posted.ID)
	}
	for name, id := range map[string]string{
		"unmatched review": objectID(artifactRow(domain.ActionReviewSubmitted, domain.ObjectKindReview, domain.GitHubReviewObjectKey("9999"))),
		"branch":           objectID(push),
	} {
		item := byID[id]
		if item == nil {
			t.Fatalf("no %s artifact: %s", name, rec.Body.String())
		}
		if d, present := item["deliverable"]; !present || d != nil {
			t.Errorf("%s deliverable = %v (present %v), want null", name, d, present)
		}
		if _, present := item["entity_id"]; present {
			t.Errorf("%s carries entity_id %v, want it omitted", name, item["entity_id"])
		}
	}
}

// repositoryIDOfEntity reads the registry row seedSteerConversation's entity
// references.
func repositoryIDOfEntity(t *testing.T, s *Server, entityID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT repository_id FROM entities WHERE id = ?`, entityID).Scan(&id); err != nil {
		t.Fatalf("read entity repository: %v", err)
	}
	return id
}

// TestHandleAgentArtifacts_UnknownConversationNotFound pins the read gate: a
// conversation the caller cannot read is a 404, not an empty list.
func TestHandleAgentArtifacts_UnknownConversationNotFound(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/"+fixtureUUID("ghost")+"/artifacts/list", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST missing conversation = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleAgentArtifacts_Empty pins that a conversation that changed
// nothing answers an empty page, never null.
func TestHandleAgentArtifacts_Empty(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "artnone", dbtest.SeedConcluded)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/"+conversationID+"/artifacts/list", map[string]any{})
	if page := decodeList[map[string]any](t, rec); len(page.Items) != 0 || page.Total() != 0 {
		t.Errorf("artifacts = %v (total %d), want an empty page", page.Items, page.Total())
	}
}

// TestHandleAgentActions_ObjectAndRefusedFilters pins the action list's two
// filters, alone and together, and the object fields its items carry.
func TestHandleAgentActions_ObjectAndRefusedFilters(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "actfilter", dbtest.SeedConcluded)
	repoID := repositoryIDOfEntity(t, s, fixtureUUID("e_actfilter"))
	push := artifactRow(domain.ActionBranchPushed, domain.ObjectKindBranch, "refs/heads/fix")
	push.RepositoryID = repoID
	seedConversationAction(t, s, conversationID, push)
	seedConversationAction(t, s, conversationID, push)
	seedConversationAction(t, s, conversationID, artifactRow(domain.ActionCommentPosted, domain.ObjectKindComment,
		domain.GitHubCommentKey(domain.GitHubIssueComment, 12)))
	seedConversationAction(t, s, conversationID, artifactRow(domain.ActionGitDenied, "", ""))
	rawBranchID, _ := domain.ObjectID(push)
	branchID := wireObjectID(rawBranchID)

	list := func(body map[string]any) []map[string]any {
		t.Helper()
		page := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost,
			"/api/agent/conversations/"+conversationID+"/actions/list", body))
		if page.Total() != len(page.Items) {
			t.Fatalf("total %d for %d items", page.Total(), len(page.Items))
		}
		return page.Items
	}

	byObject := list(map[string]any{"object_id": branchID})
	if len(byObject) != 2 {
		t.Fatalf("object filter returned %d rows, want 2", len(byObject))
	}
	for _, m := range byObject {
		if m["object_id"] != branchID || m["object_kind"] != domain.ObjectKindBranch || m["performed_by"] != domain.PerformedByAgent {
			t.Errorf("object-filtered row = %v", m)
		}
	}

	refused := list(map[string]any{"refused": true})
	if len(refused) != 1 || refused[0]["action"] != domain.ActionGitDenied {
		t.Fatalf("refused filter = %v, want the git_denied row alone", refused)
	}
	for _, key := range []string{"object_kind", "object_id"} {
		if _, present := refused[0][key]; present {
			t.Errorf("a refusal carries %q = %v, want it omitted", key, refused[0][key])
		}
	}

	if both := list(map[string]any{"object_id": branchID, "refused": true}); len(both) != 0 {
		t.Errorf("both filters = %v, want nothing", both)
	}
	if all := list(map[string]any{"refused": false}); len(all) != 4 {
		t.Errorf("refused:false returned %d rows, want all 4", len(all))
	}
}

// TestHandleAgentActions_StrictBody pins that the action list's body is
// decoded strictly: an unknown field, a non-boolean refused and an object_id
// of no object id's shape are each a 400 naming the field.
func TestHandleAgentActions_StrictBody(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "actstrict", dbtest.SeedConcluded)
	path := "/api/agent/conversations/" + conversationID + "/actions/list"
	cases := []struct {
		name   string
		body   map[string]any
		reason string
		field  string
	}{
		{"unknown field", map[string]any{"object": "branch|x|y"}, httpx.ReasonUnknownField, "object"},
		{"non-boolean refused", map[string]any{"refused": "yes"}, httpx.ReasonInvalidField, "refused"},
		{"malformed object_id", map[string]any{"object_id": "not-an-object"}, httpx.ReasonInvalidField, "object_id"},
		{"raw, unencoded object_id", map[string]any{"object_id": "branch|repo|refs/heads/fix"}, httpx.ReasonInvalidField, "object_id"},
		{"padded object_id", map[string]any{"object_id": base64.URLEncoding.EncodeToString([]byte("branch|repo|refs/heads/fix"))}, httpx.ReasonInvalidField, "object_id"},
		{"encoded unknown object kind", map[string]any{"object_id": wireObjectID("widget|x")}, httpx.ReasonInvalidField, "object_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertOneFault(t, doJSON(t, s, http.MethodPost, path, c.body), http.StatusBadRequest, c.reason, c.field)
		})
	}
}

// TestConversationResponse_ArtifactCounts pins artifact_count and
// artifact_counts on the single read and the batched list: the agent's rows
// counted once per object, a person's row not at all, and {} for a
// conversation that changed nothing.
func TestConversationResponse_ArtifactCounts(t *testing.T) {
	s := newTestServer(t)
	busy := seedSteerConversation(t, s.db, "artcnt", dbtest.SeedConcluded)
	idle := seedSteerConversation(t, s.db, "artidle", dbtest.SeedConcluded)
	entityID := fixtureUUID("e_artcnt")
	push := artifactRow(domain.ActionBranchPushed, domain.ObjectKindBranch, "refs/heads/fix")
	push.RepositoryID = repositoryIDOfEntity(t, s, entityID)
	seedConversationAction(t, s, busy, push)
	seedConversationAction(t, s, busy, push)
	created := artifactRow(domain.ActionPRCreated, domain.ObjectKindPullRequest, "")
	created.EntityID = entityID
	seedConversationAction(t, s, busy, created)
	// A person's edit of the same pull request names it too, and is not the
	// agent's.
	edited := created
	edited.Action, edited.PerformedBy = domain.ActionPREdited, domain.PerformedByPerson
	seedConversationAction(t, s, busy, edited)
	// Neither is a person's edit of an object the agent never touched.
	other := artifactRow(domain.ActionLabelDefinitionEdited, domain.ObjectKindLabel, "bug")
	other.RepositoryID, other.PerformedBy = push.RepositoryID, domain.PerformedByPerson
	seedConversationAction(t, s, busy, other)

	check := func(name string, m map[string]any, total float64, byKind map[string]float64) {
		t.Helper()
		if m["artifact_count"] != total {
			t.Errorf("%s artifact_count = %v, want %v", name, m["artifact_count"], total)
		}
		counts, ok := m["artifact_counts"].(map[string]any)
		if !ok || len(counts) != len(byKind) {
			t.Errorf("%s artifact_counts = %v, want %v", name, m["artifact_counts"], byKind)
			return
		}
		for kind, n := range byKind {
			if counts[kind] != n {
				t.Errorf("%s artifact_counts[%s] = %v, want %v", name, kind, counts[kind], n)
			}
		}
	}
	get := func(id string) map[string]any {
		t.Helper()
		rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET conversation = %d; body=%s", rec.Code, rec.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	want := map[string]float64{domain.ObjectKindBranch: 1, domain.ObjectKindPullRequest: 1}
	check("GET busy", get(busy), 2, want)
	idleResp := get(idle)
	check("GET idle", idleResp, 0, map[string]float64{})

	convs := listConversations(t, s, fixtureUUID("t_artcnt"))
	if len(convs) != 1 {
		t.Fatalf("listed %d conversations, want 1", len(convs))
	}
	check("list busy", convs[0], 2, want)
}

// TestWireObjectID pins the object id's wire spelling: it round-trips, it
// carries none of the characters a raw id has that a URL path segment
// cannot, and a non-canonical or non-id spelling is refused.
func TestWireObjectID(t *testing.T) {
	for _, raw := range []string{
		"branch|" + fixtureUUID("repo") + "|refs/heads/feature/a+b",
		"pull_request|" + fixtureUUID("pr"),
		"comment|https://github.com|github:comment:review:555",
	} {
		wire := wireObjectID(raw)
		if strings.ContainsAny(wire, "/|=+") {
			t.Errorf("wireObjectID(%q) = %q, want no '/', '|', '=' or '+'", raw, wire)
		}
		if got, ok := parseWireObjectID(wire); !ok || got != raw {
			t.Errorf("parseWireObjectID(%q) = (%q, %v), want (%q, true)", wire, got, ok, raw)
		}
	}
	if wireObjectID("") != "" {
		t.Errorf("wireObjectID(\"\") = %q, want \"\"", wireObjectID(""))
	}
	// A 14-byte id leaves two zero bits in its last character. Bumping that
	// character sets one, which a lenient decoder would still read as the
	// same id; only the canonical spelling is accepted.
	canonical := wireObjectID("pull_request|x")
	noncanonical := canonical[:len(canonical)-1] + string(canonical[len(canonical)-1]+1)
	if _, ok := parseWireObjectID(noncanonical); ok {
		t.Errorf("parseWireObjectID accepted the non-canonical %q for %q", noncanonical, canonical)
	}
	if lenient, err := base64.RawURLEncoding.DecodeString(noncanonical); err != nil || string(lenient) != "pull_request|x" {
		t.Fatalf("fixture: a lenient decode of %q = (%q, %v), want the same id", noncanonical, lenient, err)
	}
	for _, bad := range []string{"", "pull_request|x", "%%%", wireObjectID("pull_request|")} {
		if _, ok := parseWireObjectID(bad); ok {
			t.Errorf("parseWireObjectID(%q) accepted it", bad)
		}
	}
}
