package deliverableteardown

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
)

// fakeStores is an in-memory teardown surface: one task, its conversations, and
// the deliverables hanging off each, plus the entities and registry rows a linked
// deliverable's pull request resolves through. Writes land where a caller can read
// them back.
type fakeStores struct {
	convs        []domain.Conversation
	deliverables map[string][]domain.Deliverable // conversation id → deliverables
	entities     map[string]domain.Entity        // entity id → entity
	repositories map[string]domain.Repository    // registry row id → row
	upserted     []domain.Deliverable
	recorded     []domain.ExternalAction
	listErr      error
	upsertErr    error
}

func (f *fakeStores) ConversationsForTask(context.Context, string, string) ([]domain.Conversation, error) {
	return f.convs, f.listErr
}

func (f *fakeStores) DeliverablesForConversation(_ context.Context, _, conversationID string) ([]domain.Deliverable, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.deliverables[conversationID], nil
}

func (f *fakeStores) UpsertDeliverable(_ context.Context, _ string, a domain.Deliverable) error {
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

func (f *fakeStores) Entity(_ context.Context, _, id string) (*domain.Entity, error) {
	if e, ok := f.entities[id]; ok {
		return &e, nil
	}
	return nil, nil
}

func (f *fakeStores) Repository(_ context.Context, _, id string) (*domain.Repository, error) {
	if r, ok := f.repositories[id]; ok {
		return &r, nil
	}
	return nil, fmt.Errorf("%w: %s", db.ErrNoSuchRepository, id)
}

// fakeDeps runs every batch against one fakeStores and hands out one resolver.
// It counts batches, because the split between the credential pre-pass and the
// write pass is what keeps a GitHub probe out of a write transaction.
type fakeDeps struct {
	stores      *fakeStores
	resolver    ghclient.Resolver
	batches     int
	githubReads int
	// settled lists the deliverables SettlePullRequest was asked to settle;
	// settleVerifies has it settle each one verified in stores, as the
	// reconciler does for a run's own pull request.
	settled        []string
	settleVerifies bool
}

func (d *fakeDeps) SettlePullRequest(_ context.Context, _ string, a domain.Deliverable) {
	d.settled = append(d.settled, a.ID)
	if !d.settleVerifies {
		return
	}
	for _, arts := range d.stores.deliverables {
		for i := range arts {
			if arts[i].ID == a.ID {
				arts[i].Verification = domain.DeliverableVerificationVerified
			}
		}
	}
}

func (d *fakeDeps) Batch(_ context.Context, _ string, fn func(Stores) error) error {
	d.batches++
	return fn(d.stores)
}

func (d *fakeDeps) GitHub() ghclient.Resolver {
	d.githubReads++
	return d.resolver
}

// oneClientResolver answers every repo with the same client. Only the arm the
// teardown uses answers; the rest panic so a test that starts depending on them
// says so.
type oneClientResolver struct{ client *ghclient.Client }

func (r oneClientResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	panic("ClientFor is not used by the deliverable teardown")
}

func (r oneClientResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	panic("ClientForInstallation is not used by the deliverable teardown")
}

func (r oneClientResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r oneClientResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	panic("TokenFor is not used by the deliverable teardown")
}

func (r oneClientResolver) BaseURLFor(context.Context, string) (string, error) {
	panic("BaseURLFor is not used by the deliverable teardown")
}

func (r oneClientResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	panic("OrgIdentityFor is not used by the deliverable teardown")
}

// draftPR and stagedReview are the two unresolved shapes a stopped run leaves.
func draftPR(conversationID string) domain.Deliverable {
	return draftPRIn(conversationID, "owner/repo", 7, "art_pr")
}

// draftPRIn is draftPR addressed at a named repo and number, for the tests that
// need several — the credential classification is keyed per repo and the closes
// are per deliverable, so "once" only means something across more than one of each.
func draftPRIn(conversationID, repoPath string, number int, id string) domain.Deliverable {
	a := domain.NewPullRequestDeliverable(repoPath, number, fmt.Sprintf("PR_%s_%d", repoPath, number), "tf/fix", "main",
		"https://example.test/"+repoPath+"/pull/7", "Proposed title", "Proposed body", true)
	a.ID = id
	a.ConversationID = conversationID
	a.TeamID = "team"
	a.Verification = domain.DeliverableVerificationVerified
	return a
}

func stagedReview(conversationID string) domain.Deliverable {
	a := domain.NewReviewDeliverable("owner/repo", 7, "PR_owner/repo_7", "headsha", conversationID)
	a.ID = "art_review"
	a.ConversationID = conversationID
	a.TeamID = "team"
	a.Verification = domain.DeliverableVerificationVerified
	rd, _ := domain.ParseReviewDeliverableDetails(a.DetailsJSON)
	rd.ReviewBody = "agent draft body"
	rd.ReviewEvent = "COMMENT"
	a.DetailsJSON = domain.MarshalReviewDeliverableDetails(rd)
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
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {draftPR("conv"), stagedReview("conv")}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	states := map[string]string{}
	for _, a := range stores.upserted {
		states[a.ID] = a.State
	}
	if states["art_pr"] != domain.DeliverableStatePRClosed {
		t.Errorf("draft PR flipped to %q, want %q", states["art_pr"], domain.DeliverableStatePRClosed)
	}
	if states["art_review"] != domain.DeliverableStateReviewDismissed {
		t.Errorf("staged review flipped to %q, want %q", states["art_review"], domain.DeliverableStateReviewDismissed)
	}
	if *closes != 1 {
		t.Errorf("PR closes on GitHub = %d, want 1", *closes)
	}
	if len(deps.settled) != 0 {
		t.Errorf("settled = %v, want none: the draft was already verified", deps.settled)
	}
	if len(stores.recorded) != 1 {
		t.Fatalf("audit rows = %d, want 1 — only the PR close is an org-credential write", len(stores.recorded))
	}
	act := stores.recorded[0]
	if act.Action != domain.ActionPRClosed || act.ActorUserID != "user-1" || act.PerformedBy != domain.PerformedByPerson {
		t.Errorf("audit row = (action %q, actor %q, performed by %q), want (pr_closed, user-1, person)", act.Action, act.ActorUserID, act.PerformedBy)
	}
	if act.FromState != domain.DeliverableStatePRDraft || act.ToState != domain.DeliverableStatePRClosed {
		t.Errorf("audit transition = %q → %q, want draft → closed", act.FromState, act.ToState)
	}
	// One batch reads what the task holds, the other writes: the credential
	// probe between them can reach GitHub and must not run inside a write tx.
	if deps.batches != 2 {
		t.Errorf("batches = %d, want 2 (the credential pre-read, then the writes)", deps.batches)
	}
}

// TestTeardown_SettlesAPendingDraftBeforeClosingIt: a draft not yet
// confirmed as its run's is settled before the write batch, and one the
// settle confirms is closed on GitHub like any other, rather than left open
// there behind a row TF closed.
func TestTeardown_SettlesAPendingDraftBeforeClosingIt(t *testing.T) {
	client, closes := closingGitHub(t)
	pending := draftPR("conv")
	pending.Verification = domain.DeliverableVerificationPending
	stores := &fakeStores{
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {pending, stagedReview("conv")}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}, settleVerifies: true}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(deps.settled) != 1 || deps.settled[0] != "art_pr" {
		t.Errorf("settled = %v, want the pending draft alone", deps.settled)
	}
	if *closes != 1 || len(stores.recorded) != 1 {
		t.Errorf("GitHub closes = %d, audit rows = %d, want one each for the draft the settle confirmed", *closes, len(stores.recorded))
	}
}

// TestTeardown_ClosesAnUnverifiedDraftTFSideOnly: a draft its settle leaves
// unconfirmed as its run's is flipped to closed with the rest, but its pull
// request is not closed on GitHub and no org-credential write is audited,
// since the pull request may be someone else's.
func TestTeardown_ClosesAnUnverifiedDraftTFSideOnly(t *testing.T) {
	client, closes := closingGitHub(t)
	pending := draftPR("conv")
	pending.Verification = domain.DeliverableVerificationPending
	stores := &fakeStores{
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {pending}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(stores.upserted) != 1 || stores.upserted[0].State != domain.DeliverableStatePRClosed {
		t.Errorf("upserts = %+v, want the draft flipped to closed", stores.upserted)
	}
	if *closes != 0 || len(stores.recorded) != 0 {
		t.Errorf("GitHub closes = %d, audit rows = %d, want neither for an unverified draft", *closes, len(stores.recorded))
	}
	if len(deps.settled) != 1 {
		t.Errorf("settled = %v, want the draft settled before it was closed", deps.settled)
	}
}

// TestTeardown_AutonomousCloseNamesNoActor pins the System adapter's spelling:
// an empty actor is what an event-driven close records, and it must survive the
// row builder rather than being filled in with something. The row names TF as
// its performer.
func TestTeardown_AutonomousCloseNamesNoActor(t *testing.T) {
	client, _ := closingGitHub(t)
	stores := &fakeStores{
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {draftPR("conv")}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedBySystem, ""); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(stores.recorded) != 1 || stores.recorded[0].ActorUserID != "" || stores.recorded[0].PerformedBy != domain.PerformedBySystem {
		t.Fatalf("audit rows = %+v, want exactly one naming no actor, performed by the system", stores.recorded)
	}
}

// TestTeardown_ResolvedDeliverablesAreLeftAlone: the teardown resolves what is
// unresolved. An already-closed PR and an already-dismissed review are written
// nowhere and closed nowhere, which is what makes a retry after a partial pass
// safe.
func TestTeardown_ResolvedDeliverablesAreLeftAlone(t *testing.T) {
	client, closes := closingGitHub(t)
	closedPR := draftPR("conv")
	closedPR.State = domain.DeliverableStatePRClosed
	dismissed := stagedReview("conv")
	dismissed.State = domain.DeliverableStateReviewDismissed
	stores := &fakeStores{
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {closedPR, dismissed}},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err != nil {
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
// whose deliverable still says draft.
func TestTeardown_BatchFailureClosesNothingOnGitHub(t *testing.T) {
	client, closes := closingGitHub(t)
	stores := &fakeStores{
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {draftPR("conv")}},
		upsertErr:    errors.New("boom: the write rolled back"),
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err == nil {
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
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {draftPR("conv")}},
	}
	deps := &fakeDeps{stores: stores}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(stores.upserted) != 1 || stores.upserted[0].State != domain.DeliverableStatePRClosed {
		t.Errorf("upserts = %+v, want the draft PR flipped to closed", stores.upserted)
	}
	if len(stores.recorded) != 1 || stores.recorded[0].Credential != domain.CredentialGitHubApp {
		t.Errorf("audit rows = %+v, want one recording the app fallback", stores.recorded)
	}
}

// TestTeardown_ReadsTheResolverOnce is the consistency contract, not a
// micro-optimization. The credential an audit row names and the client the
// close that row describes is made through have to be the same resolver: an
// adapter's is swapped under a running teardown on a GitHub config change, so a
// re-read per repo or per deliverable could file a row asserting a tier no close
// used. Three PRs across two repos, and the resolver is still read once.
func TestTeardown_ReadsTheResolverOnce(t *testing.T) {
	client, closes := closingGitHub(t)
	stores := &fakeStores{
		convs: []domain.Conversation{{ID: "conv-a"}, {ID: "conv-b"}},
		deliverables: map[string][]domain.Deliverable{
			"conv-a": {draftPRIn("conv-a", "owner/one", 7, "art_1"), draftPRIn("conv-a", "owner/two", 8, "art_2")},
			"conv-b": {draftPRIn("conv-b", "owner/two", 9, "art_3"), stagedReview("conv-b")},
		},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: client}}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if deps.githubReads != 1 {
		t.Errorf("GitHub() reads = %d, want 1 — both halves must see one resolver", deps.githubReads)
	}
	if *closes != 3 {
		t.Errorf("PR closes on GitHub = %d, want 3", *closes)
	}
	if len(stores.recorded) != 3 {
		t.Errorf("audit rows = %d, want one per closed PR", len(stores.recorded))
	}
}

// TestCredentialFor pins the map lookup, including the miss a draft PR opened
// between the pre-pass and the write batch would produce.
func TestCredentialFor(t *testing.T) {
	credentials := map[string]string{"art_1": domain.CredentialGitHubPAT}
	if got := credentialFor(credentials, "art_1"); got != domain.CredentialGitHubPAT {
		t.Errorf("hit = %q, want github_pat", got)
	}
	if got := credentialFor(credentials, "art_2"); got != domain.CredentialGitHubApp {
		t.Errorf("miss = %q, want the app fallback", got)
	}
}

// TestTeardown_ClosesThePullRequestItsEntityNames: a draft PR's pull request is
// the one its entity is about, in the repository that entity's registry row
// names now. A row renamed since the draft was written is closed under its new
// name, whatever the deliverable's target still spells; a retired row's pull
// request is not closed at all, since GitHub serves the name as another
// repository now. Both are still flipped and audited.
func TestTeardown_ClosesThePullRequestItsEntityNames(t *testing.T) {
	var paths []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
	}))
	t.Cleanup(stub.Close)

	renamed := draftPRIn("conv", "owner/old-name", 7, "art_renamed")
	renamed.EntityID = "ent-renamed"
	retired := draftPRIn("conv", "owner/gone", 9, "art_retired")
	retired.EntityID = "ent-retired"
	retiredAt := time.Now()
	stores := &fakeStores{
		convs:        []domain.Conversation{{ID: "conv"}},
		deliverables: map[string][]domain.Deliverable{"conv": {renamed, retired}},
		entities: map[string]domain.Entity{
			"ent-renamed": {ID: "ent-renamed", SourceID: "owner/old-name#7", RepositoryID: "repo-renamed"},
			"ent-retired": {ID: "ent-retired", SourceID: "owner/gone#9", RepositoryID: "repo-retired"},
		},
		repositories: map[string]domain.Repository{
			"repo-renamed": {ID: "repo-renamed", Owner: "owner", Repo: "new-name"},
			"repo-retired": {ID: "repo-retired", Owner: "owner", Repo: "gone", RetiredAt: &retiredAt},
		},
	}
	deps := &fakeDeps{stores: stores, resolver: oneClientResolver{client: ghclient.NewClient(stub.URL, "pat")}}

	if err := Teardown(context.Background(), deps, "org", "task", domain.PerformedByPerson, "user-1"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/repos/owner/new-name/pulls/7") {
		t.Errorf("GitHub requests = %v, want the one close of owner/new-name#7", paths)
	}
	if len(stores.upserted) != 2 || len(stores.recorded) != 2 {
		t.Errorf("upserts=%d audit=%d, want both drafts flipped and audited", len(stores.upserted), len(stores.recorded))
	}
}
