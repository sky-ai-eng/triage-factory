package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The bot accounts two Apps act as, and a person's account.
const (
	botA      = "41898282"
	botALogin = "acme-bot[bot]"
	botB      = "29110"
	person    = "583231"
)

var attrT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// engagement is one claim of a conversation from `from` until `to` (nil for a
// live one), whose seal recorded the bot account (id, login) on its github
// entry; both empty for a seal that recorded none.
func engagement(id, login string, from time.Time, to *time.Time) domain.ExecutorClaim {
	return domain.ExecutorClaim{
		ID:         fmt.Sprintf("claim-%s-%d", id, from.Unix()),
		ClaimedAt:  from,
		ReleasedAt: to,
		Sources: domain.SourceManifest{eventsource.KindGitHub: {
			State: domain.SourceSealed, AccountID: id, AccountLogin: login,
		}},
	}
}

func at(d time.Duration) *time.Time {
	t := attrT0.Add(d)
	return &t
}

func TestJudgeSealedAuthor(t *testing.T) {
	unsealed := engagement("", "", attrT0, at(time.Hour))
	for _, tc := range []struct {
		name          string
		claims        []domain.ExecutorClaim
		author, login string
		created       time.Time
		want          verdict
		wantReason    string
	}{
		{
			name:   "the live engagement's account authored it",
			claims: []domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)},
			author: botA, login: botALogin, created: attrT0.Add(10 * time.Minute),
			want: verdictRun,
		},
		{
			name:   "a person authored it while the run was live",
			claims: []domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)},
			author: person, login: "octocat", created: attrT0.Add(10 * time.Minute),
			want: verdictForeign, wantReason: foreignAuthor,
		},
		{
			name:   "the ids decide when both sides have one, whatever the logins say",
			claims: []domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)},
			author: person, login: botALogin, created: attrT0.Add(10 * time.Minute),
			want: verdictForeign, wantReason: foreignAuthor,
		},
		{
			name:   "a seal with no bot id is matched by the bot's login",
			claims: []domain.ExecutorClaim{engagement("", botALogin, attrT0, nil)},
			author: botA, login: "Acme-Bot[bot]", created: attrT0.Add(10 * time.Minute),
			want: verdictRun,
		},
		{
			name:   "a seal with no bot id and another author's login",
			claims: []domain.ExecutorClaim{engagement("", botALogin, attrT0, nil)},
			author: person, login: "octocat", created: attrT0.Add(10 * time.Minute),
			want: verdictForeign, wantReason: foreignAuthor,
		},
		{
			name:   "opened before the conversation was ever claimed",
			claims: []domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)},
			author: botA, login: botALogin, created: attrT0.Add(-time.Hour),
			want: verdictForeign, wantReason: foreignNoEngagement,
		},
		{
			name:   "opened after its only engagement was released",
			claims: []domain.ExecutorClaim{engagement(botA, botALogin, attrT0, at(time.Hour))},
			author: botA, login: botALogin, created: attrT0.Add(2 * time.Hour),
			want: verdictForeign, wantReason: foreignNoEngagement,
		},
		{
			name:   "no engagements at all",
			author: botA, login: botALogin, created: attrT0,
			want: verdictForeign, wantReason: foreignNoEngagement,
		},
		{
			name:   "GitHub's clock a minute behind the claim",
			claims: []domain.ExecutorClaim{engagement(botA, botALogin, attrT0, at(time.Hour))},
			author: botA, login: botALogin, created: attrT0.Add(-time.Minute),
			want: verdictRun,
		},
		{
			name:   "GitHub's clock a minute past the release",
			claims: []domain.ExecutorClaim{engagement(botA, botALogin, attrT0, at(time.Hour))},
			author: botA, login: botALogin, created: attrT0.Add(time.Hour + time.Minute),
			want: verdictRun,
		},
		{
			name:   "the live engagement sealed no account",
			claims: []domain.ExecutorClaim{unsealed},
			author: botA, login: botALogin, created: attrT0.Add(10 * time.Minute),
			want: verdictUnknown,
		},
		{
			name: "a handover: one engagement unsealed, the other the author's",
			claims: []domain.ExecutorClaim{
				func() domain.ExecutorClaim { c := unsealed; c.ReleasedAt = at(time.Minute); return c }(),
				engagement(botA, botALogin, attrT0.Add(time.Minute), nil),
			},
			author: botA, login: botALogin, created: attrT0.Add(time.Minute),
			want: verdictRun,
		},
		{
			name: "an earlier engagement's App authored it after the org bound another",
			claims: []domain.ExecutorClaim{
				engagement(botA, botALogin, attrT0, at(time.Hour)),
				engagement(botB, "other-bot[bot]", attrT0.Add(2*time.Hour), nil),
			},
			author: botA, login: botALogin, created: attrT0.Add(30 * time.Minute),
			want: verdictRun,
		},
		{
			name: "the later engagement's App did not author one opened under the earlier",
			claims: []domain.ExecutorClaim{
				engagement(botA, botALogin, attrT0, at(time.Hour)),
				engagement(botB, "other-bot[bot]", attrT0.Add(2*time.Hour), nil),
			},
			author: botB, login: "other-bot[bot]", created: attrT0.Add(30 * time.Minute),
			want: verdictForeign, wantReason: foreignAuthor,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := judgeSealedAuthor(tc.claims, tc.author, tc.login, tc.created)
			if got != tc.want || reason != tc.wantReason {
				t.Errorf("judgeSealedAuthor = (%v, %q), want (%v, %q)", got, reason, tc.want, tc.wantReason)
			}
		})
	}
}

// fakeEngagements answers ClaimsForConversationSystem from claims, keyed by
// conversation, and leaves every other queue method to the real store.
type fakeEngagements struct {
	db.ConversationQueueStore
	claims map[string][]domain.ExecutorClaim
}

func (f fakeEngagements) ClaimsForConversationSystem(_ context.Context, _, conversationID string) ([]domain.ExecutorClaim, error) {
	return f.claims[conversationID], nil
}

// flakyCommissioner fails its first commissioned-by stamp, and leaves every
// other entity method to the real store.
type flakyCommissioner struct {
	db.EntityStore
	failures int
}

func (f *flakyCommissioner) StampCommissionedByIfUnsetSystem(ctx context.Context, orgID, entityID, userID string) (bool, error) {
	if f.failures > 0 {
		f.failures--
		return false, errors.New("connection reset")
	}
	return f.EntityStore.StampCommissionedByIfUnsetSystem(ctx, orgID, entityID, userID)
}

// authoredPRNode is an open draft pull request node as RefreshPRs' fragment
// returns it, PR_<number> in octo/repo on head feat-<number>, opened at
// created by the account authorID (the acme bot when bot is set).
func authoredPRNode(number int, authorID string, bot bool, created time.Time) string {
	typename, login := "User", "octocat"
	if bot {
		typename, login = "Bot", "acme-bot"
	}
	id, _ := json.Number(authorID).Int64()
	b, _ := json.Marshal(map[string]any{
		"id": fmt.Sprintf("PR_%d", number), "number": number, "title": "t", "body": "b",
		"state": "OPEN", "isDraft": true, "merged": false,
		"headRefName": fmt.Sprintf("feat-%d", number), "baseRefName": "main",
		"url":        fmt.Sprintf("https://github.com/octo/repo/pull/%d", number),
		"repository": map[string]any{"nameWithOwner": "octo/repo"},
		"author":     map[string]any{"__typename": typename, "login": login, "databaseId": id},
		"createdAt":  created.Format(time.RFC3339),
	})
	return string(b)
}

// attributionRig is a reconciler under SealedAuthor over one conversation
// whose engagements are claims.
type attributionRig struct {
	conn    *sql.DB
	stores  db.Stores
	seedArt func(domain.Deliverable)
	stub    *backstopStub
	rc      *Reconciler
}

const attrConversation = "88888888-8888-8888-8888-888888888888"

func newAttributionRig(t *testing.T, nodes map[string]string, claims []domain.ExecutorClaim) *attributionRig {
	t.Helper()
	conn, stores, seedConversation, seedArt := reconcileTestDB(t)
	seedConversation("ent-attr", attrConversation)
	stores.ConversationQueue = fakeEngagements{
		ConversationQueueStore: stores.ConversationQueue,
		claims:                 map[string][]domain.ExecutorClaim{attrConversation: claims},
	}
	stub, client := newBackstopStub(t, nodes)
	return &attributionRig{conn: conn, stores: stores, seedArt: seedArt, stub: stub,
		rc: NewReconciler(&fakeResolver{client: client}, stores, SealedAuthor, nil)}
}

// seedEntity creates the entity of PR_<number> in octo/repo, carrying its
// node id, and returns its id.
func (r *attributionRig) seedEntity(t *testing.T, number int) string {
	t.Helper()
	entityID := fmt.Sprintf("ent-pr-%d", number)
	var repoID string
	if err := r.conn.QueryRow(`SELECT repository_id FROM entities WHERE id = 'ent-attr'`).Scan(&repoID); err != nil {
		t.Fatalf("read the seeded repository: %v", err)
	}
	if _, err := r.conn.Exec(`INSERT INTO entities (id, source, source_id, kind, scope, repository_id, external_id) VALUES (?, 'github', ?, 'pull_request', ?, ?, ?)`,
		entityID, domain.PullRequestTarget("octo/repo", number), reconcileTestHost, repoID, fmt.Sprintf("PR_%d", number)); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return entityID
}

// seedPR records the run's pending pull request deliverable for PR_<number>,
// as the create records it, linked to its entity (seedEntity), and returns
// both.
func (r *attributionRig) seedPR(t *testing.T, number int) (domain.Deliverable, string) {
	t.Helper()
	entityID := r.seedEntity(t, number)
	pr := domain.NewPullRequestDeliverable("octo/repo", number, fmt.Sprintf("PR_%d", number), "recorded-head", "main", "u", "t", "b", true)
	pr.ConversationID, pr.EntityID, pr.Verification = attrConversation, entityID, domain.DeliverableVerificationPending
	r.seedArt(pr)
	return pr, entityID
}

func (r *attributionRig) reconcile(t *testing.T) {
	t.Helper()
	if err := r.rc.ReconcileOrg(context.Background(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ReconcileOrg: %v", err)
	}
}

// attribution reads the entity's owning team and commissioner.
func (r *attributionRig) attribution(t *testing.T, entityID string) (team, user string) {
	t.Helper()
	var tm, us sql.NullString
	if err := r.conn.QueryRow(`SELECT owning_team_id, commissioned_by_user_id FROM entities WHERE id = ?`, entityID).Scan(&tm, &us); err != nil {
		t.Fatalf("read attribution of %s: %v", entityID, err)
	}
	return tm.String, us.String
}

// row reads PR_<number>'s deliverable back, failing when there is none.
func (r *attributionRig) row(t *testing.T, number int) domain.Deliverable {
	t.Helper()
	got := prDeliverableFor(t, r.conn, "octo/repo", number)
	if got == nil {
		t.Fatalf("no deliverable for PR_%d", number)
	}
	return *got
}

// countForeignWith swaps the foreign counter for one backed by a manual
// reader for the rest of the test, and returns a read of its total.
func countForeignWith(t *testing.T) func() int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := foreignStats.Load()
	foreignStats.Store(newForeignCounters(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { foreignStats.Store(prev) })
	return func() int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		var n int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
					for _, dp := range sum.DataPoints {
						n += dp.Value
					}
				}
			}
		}
		return n
	}
}

// A run's own pull request — authored by the account sealed into the
// engagement live when it was opened — is settled verified: its entity gets
// the run's team as owner and the conversation's creator as commissioner, and
// the row's head branch is GitHub's, not the one the run recorded.
func TestReconcile_SealedAuthor_VerifiesTheRunsOwnPullRequest(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_11": authoredPRNode(11, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)})
	_, entityID := rig.seedPR(t, 11)

	rig.reconcile(t)

	got := rig.row(t, 11)
	if got.Verification != domain.DeliverableVerificationVerified || got.State != domain.DeliverableStatePRDraft {
		t.Errorf("row = (%q, %q), want verified and still draft", got.Verification, got.State)
	}
	if d, _ := domain.ParsePRDeliverableDetails(got.DetailsJSON); d.HeadBranch != "feat-11" || d.NodeID != "PR_11" {
		t.Errorf("details = %+v, want GitHub's head feat-11 and node PR_11", d)
	}
	if team, user := rig.attribution(t, entityID); team != runmode.LocalDefaultTeamID || user != runmode.LocalDefaultUserID {
		t.Errorf("attribution = (%q, %q), want (%s, %s)", team, user, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	}
}

// The done-when: a run's own pull request is still verified after the org
// binds a different App, because the check reads the account the run's own
// engagement sealed, not the org's current one.
func TestReconcile_SealedAuthor_VerifiesAcrossAnAppRebind(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_12": authoredPRNode(12, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{
			engagement(botA, botALogin, attrT0, at(time.Hour)),
			engagement(botB, "other-bot[bot]", attrT0.Add(3*time.Hour), nil),
		})
	_, entityID := rig.seedPR(t, 12)

	rig.reconcile(t)

	if got := rig.row(t, 12); got.Verification != domain.DeliverableVerificationVerified {
		t.Errorf("verification = %q, want verified", got.Verification)
	}
	if team, _ := rig.attribution(t, entityID); team != runmode.LocalDefaultTeamID {
		t.Errorf("owning team = %q, want %s", team, runmode.LocalDefaultTeamID)
	}
}

// The other half of the done-when: a relayed create naming a pull request
// someone else authored leaves no owner or commissioner on it, and its
// deliverable is settled foreign and closed, counted once however many passes
// see it. Closing a draft runs the resolved hook, so a task holding open on it
// is not left waiting on an approval nobody can give.
func TestReconcile_SealedAuthor_SettlesAPullRequestTheRunDidNotOpenForeign(t *testing.T) {
	foreign := countForeignWith(t)
	rig := newAttributionRig(t,
		map[string]string{"PR_13": authoredPRNode(13, person, false, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)})
	_, entityID := rig.seedPR(t, 13)
	var resolved []string
	rig.rc.SetPullRequestResolvedHook(func(_ context.Context, _, id string) { resolved = append(resolved, id) })

	rig.reconcile(t)
	rig.reconcile(t)

	got := rig.row(t, 13)
	if got.Verification != domain.DeliverableVerificationForeign || got.State != domain.DeliverableStatePRClosed {
		t.Errorf("row = (%q, %q), want foreign and closed", got.Verification, got.State)
	}
	if team, user := rig.attribution(t, entityID); team != "" || user != "" {
		t.Errorf("attribution = (%q, %q), want none", team, user)
	}
	if n := foreign(); n != 1 {
		t.Errorf("foreign count after two passes = %d, want 1", n)
	}
	if len(resolved) != 1 || resolved[0] != attrConversation {
		t.Errorf("resolved hook ran for %v, want [%s]", resolved, attrConversation)
	}
}

// A row keyed on one pull request with another's node id in its details is
// checked against the pull request its key names, the one its entity is: a
// sidecar cannot pass the check with its own pull request's id and have the
// person's pull request stamped.
func TestReconcile_SealedAuthor_ChecksThePullRequestTheKeyNames(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{
			"PR_14": authoredPRNode(14, person, false, attrT0.Add(10*time.Minute)),
			"PR_15": authoredPRNode(15, botA, true, attrT0.Add(10*time.Minute)),
		},
		[]domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)})
	entityID := rig.seedEntity(t, 14)
	forged := domain.NewPullRequestDeliverable("octo/repo", 14, "PR_14", "feat", "main", "u", "t", "b", true)
	forged.DetailsJSON = domain.MarshalPRDeliverableDetails(domain.PRDeliverableDetails{NodeID: "PR_15", HeadBranch: "feat", Base: "main"})
	forged.ConversationID, forged.EntityID, forged.Verification = attrConversation, entityID, domain.DeliverableVerificationPending
	rig.seedArt(forged)

	rig.reconcile(t)

	if got := rig.row(t, 14); got.Verification != domain.DeliverableVerificationForeign {
		t.Errorf("verification = %q, want foreign", got.Verification)
	}
	if team, user := rig.attribution(t, entityID); team != "" || user != "" {
		t.Errorf("the person's pull request was attributed (%q, %q)", team, user)
	}
}

// An answer the reconciler cannot read — the engagement live at the creation
// sealed no account — settles nothing, and the row stays pending.
func TestReconcile_SealedAuthor_LeavesAnUnsettledPullRequestPending(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_16": authoredPRNode(16, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{engagement("", "", attrT0, nil)})
	_, entityID := rig.seedPR(t, 16)

	rig.reconcile(t)

	if got := rig.row(t, 16); got.Verification != domain.DeliverableVerificationPending || got.State != domain.DeliverableStatePRDraft {
		t.Errorf("row = (%q, %q), want pending and still draft", got.Verification, got.State)
	}
	if team, user := rig.attribution(t, entityID); team != "" || user != "" {
		t.Errorf("attribution = (%q, %q), want none", team, user)
	}
}

// A stamp that fails leaves the row pending, so the next pass stamps again
// rather than leaving the commissioner unset for good behind an owner that
// landed.
func TestReconcile_SealedAuthor_RetriesAFailedStamp(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_17": authoredPRNode(17, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)})
	_, entityID := rig.seedPR(t, 17)
	stores := rig.stores
	stores.Entities = &flakyCommissioner{EntityStore: stores.Entities, failures: 1}
	_, client := newBackstopStub(t, map[string]string{"PR_17": authoredPRNode(17, botA, true, attrT0.Add(10*time.Minute))})
	rig.rc = NewReconciler(&fakeResolver{client: client}, stores, SealedAuthor, nil)

	rig.reconcile(t)
	if got := rig.row(t, 17); got.Verification != domain.DeliverableVerificationPending {
		t.Fatalf("after a failed stamp, verification = %q, want pending", got.Verification)
	}
	rig.reconcile(t)

	if got := rig.row(t, 17); got.Verification != domain.DeliverableVerificationVerified {
		t.Errorf("after the retry, verification = %q, want verified", got.Verification)
	}
	if team, user := rig.attribution(t, entityID); team != runmode.LocalDefaultTeamID || user != runmode.LocalDefaultUserID {
		t.Errorf("attribution = (%q, %q), want (%s, %s)", team, user, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	}
}

// The backstop records a pull request from a pr_created row pending, like the
// create's own row, and the same cycle settles it: the run's own is verified
// and attributed, someone else's is settled foreign. A foreign row is still a
// row, so a later cycle neither reads its ledger row again nor counts it again.
func TestBackstop_SealedAuthor_RecordsPendingAndSettlesInTheSameCycle(t *testing.T) {
	foreign := countForeignWith(t)
	rig := newAttributionRig(t,
		map[string]string{
			"PR_18": authoredPRNode(18, botA, true, attrT0.Add(10*time.Minute)),
			"PR_19": authoredPRNode(19, person, false, attrT0.Add(10*time.Minute)),
		},
		[]domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)})
	for _, n := range []int{18, 19} {
		rig.seedEntity(t, n)
		recordPRCreated(t, rig.stores, reconcileTestHost, attrConversation, "octo/repo", n, fmt.Sprintf("PR_%d", n),
			domain.DeliverableStatePRDraft, fmt.Sprintf("https://github.com/octo/repo/pull/%d", n))
	}

	rig.reconcile(t)

	if got := rig.row(t, 18); got.Verification != domain.DeliverableVerificationVerified {
		t.Errorf("the run's own backfilled pull request = %q, want verified", got.Verification)
	}
	if team, user := rig.attribution(t, "ent-pr-18"); team != runmode.LocalDefaultTeamID || user != runmode.LocalDefaultUserID {
		t.Errorf("backfilled pull request attribution = (%q, %q), want (%s, %s)", team, user, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	}
	if got := rig.row(t, 19); got.Verification != domain.DeliverableVerificationForeign || got.State != domain.DeliverableStatePRClosed {
		t.Errorf("someone else's backfilled pull request = (%q, %q), want foreign and closed", got.Verification, got.State)
	}
	if team, user := rig.attribution(t, "ent-pr-19"); team != "" || user != "" {
		t.Errorf("someone else's pull request attribution = (%q, %q), want none", team, user)
	}

	before := len(rig.stub.requested())
	rig.reconcile(t)
	if calls := rig.stub.requested()[before:]; len(calls) != 1 || calls[0] != "nodes PR_18" {
		t.Errorf("second cycle's GitHub calls = %v, want only PR_18's reconcile", calls)
	}
	if n := foreign(); n != 1 {
		t.Errorf("foreign count after two cycles = %d, want 1", n)
	}
}

// SettleDeliverable settles a pending row on demand, for a person about to
// act on it: verified for the run's own pull request, foreign for another's.
func TestSettleDeliverable_SettlesOnDemand(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{
			"PR_20": authoredPRNode(20, botA, true, attrT0.Add(10*time.Minute)),
			"PR_21": authoredPRNode(21, person, false, attrT0.Add(10*time.Minute)),
		},
		[]domain.ExecutorClaim{engagement(botA, botALogin, attrT0, nil)})
	rig.seedPR(t, 20)
	rig.seedPR(t, 21)
	var resolved []string
	rig.rc.SetPullRequestResolvedHook(func(_ context.Context, _, id string) { resolved = append(resolved, id) })

	ctx := context.Background()
	rig.rc.SettleDeliverable(ctx, runmode.LocalDefaultOrgID, rig.row(t, 20))
	rig.rc.SettleDeliverable(ctx, runmode.LocalDefaultOrgID, rig.row(t, 21))

	if got := rig.row(t, 20); got.Verification != domain.DeliverableVerificationVerified {
		t.Errorf("the run's own = %q, want verified", got.Verification)
	}
	if got := rig.row(t, 21); got.Verification != domain.DeliverableVerificationForeign || got.State != domain.DeliverableStatePRClosed {
		t.Errorf("someone else's = (%q, %q), want foreign and closed", got.Verification, got.State)
	}
	if len(resolved) != 1 || resolved[0] != attrConversation {
		t.Errorf("resolved hook ran for %v, want [%s] once, for the foreign draft", resolved, attrConversation)
	}
}

// Local mode takes its records' word: a run's pending pull request is
// verified and attributed with no engagement to check it against, whoever
// GitHub says authored it, and on demand without reading GitHub at all.
func TestTrustRecords_VerifiesWithoutACheck(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_22": authoredPRNode(22, person, false, attrT0)},
		nil)
	_, entityID := rig.seedPR(t, 22)
	rig.seedPR(t, 23)
	_, client := newBackstopStub(t, map[string]string{"PR_22": authoredPRNode(22, person, false, attrT0)})
	rig.rc = NewReconciler(&fakeResolver{client: client}, rig.stores, TrustRecords, nil)

	rig.reconcile(t)
	if got := rig.row(t, 22); got.Verification != domain.DeliverableVerificationVerified {
		t.Errorf("verification = %q, want verified", got.Verification)
	}
	if team, user := rig.attribution(t, entityID); team != runmode.LocalDefaultTeamID || user != runmode.LocalDefaultUserID {
		t.Errorf("attribution = (%q, %q), want (%s, %s)", team, user, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	}

	rig.rc.SettleDeliverable(context.Background(), runmode.LocalDefaultOrgID, rig.row(t, 23))
	if got := rig.row(t, 23); got.Verification != domain.DeliverableVerificationVerified {
		t.Errorf("on demand, verification = %q, want verified", got.Verification)
	}
}
