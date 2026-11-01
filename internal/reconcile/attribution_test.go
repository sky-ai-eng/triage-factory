package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The bot accounts two Apps act as, and a person's account.
const (
	botA   = "41898282"
	botB   = "29110"
	person = "583231"
)

var attrT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// engagement is one claim of a conversation from `from` until `to` (nil for a
// live one), whose seal recorded account on its github entry ("" for none).
func engagement(account string, from time.Time, to *time.Time) domain.ExecutorClaim {
	return domain.ExecutorClaim{
		ID:         fmt.Sprintf("claim-%s-%d", account, from.Unix()),
		ClaimedAt:  from,
		ReleasedAt: to,
		Sources:    domain.SourceManifest{eventsource.KindGitHub: {State: domain.SourceSealed, AccountID: account}},
	}
}

func at(d time.Duration) *time.Time {
	t := attrT0.Add(d)
	return &t
}

func TestJudgeSealedAuthor(t *testing.T) {
	unsealed := engagement("", attrT0, at(time.Hour))
	unsealed.Sources = nil
	for _, tc := range []struct {
		name       string
		claims     []domain.ExecutorClaim
		author     string
		created    time.Time
		want       verdict
		wantReason string
	}{
		{
			name:   "the live engagement's account authored it",
			claims: []domain.ExecutorClaim{engagement(botA, attrT0, nil)},
			author: botA, created: attrT0.Add(10 * time.Minute),
			want: verdictRun,
		},
		{
			name:   "a person authored it while the run was live",
			claims: []domain.ExecutorClaim{engagement(botA, attrT0, nil)},
			author: person, created: attrT0.Add(10 * time.Minute),
			want: verdictForeign, wantReason: foreignAuthor,
		},
		{
			name:   "opened before the conversation was ever claimed",
			claims: []domain.ExecutorClaim{engagement(botA, attrT0, nil)},
			author: botA, created: attrT0.Add(-time.Hour),
			want: verdictForeign, wantReason: foreignNoEngagement,
		},
		{
			name:   "opened after its only engagement was released",
			claims: []domain.ExecutorClaim{engagement(botA, attrT0, at(time.Hour))},
			author: botA, created: attrT0.Add(2 * time.Hour),
			want: verdictForeign, wantReason: foreignNoEngagement,
		},
		{
			name:   "no engagements at all",
			author: botA, created: attrT0,
			want: verdictForeign, wantReason: foreignNoEngagement,
		},
		{
			name:   "GitHub's clock a minute behind the claim",
			claims: []domain.ExecutorClaim{engagement(botA, attrT0, at(time.Hour))},
			author: botA, created: attrT0.Add(-time.Minute),
			want: verdictRun,
		},
		{
			name:   "GitHub's clock a minute past the release",
			claims: []domain.ExecutorClaim{engagement(botA, attrT0, at(time.Hour))},
			author: botA, created: attrT0.Add(time.Hour + time.Minute),
			want: verdictRun,
		},
		{
			name:   "the live engagement sealed no account",
			claims: []domain.ExecutorClaim{unsealed},
			author: botA, created: attrT0.Add(10 * time.Minute),
			want: verdictUnknown,
		},
		{
			name: "a handover: one engagement unsealed, the other the author's",
			claims: []domain.ExecutorClaim{
				func() domain.ExecutorClaim { c := unsealed; c.ReleasedAt = at(time.Minute); return c }(),
				engagement(botA, attrT0.Add(time.Minute), nil),
			},
			author: botA, created: attrT0.Add(time.Minute),
			want: verdictRun,
		},
		{
			name: "an earlier engagement's App authored it after the org bound another",
			claims: []domain.ExecutorClaim{
				engagement(botA, attrT0, at(time.Hour)),
				engagement(botB, attrT0.Add(2*time.Hour), nil),
			},
			author: botA, created: attrT0.Add(30 * time.Minute),
			want: verdictRun,
		},
		{
			name: "the later engagement's App did not author one opened under the earlier",
			claims: []domain.ExecutorClaim{
				engagement(botA, attrT0, at(time.Hour)),
				engagement(botB, attrT0.Add(2*time.Hour), nil),
			},
			author: botB, created: attrT0.Add(30 * time.Minute),
			want: verdictForeign, wantReason: foreignAuthor,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := judgeSealedAuthor(tc.claims, tc.author, tc.created)
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

// authoredPRNode is an open pull request node as RefreshPRs' fragment returns
// it, PR_<number> in octo/repo, opened at created by the account authorID
// (a Bot when bot is set).
func authoredPRNode(number int, authorID string, bot bool, created time.Time) string {
	typename, login := "User", "octocat"
	if bot {
		typename, login = "Bot", "acme-bot"
	}
	id, _ := json.Number(authorID).Int64()
	b, _ := json.Marshal(map[string]any{
		"id": fmt.Sprintf("PR_%d", number), "number": number, "title": "t", "body": "b",
		"state": "OPEN", "isDraft": true, "merged": false,
		"headRefName": "feat", "baseRefName": "main",
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
	_, client := newBackstopStub(t, nodes)
	return &attributionRig{conn: conn, stores: stores, seedArt: seedArt,
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

// seedPR records the run's pull request deliverable for PR_<number>, linked to
// its entity (seedEntity), and returns both.
func (r *attributionRig) seedPR(t *testing.T, number int) (domain.Deliverable, string) {
	t.Helper()
	entityID := r.seedEntity(t, number)
	pr := domain.NewPullRequestDeliverable("octo/repo", number, fmt.Sprintf("PR_%d", number), "feat", "main", "u", "t", "b", true)
	pr.ConversationID, pr.EntityID = attrConversation, entityID
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

// countForeignWith swaps the foreign counter for one backed by a manual
// reader for the rest of the test, and returns a read of its total for
// disposition.
func countForeignWith(t *testing.T) func(disposition string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := foreignStats.Load()
	foreignStats.Store(newForeignCounters(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { foreignStats.Store(prev) })
	return func(disposition string) int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		var n int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					continue
				}
				for _, dp := range sum.DataPoints {
					if v, _ := dp.Attributes.Value(attribute.Key("disposition")); v.AsString() == disposition {
						n += dp.Value
					}
				}
			}
		}
		return n
	}
}

// A run's own pull request — authored by the account sealed into the
// engagement live when it was opened — is attributed to the run: its entity
// gets the run's team as owner and the conversation's creator as
// commissioner.
func TestReconcile_SealedAuthor_AttributesTheRunsOwnPullRequest(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_11": authoredPRNode(11, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{engagement(botA, attrT0, nil)})
	pr, entityID := rig.seedPR(t, 11)

	rig.reconcile(t)

	if team, user := rig.attribution(t, entityID); team != runmode.LocalDefaultTeamID || user != runmode.LocalDefaultUserID {
		t.Errorf("attribution = (%q, %q), want (%s, %s)", team, user, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	}
	if got := prDeliverableFor(t, rig.conn, "octo/repo", 11); got == nil || got.ID == "" || got.DedupKey != pr.DedupKey {
		t.Errorf("deliverable after the pass = %+v, want it kept", got)
	}
}

// The done-when of the change: a run's own pull request is still attributed
// after the org binds a different App, because the check reads the account
// the run's own engagement sealed, not the org's current one.
func TestReconcile_SealedAuthor_AttributesAcrossAnAppRebind(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_12": authoredPRNode(12, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{
			engagement(botA, attrT0, at(time.Hour)),
			engagement(botB, attrT0.Add(3*time.Hour), nil),
		})
	_, entityID := rig.seedPR(t, 12)

	rig.reconcile(t)

	if team, _ := rig.attribution(t, entityID); team != runmode.LocalDefaultTeamID {
		t.Errorf("owning team = %q, want %s", team, runmode.LocalDefaultTeamID)
	}
}

// The other half of the done-when: a relayed create naming a pull request
// someone else authored leaves no owner or commissioner on it, and its
// deliverable, which offers actions on that pull request, is deleted and
// counted. The ledger row is not the reconciler's to touch.
func TestReconcile_SealedAuthor_DeletesAPullRequestTheRunDidNotOpen(t *testing.T) {
	foreign := countForeignWith(t)
	rig := newAttributionRig(t,
		map[string]string{"PR_13": authoredPRNode(13, person, false, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{engagement(botA, attrT0, nil)})
	_, entityID := rig.seedPR(t, 13)

	rig.reconcile(t)

	if team, user := rig.attribution(t, entityID); team != "" || user != "" {
		t.Errorf("attribution = (%q, %q), want none", team, user)
	}
	if got := prDeliverableFor(t, rig.conn, "octo/repo", 13); got != nil {
		t.Errorf("deliverable after the pass = %+v, want it deleted", got)
	}
	if n := foreign(foreignDeleted); n != 1 {
		t.Errorf("foreign deleted count = %d, want 1", n)
	}
}

// An answer the reconciler cannot read — the engagement live at the creation
// sealed no account — attributes nothing and deletes nothing.
func TestReconcile_SealedAuthor_LeavesAnUnsettledPullRequestAlone(t *testing.T) {
	unsealed := engagement("", attrT0, nil)
	unsealed.Sources = domain.SourceManifest{eventsource.KindGitHub: {State: domain.SourceSealed}}
	rig := newAttributionRig(t,
		map[string]string{"PR_14": authoredPRNode(14, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{unsealed})
	_, entityID := rig.seedPR(t, 14)

	rig.reconcile(t)

	if team, user := rig.attribution(t, entityID); team != "" || user != "" {
		t.Errorf("attribution = (%q, %q), want none", team, user)
	}
	if got := prDeliverableFor(t, rig.conn, "octo/repo", 14); got == nil {
		t.Error("an unsettled deliverable was deleted")
	}
}

// An entity that already has an owner is not judged again: a deliverable
// attributed on an earlier pass is not deleted by a later one that reads a
// manifest a re-seal has since replaced.
func TestReconcile_SealedAuthor_DoesNotRejudgeAnOwnedPullRequest(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_15": authoredPRNode(15, botA, true, attrT0.Add(10*time.Minute))},
		[]domain.ExecutorClaim{engagement(botB, attrT0, nil)})
	_, entityID := rig.seedPR(t, 15)
	if _, err := rig.stores.Entities.StampOwningTeamIfUnsetSystem(context.Background(), runmode.LocalDefaultOrgID, entityID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("stamp owner: %v", err)
	}

	rig.reconcile(t)

	if got := prDeliverableFor(t, rig.conn, "octo/repo", 15); got == nil {
		t.Error("an owned pull request's deliverable was deleted")
	}
}

// The backstop records a deliverable from a pr_created row only for a pull
// request the row's run opened: a row naming someone else's pull request is
// counted and never recorded, and the run's own is recorded and, in the same
// cycle, attributed.
func TestBackstop_SealedAuthor_RecordsOnlyTheRunsOwnPullRequests(t *testing.T) {
	foreign := countForeignWith(t)
	rig := newAttributionRig(t,
		map[string]string{
			"PR_16": authoredPRNode(16, botA, true, attrT0.Add(10*time.Minute)),
			"PR_17": authoredPRNode(17, person, false, attrT0.Add(10*time.Minute)),
		},
		[]domain.ExecutorClaim{engagement(botA, attrT0, nil)})
	for _, n := range []int{16, 17} {
		rig.seedEntity(t, n)
		recordPRCreated(t, rig.stores, reconcileTestHost, attrConversation, "octo/repo", n, fmt.Sprintf("PR_%d", n),
			domain.DeliverableStatePRDraft, fmt.Sprintf("https://github.com/octo/repo/pull/%d", n))
	}

	rig.reconcile(t)

	if got := prDeliverableFor(t, rig.conn, "octo/repo", 16); got == nil {
		t.Error("the run's own pull request was not backfilled")
	}
	if team, user := rig.attribution(t, "ent-pr-16"); team != runmode.LocalDefaultTeamID || user != runmode.LocalDefaultUserID {
		t.Errorf("backfilled pull request attribution = (%q, %q), want (%s, %s)", team, user, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	}
	if got := prDeliverableFor(t, rig.conn, "octo/repo", 17); got != nil {
		t.Errorf("someone else's pull request was backfilled: %+v", got)
	}
	if team, user := rig.attribution(t, "ent-pr-17"); team != "" || user != "" {
		t.Errorf("someone else's pull request attribution = (%q, %q), want none", team, user)
	}
	if n := foreign(foreignNotRecorded); n != 1 {
		t.Errorf("foreign not-recorded count = %d, want 1", n)
	}
}

// Local mode takes its records' word: a run's pull request is attributed with
// no engagement to check it against, whoever GitHub says authored it.
func TestReconcile_TrustRecords_AttributesWithoutACheck(t *testing.T) {
	rig := newAttributionRig(t,
		map[string]string{"PR_18": authoredPRNode(18, person, false, attrT0)},
		nil)
	_, entityID := rig.seedPR(t, 18)
	_, client := newBackstopStub(t, map[string]string{"PR_18": authoredPRNode(18, person, false, attrT0)})
	rig.rc = NewReconciler(&fakeResolver{client: client}, rig.stores, TrustRecords, nil)

	rig.reconcile(t)

	if team, user := rig.attribution(t, entityID); team != runmode.LocalDefaultTeamID || user != runmode.LocalDefaultUserID {
		t.Errorf("attribution = (%q, %q), want (%s, %s)", team, user, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	}
	if got := prDeliverableFor(t, rig.conn, "octo/repo", 18); got == nil {
		t.Error("a trusted record's deliverable was deleted")
	}
}
