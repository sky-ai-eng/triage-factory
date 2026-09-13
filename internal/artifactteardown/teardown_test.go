package artifactteardown

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// fakeStores is an in-memory teardown surface: one task, its conversations, and
// the artifacts hanging off each. Writes land where a caller can read them back.
type fakeStores struct {
	convs     []domain.Conversation
	artifacts map[string][]domain.Artifact // conversation id → artifacts
	upserted  []domain.Artifact
	recorded  []domain.ExternalAction
	listErr   error
	upsertErr error
}

func (f *fakeStores) ConversationsForTask(context.Context, string, string) ([]domain.Conversation, error) {
	return f.convs, f.listErr
}

func (f *fakeStores) ArtifactsForConversation(_ context.Context, _, conversationID string) ([]domain.Artifact, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.artifacts[conversationID], nil
}

func (f *fakeStores) UpsertArtifact(_ context.Context, _ string, a domain.Artifact) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserted = append(f.upserted, a)
	return nil
}

func (f *fakeStores) RecordExternalAction(_ context.Context, _ string, act domain.ExternalAction) error {
	f.recorded = append(f.recorded, act)
	return nil
}

// fakeDeps runs every batch against one fakeStores and hands out one resolver.
// It counts batches, because the split between the credential pre-pass and the
// write pass is what keeps a GitHub probe out of a write transaction.
type fakeDeps struct {
	stores   *fakeStores
	resolver ghclient.Resolver
	batches  int
}

func (d *fakeDeps) Batch(_ context.Context, _ string, fn func(Stores) error) error {
	d.batches++
	return fn(d.stores)
}

func (d *fakeDeps) GitHub() ghclient.Resolver { return d.resolver }

// oneClientResolver answers every repo with the same client. Only the arm the
// teardown uses answers; the rest panic so a test that starts depending on them
// says so.
type oneClientResolver struct{ client *ghclient.Client }

func (r oneClientResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	panic("ClientFor is not used by the artifact teardown")
}

func (r oneClientResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r oneClientResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	panic("TokenFor is not used by the artifact teardown")
}

func (r oneClientResolver) BaseURLFor(context.Context, string) (string, error) {
	panic("BaseURLFor is not used by the artifact teardown")
}

func (r oneClientResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	panic("OrgIdentityFor is not used by the artifact teardown")
}

// draftPR and stagedReview are the two unresolved shapes a stopped run leaves.
func draftPR(conversationID string) domain.Artifact {
	a := domain.NewPullRequestArtifact("owner/repo", 7, "PR_node", "tf/fix", "main",
		"https://example.test/owner/repo/pull/7", "Proposed title", "Proposed body", true)
	a.ID = "art_pr"
	a.ConversationID = conversationID
	a.TeamID = "team"
	return a
}

func stagedReview(conversationID string) domain.Artifact {
	a := domain.NewReviewArtifact("owner/repo", 7, "headsha", conversationID)
	a.ID = "art_review"
	a.ConversationID = conversationID
	a.TeamID = "team"
	rd, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
	rd.ReviewBody = "agent draft body"
	rd.ReviewEvent = "COMMENT"
	a.DetailsJSON = domain.MarshalReviewArtifactDetails(rd)
	return a
}

// closingGitHub answers the PR-close PATCH and records how many it got.
func closingGitHub(t *testing.T) (*ghclient.Client, *int) {
	t.Helper()
	var closes int
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("unexpected %s %s — the teardown closes PRs and touches nothing else", r.Method, r.URL.Path)
		}
		closes++
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
	}))
	t.Cleanup(stub.Close)
	return ghclient.NewClient(stub.URL, "pat"), &closes
}

// TestTeardown_ResolvesBothKinds is the shape of the whole pass: the draft PR
// flips to closed AND is closed on GitHub with an audited org-credential write;
// the staged review flips to dismissed with neither, because it was never on
// GitHub to begin with.
func TestTeardown_ResolvesBothKinds(t *testing.T) {
	client, closes := closingGitHub(t)
	stores := &fakeStores{
		convs:     []domain.Conversation{{ID: "conv"}},
		artifacts: map[string][]domain.Artifact{"conv": {draftPR("conv"), stagedReview("conv")}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	states := map[string]string{}
	for _, a := range stores.upserted {
		states[a.ID] = a.State
	}
	if states["art_pr"] != domain.ArtifactStatePRClosed {
		t.Errorf("draft PR flipped to %q, want %q", states["art_pr"], domain.ArtifactStatePRClosed)
	}
	if states["art_review"] != domain.ArtifactStateReviewDismissed {
		t.Errorf("staged review flipped to %q, want %q", states["art_review"], domain.ArtifactStateReviewDismissed)
	}
	if *closes != 1 {
		t.Errorf("PR closes on GitHub = %d, want 1", *closes)
	}
	if len(stores.recorded) != 1 {
		t.Fatalf("audit rows = %d, want 1 — only the PR close is an org-credential write", len(stores.recorded))
	}
	act := stores.recorded[0]
	if act.Action != domain.ActionPRClosed || act.ActorUserID != "user-1" {
		t.Errorf("audit row = (action %q, actor %q), want (pr_closed, user-1)", act.Action, act.ActorUserID)
	}
	if act.FromState != domain.ArtifactStatePRDraft || act.ToState != domain.ArtifactStatePRClosed {
		t.Errorf("audit transition = %q → %q, want draft → closed", act.FromState, act.ToState)
	}
	// One batch reads what the task holds, the other writes: the credential
	// probe between them can reach GitHub and must not run inside a write tx.
	if deps.batches != 2 {
		t.Errorf("batches = %d, want 2 (the credential pre-read, then the writes)", deps.batches)
	}
}

// TestTeardown_AutonomousCloseNamesNoActor pins the System adapter's spelling:
// an empty actor is what an event-driven close records, and it must survive the
// row builder rather than being filled in with something.
func TestTeardown_AutonomousCloseNamesNoActor(t *testing.T) {
	client, _ := closingGitHub(t)
	stores := &fakeStores{
		convs:     []domain.Conversation{{ID: "conv"}},
		artifacts: map[string][]domain.Artifact{"conv": {draftPR("conv")}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", ""); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(stores.recorded) != 1 || stores.recorded[0].ActorUserID != "" {
		t.Fatalf("audit rows = %+v, want exactly one naming no actor", stores.recorded)
	}
}

// TestTeardown_ResolvedArtifactsAreLeftAlone: the teardown resolves what is
// unresolved. An already-closed PR and an already-dismissed review are written
// nowhere and closed nowhere, which is what makes a retry after a partial pass
// safe.
func TestTeardown_ResolvedArtifactsAreLeftAlone(t *testing.T) {
	client, closes := closingGitHub(t)
	closedPR := draftPR("conv")
	closedPR.State = domain.ArtifactStatePRClosed
	dismissed := stagedReview("conv")
	dismissed.State = domain.ArtifactStateReviewDismissed
	stores := &fakeStores{
		convs:     []domain.Conversation{{ID: "conv"}},
		artifacts: map[string][]domain.Artifact{"conv": {closedPR, dismissed}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(stores.upserted) != 0 || len(stores.recorded) != 0 || *closes != 0 {
		t.Errorf("upserts=%d audit=%d closes=%d, want 0/0/0 — nothing was unresolved",
			len(stores.upserted), len(stores.recorded), *closes)
	}
}

// TestTeardown_BatchFailureClosesNothingOnGitHub is the ordering contract: the
// GitHub closes run only for PRs the batch actually flipped, so a batch that
// failed leaves the PR open for the retry to find rather than closing a PR
// whose artifact still says draft.
func TestTeardown_BatchFailureClosesNothingOnGitHub(t *testing.T) {
	client, closes := closingGitHub(t)
	stores := &fakeStores{
		convs:     []domain.Conversation{{ID: "conv"}},
		artifacts: map[string][]domain.Artifact{"conv": {draftPR("conv")}},
		upsertErr: errors.New("boom: the write rolled back"),
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", "user-1"); err == nil {
		t.Fatal("Teardown returned nil; a failed batch must report so the caller can retry")
	}
	if *closes != 0 {
		t.Errorf("PR closes on GitHub = %d, want 0 — nothing was flipped", *closes)
	}
}

// TestTeardown_NoGitHubResolverStillFlips: the flips are the durable half. A
// deployment with no resolver wired loses the outward close (reconciliation
// retires the PR later) but must not lose the state change.
func TestTeardown_NoGitHubResolverStillFlips(t *testing.T) {
	stores := &fakeStores{
		convs:     []domain.Conversation{{ID: "conv"}},
		artifacts: map[string][]domain.Artifact{"conv": {draftPR("conv")}},
	}
	deps := &fakeDeps{stores: stores}

	if err := Teardown(context.Background(), deps, "org", "task", "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(stores.upserted) != 1 || stores.upserted[0].State != domain.ArtifactStatePRClosed {
		t.Errorf("upserts = %+v, want the draft PR flipped to closed", stores.upserted)
	}
	if len(stores.recorded) != 1 || stores.recorded[0].Credential != domain.CredentialGitHubApp {
		t.Errorf("audit rows = %+v, want one recording the app fallback", stores.recorded)
	}
}

// TestCredentialForTarget pins the map lookup, including the miss a draft PR
// opened between the pre-pass and the write batch would produce.
func TestCredentialForTarget(t *testing.T) {
	credentials := map[string]string{"octo/repo": domain.CredentialGitHubPAT}
	if got := credentialForTarget(credentials, "octo/repo#3"); got != domain.CredentialGitHubPAT {
		t.Errorf("hit = %q, want github_pat", got)
	}
	if got := credentialForTarget(credentials, "other/repo#3"); got != domain.CredentialGitHubApp {
		t.Errorf("miss = %q, want the app fallback", got)
	}
	if got := credentialForTarget(credentials, "garbage"); got != domain.CredentialGitHubApp {
		t.Errorf("unparseable target = %q, want the app fallback", got)
	}
}
