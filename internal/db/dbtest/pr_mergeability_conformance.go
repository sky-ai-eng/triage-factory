package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// PRMergeabilityFixture is one subtest's world for the mergeability store:
// the stores, the org, and the schema-aware writes the suite cannot make
// through a store.
type PRMergeabilityFixture struct {
	Stores db.Stores
	OrgID  string
	// Ripen moves every ready probe's probe_after into the past, standing in
	// for the admission delay elapsing.
	Ripen func(t *testing.T)
	// ExpireLease rewinds a leased probe's lease into the past, standing in
	// for a holder that died without a terminal write.
	ExpireLease func(t *testing.T, id int64)
	// Probes reads the entity's probe rows, oldest first.
	Probes func(t *testing.T, entityID string) []MergeabilityProbeRow
}

// MergeabilityProbeRow is the slice of a pr_mergeability_probes row the suite
// asserts on.
type MergeabilityProbeRow struct {
	ID           int64
	Status       string
	Reason       string
	UnknownReads int
	Attempt      int
	ProbeAfter   time.Time
}

// PRMergeabilityFactory builds a fresh fixture per subtest.
type PRMergeabilityFactory func(t *testing.T) PRMergeabilityFixture

var mergeabilityOwner = workitem.Owner{ID: "conformance-mergeability-worker", Epoch: 1}

// RunPRMergeabilityConformance covers db.PRMergeabilityStore: admission keys
// on the entity and keeps the first reason, a probe is not claimable before
// its probe_after, the claim names the pull request and its repository, a
// known answer upserts the row and enqueues exactly the transition it implies
// in the same transaction, an UNKNOWN answer admits its retry until the
// limit, a probe of a pull request that is gone writes nothing, a taken-over
// receipt writes nothing at all, and the row reads, the inactive delete, the
// prune and the handle's description.
func RunPRMergeabilityConformance(t *testing.T, mk PRMergeabilityFactory) {
	t.Helper()
	ctx := context.Background()

	pr := func(t *testing.T, f PRMergeabilityFixture, number string) *domain.Entity {
		t.Helper()
		e, _, err := f.Stores.Entities.FindOrCreateSystem(ctx, f.OrgID, "github", TestGitHubHost,
			"octo/api#"+number, "PR_merge"+number, domain.EntityKindPullRequest, "PR "+number, TestGitHubHost+"/octo/api/pull/"+number)
		if err != nil {
			t.Fatalf("seed pull request entity: %v", err)
		}
		snap, _ := json.Marshal(domain.PRSnapshot{
			Number: 7, Author: "aidan", AuthorID: "41", Repo: "octo/api", IsDraft: true,
			HeadSHA: "stored-head", Labels: []string{"bug"},
		})
		if ok, err := f.Stores.Entities.UpdateSnapshotCASSystem(ctx, f.OrgID, e.ID, string(snap), e.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		return e
	}
	admit := func(t *testing.T, f PRMergeabilityFixture, entityID, reason string) int {
		t.Helper()
		n, err := f.Stores.PRMergeability.AdmitSystem(ctx, f.OrgID, []db.PRMergeabilityProbe{{EntityID: entityID, Reason: reason}})
		if err != nil {
			t.Fatalf("AdmitSystem: %v", err)
		}
		return n
	}
	claimOne := func(t *testing.T, f PRMergeabilityFixture) db.ClaimedPRMergeabilityProbe {
		t.Helper()
		f.Ripen(t)
		batch, err := f.Stores.PRMergeability.Claim(ctx, mergeabilityOwner, f.OrgID, 100)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Items) != 1 {
			t.Fatalf("Claim returned %d items, want 1", len(batch.Items))
		}
		return batch.Items[0]
	}
	answer := func(t *testing.T, f PRMergeabilityFixture, item db.ClaimedPRMergeabilityProbe, mergeable, head string) db.PRMergeabilityProbeResult {
		t.Helper()
		res, err := f.Stores.PRMergeability.CompleteProbe(ctx, item.Receipt, db.PRMergeabilityAnswer{Found: true, Mergeable: mergeable, HeadSHA: head}, "")
		if err != nil {
			t.Fatalf("CompleteProbe %s: %v", mergeable, err)
		}
		return res
	}
	probe := func(t *testing.T, f PRMergeabilityFixture, entityID, mergeable, head string) db.PRMergeabilityProbeResult {
		t.Helper()
		admit(t, f, entityID, domain.MergeabilityProbeSweep)
		return answer(t, f, claimOne(t, f), mergeable, head)
	}
	queued := func(t *testing.T, f PRMergeabilityFixture, entityID string) []domain.QueuedEvent {
		t.Helper()
		rows, err := f.Stores.EventQueue.ListForEntity(ctx, f.OrgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		return rows
	}
	stored := func(f PRMergeabilityFixture, entityID string) func() (*domain.PRMergeability, error) {
		return func() (*domain.PRMergeability, error) {
			return f.Stores.PRMergeability.GetSystem(ctx, f.OrgID, entityID)
		}
	}

	t.Run("Admit_keys_on_the_entity_and_keeps_the_first_reason", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "1")
		if n := admit(t, f, e.ID, domain.MergeabilityProbeNew); n != 1 {
			t.Fatalf("first admission admitted %d, want 1", n)
		}
		if n := admit(t, f, e.ID, domain.MergeabilityProbeHeadMoved); n != 0 {
			t.Errorf("second admission admitted %d, want it collapsed", n)
		}
		rows := f.Probes(t, e.ID)
		if len(rows) != 1 || rows[0].Reason != domain.MergeabilityProbeNew || rows[0].Status != workitem.StatusReady {
			t.Fatalf("probes = %+v, want one ready probe for the first reason", rows)
		}
		if delay := time.Until(rows[0].ProbeAfter); delay < workkinds.PRMergeabilityProbeDelay-5*time.Second || delay > workkinds.PRMergeabilityProbeDelay {
			t.Errorf("probe_after is %s away, want about %s", delay, workkinds.PRMergeabilityProbeDelay)
		}
		if _, err := f.Stores.PRMergeability.AdmitSystem(ctx, f.OrgID, []db.PRMergeabilityProbe{{EntityID: e.ID, Reason: "base_moved_later"}}); err == nil {
			t.Error("an admission with a reason outside the vocabulary succeeded")
		}
	})

	t.Run("Claim_waits_for_probe_after_and_names_the_pull_request", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "2")
		admit(t, f, e.ID, domain.MergeabilityProbeNew)
		batch, err := f.Stores.PRMergeability.Claim(ctx, mergeabilityOwner, f.OrgID, 100)
		if err != nil || len(batch.Items) != 0 {
			t.Fatalf("Claim before probe_after = %d items (err %v), want none", len(batch.Items), err)
		}
		item := claimOne(t, f)
		repo, err := f.Stores.Repos.GetSystem(ctx, f.OrgID, e.RepositoryID)
		if err != nil || repo == nil {
			t.Fatalf("read the entity's repository: %v", err)
		}
		if item.EntityID != e.ID || item.Reason != domain.MergeabilityProbeNew || item.NodeID != "PR_merge2" || !item.Active ||
			item.RepositoryID != e.RepositoryID || item.Owner != repo.Owner || item.Repo != repo.Repo || item.UnknownReads != 0 {
			t.Errorf("claimed = %+v, want the pull request's node id, its repository %s/%s, and the admission's columns", item, repo.Owner, repo.Repo)
		}
	})

	t.Run("A_conflicting_answer_upserts_the_row_and_enqueues_conflicts_in_one_write", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "3")
		res := probe(t, f, e.ID, domain.MergeableConflicting, "head-3")
		if res.Outcome != db.PRMergeabilityRecorded || res.Previous != "" || res.Row == nil {
			t.Fatalf("result = %+v, want a recorded answer with no previous one", res)
		}
		if res.Row.Mergeable != domain.MergeableConflicting || res.Row.MergeableHeadSHA != "head-3" || res.Row.CheckedAt == nil || res.Row.OrgID != f.OrgID {
			t.Errorf("row = %+v, want CONFLICTING at head-3, checked now", res.Row)
		}
		AssertWriteReturnedStoredRow(t, "CompleteProbe upsert", *res.Row, stored(f, e.ID))

		if len(res.Events) != 1 || res.Events[0].EventType != domain.EventGitHubPRConflicts || res.Events[0].ID == "" || res.Events[0].OrgID != f.OrgID {
			t.Fatalf("events = %+v, want one stamped github:pr:conflicts", res.Events)
		}
		var meta events.GitHubPRConflictsMetadata
		if err := json.Unmarshal([]byte(res.Events[0].MetadataJSON), &meta); err != nil {
			t.Fatalf("metadata: %v", err)
		}
		want := events.GitHubPRConflictsMetadata{Author: "aidan", AuthorID: "41", Repo: "octo/api", RepositoryID: e.RepositoryID,
			PRNumber: 7, IsDraft: true, HeadSHA: "head-3", Labels: []string{"bug"}}
		if meta.Author != want.Author || meta.AuthorID != want.AuthorID || meta.Repo != want.Repo || meta.RepositoryID != want.RepositoryID ||
			meta.PRNumber != want.PRNumber || !meta.IsDraft || meta.HeadSHA != want.HeadSHA || len(meta.Labels) != 1 || meta.Labels[0] != "bug" {
			t.Errorf("metadata = %+v, want the snapshot's fields with the answer's head: %+v", meta, want)
		}
		q := queued(t, f, e.ID)
		if len(q) != 1 || q[0].EventID != res.Events[0].ID || q[0].EventType != domain.EventGitHubPRConflicts || q[0].Status != workitem.StatusReady {
			t.Errorf("event queue = %+v, want the conflicts event ready to route", q)
		}
		if rows := f.Probes(t, e.ID); len(rows) != 1 || rows[0].Status != workitem.StatusDone {
			t.Errorf("probes = %+v, want the one probe done", rows)
		}

		again := probe(t, f, e.ID, domain.MergeableConflicting, "head-3")
		if again.Previous != domain.MergeableConflicting || len(again.Events) != 0 {
			t.Errorf("a repeated CONFLICTING = %+v, want no event", again)
		}
	})

	t.Run("Conflicting_to_mergeable_enqueues_conflicts_resolved_and_a_repeat_nothing", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "4")
		probe(t, f, e.ID, domain.MergeableConflicting, "head-4")
		res := probe(t, f, e.ID, domain.MergeableMergeable, "head-4b")
		if len(res.Events) != 1 || res.Events[0].EventType != domain.EventGitHubPRConflictsResolved || res.Previous != domain.MergeableConflicting {
			t.Fatalf("result = %+v, want one github:pr:conflicts_resolved", res)
		}
		AssertWriteReturnedStoredRow(t, "CompleteProbe upsert over a row", *res.Row, stored(f, e.ID))
		if again := probe(t, f, e.ID, domain.MergeableMergeable, "head-4b"); len(again.Events) != 0 {
			t.Errorf("a repeated MERGEABLE enqueued %+v, want nothing", again.Events)
		}
		if q := queued(t, f, e.ID); len(q) != 2 {
			t.Errorf("event queue holds %d rows, want the conflicts and the resolution", len(q))
		}

		// A first answer of MERGEABLE is no transition at all.
		fresh := pr(t, f, "4c")
		if res := probe(t, f, fresh.ID, domain.MergeableMergeable, "head-4c"); len(res.Events) != 0 {
			t.Errorf("a first MERGEABLE enqueued %+v, want nothing", res.Events)
		}
	})

	t.Run("Unknown_admits_a_retry_until_the_limit_and_the_give_up_keeps_the_answer", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "5")
		probe(t, f, e.ID, domain.MergeableMergeable, "head-5")
		before, _ := f.Stores.PRMergeability.GetSystem(ctx, f.OrgID, e.ID)

		admit(t, f, e.ID, domain.MergeabilityProbeHeadMoved)
		for reads := 0; reads < workkinds.PRMergeabilityMaxUnknownReads; reads++ {
			item := claimOne(t, f)
			if item.UnknownReads != reads {
				t.Fatalf("read %d claimed a probe with unknown_reads %d", reads, item.UnknownReads)
			}
			res := answer(t, f, item, domain.MergeableUnknown, "head-5b")
			last := reads == workkinds.PRMergeabilityMaxUnknownReads-1
			switch {
			case last && (res.Outcome != db.PRMergeabilityGaveUp || res.SuccessorID != 0):
				t.Fatalf("read %d = %+v, want a give-up with no successor", reads, res)
			case !last && (res.Outcome != db.PRMergeabilityRetried || res.SuccessorID == 0):
				t.Fatalf("read %d = %+v, want a retry", reads, res)
			}
			if len(res.Events) != 0 {
				t.Fatalf("an UNKNOWN answer enqueued %+v", res.Events)
			}
		}
		rows := f.Probes(t, e.ID)
		retry := rows[len(rows)-1]
		if retry.Reason != domain.MergeabilityProbeUnknownRetry || retry.UnknownReads != workkinds.PRMergeabilityMaxUnknownReads-1 || retry.Status != workitem.StatusDone {
			t.Errorf("last probe = %+v, want the final retry, done", retry)
		}
		for _, r := range rows {
			if r.Status != workitem.StatusDone {
				t.Errorf("probe %+v left unsettled after the give-up", r)
			}
		}
		after, _ := f.Stores.PRMergeability.GetSystem(ctx, f.OrgID, e.ID)
		if after == nil || before == nil || after.Mergeable != before.Mergeable || after.MergeableHeadSHA != "head-5b" ||
			after.CheckedAt == nil || before.CheckedAt == nil || after.CheckedAt.Before(*before.CheckedAt) {
			t.Errorf("row after the give-up = %+v, want %+v's answer at the give-up's head, read no earlier", after, before)
		}
	})

	t.Run("A_give_up_with_no_row_records_a_read_with_no_answer", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "5n")
		admit(t, f, e.ID, domain.MergeabilityProbeNew)
		var res db.PRMergeabilityProbeResult
		for reads := 0; reads < workkinds.PRMergeabilityMaxUnknownReads; reads++ {
			res = answer(t, f, claimOne(t, f), domain.MergeableUnknown, "head-5n")
		}
		if res.Outcome != db.PRMergeabilityGaveUp || res.Row == nil {
			t.Fatalf("last read = %+v, want a give-up that wrote the row", res)
		}
		AssertWriteReturnedStoredRow(t, "CompleteProbe give-up insert", *res.Row, stored(f, e.ID))
		if res.Row.Mergeable != "" || res.Row.MergeableHeadSHA != "head-5n" || res.Row.CheckedAt == nil {
			t.Errorf("row = %+v, want no answer at head-5n, read now", *res.Row)
		}
		// The next known answer is the pull request's first, so a conflict
		// emits as it does for a pull request with no row.
		admit(t, f, e.ID, domain.MergeabilityProbeSweep)
		if got := answer(t, f, claimOne(t, f), domain.MergeableConflicting, "head-5n"); len(got.Events) != 1 || got.Events[0].EventType != domain.EventGitHubPRConflicts || got.Previous != "" {
			t.Errorf("first answer after the give-up = %+v, want one github:pr:conflicts", got)
		}
	})

	t.Run("Unknown_retry_waits_the_retry_delay", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "5r")
		admit(t, f, e.ID, domain.MergeabilityProbeNew)
		answer(t, f, claimOne(t, f), domain.MergeableUnknown, "")
		rows := f.Probes(t, e.ID)
		if len(rows) != 2 || rows[1].Status != workitem.StatusReady || rows[1].UnknownReads != 1 {
			t.Fatalf("probes = %+v, want the done probe and a ready retry", rows)
		}
		if delay := time.Until(rows[1].ProbeAfter); delay < workkinds.PRMergeabilityUnknownRetryDelay-5*time.Second || delay > workkinds.PRMergeabilityUnknownRetryDelay {
			t.Errorf("retry's probe_after is %s away, want about %s", delay, workkinds.PRMergeabilityUnknownRetryDelay)
		}
	})

	t.Run("No_answer_or_an_inactive_entity_completes_with_no_write", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "6")
		admit(t, f, e.ID, domain.MergeabilityProbeNew)
		res, err := f.Stores.PRMergeability.CompleteProbe(ctx, claimOne(t, f).Receipt, db.PRMergeabilityAnswer{}, "")
		if err != nil || res.Outcome != db.PRMergeabilitySkipped {
			t.Fatalf("no answer = (%+v, %v), want skipped", res, err)
		}

		admit(t, f, e.ID, domain.MergeabilityProbeSweep)
		item := claimOne(t, f)
		if _, err := f.Stores.Entities.CloseSystem(ctx, f.OrgID, e.ID); err != nil {
			t.Fatalf("close entity: %v", err)
		}
		if res := answer(t, f, item, domain.MergeableConflicting, "head-6"); res.Outcome != db.PRMergeabilitySkipped || len(res.Events) != 0 {
			t.Errorf("an answer for a closed pull request = %+v, want skipped", res)
		}
		if row, _ := f.Stores.PRMergeability.GetSystem(ctx, f.OrgID, e.ID); row != nil {
			t.Errorf("row = %+v, want none written", row)
		}
		if q := queued(t, f, e.ID); len(q) != 0 {
			t.Errorf("event queue = %+v, want nothing", q)
		}
		for _, r := range f.Probes(t, e.ID) {
			if r.Status != workitem.StatusDone {
				t.Errorf("probe %+v, want done", r)
			}
		}
	})

	t.Run("A_taken_over_receipt_writes_and_enqueues_nothing", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "7")
		admit(t, f, e.ID, domain.MergeabilityProbeNew)
		stale := claimOne(t, f)
		f.ExpireLease(t, stale.Receipt.ItemID)
		successor := claimOne(t, f)
		if successor.Receipt.LeaseGeneration <= stale.Receipt.LeaseGeneration {
			t.Fatalf("takeover generation %d, want above %d", successor.Receipt.LeaseGeneration, stale.Receipt.LeaseGeneration)
		}
		_, err := f.Stores.PRMergeability.CompleteProbe(ctx, stale.Receipt, db.PRMergeabilityAnswer{Found: true, Mergeable: domain.MergeableConflicting, HeadSHA: "h"}, "")
		if !errors.Is(err, workitem.ErrLeaseLost) {
			t.Fatalf("stale CompleteProbe = %v, want ErrLeaseLost", err)
		}
		if row, _ := f.Stores.PRMergeability.GetSystem(ctx, f.OrgID, e.ID); row != nil {
			t.Errorf("row = %+v, want none", row)
		}
		if q := queued(t, f, e.ID); len(q) != 0 {
			t.Errorf("event queue = %+v, want nothing", q)
		}
		if rows := f.Probes(t, e.ID); len(rows) != 1 || rows[0].Status != workitem.StatusLeased {
			t.Errorf("probes = %+v, want the one probe still leased by its new holder", rows)
		}
		if _, err := f.Stores.PRMergeability.CompleteProbe(ctx, stale.Receipt, db.PRMergeabilityAnswer{Found: true, Mergeable: domain.MergeableUnknown}, ""); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Fatalf("stale UNKNOWN CompleteProbe = %v, want ErrLeaseLost", err)
		}
		if rows := f.Probes(t, e.ID); len(rows) != 1 {
			t.Errorf("a stale UNKNOWN completion admitted a retry: %+v", rows)
		}
	})

	t.Run("A_rate_limit_defers_the_probe_and_refunds_its_attempt", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "12")
		admit(t, f, e.ID, domain.MergeabilityProbeNew)
		item := claimOne(t, f)
		if err := f.Stores.PRMergeability.DeferRateLimited(ctx, item.Receipt, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("DeferRateLimited: %v", err)
		}
		if rows := f.Probes(t, e.ID); len(rows) != 1 || rows[0].Status != workitem.StatusReady || rows[0].Attempt != 0 {
			t.Fatalf("probes = %+v, want the one probe ready with its attempt refunded", rows)
		}
		f.Ripen(t)
		if batch, err := f.Stores.PRMergeability.Claim(ctx, mergeabilityOwner, f.OrgID, 100); err != nil || len(batch.Items) != 0 {
			t.Errorf("Claim before the reset = %d items (err %v), want none", len(batch.Items), err)
		}
		if err := f.Stores.PRMergeability.DeferRateLimited(ctx, item.Receipt, time.Now().Add(time.Hour)); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("a second deferral on the same receipt = %v, want ErrLeaseLost", err)
		}
	})

	t.Run("Rows_list_and_inactive_ones_delete", func(t *testing.T) {
		f := mk(t)
		open, closed := pr(t, f, "8"), pr(t, f, "9")
		probe(t, f, open.ID, domain.MergeableMergeable, "a")
		probe(t, f, closed.ID, domain.MergeableConflicting, "b")
		rows, err := f.Stores.PRMergeability.ListSystem(ctx, f.OrgID)
		if err != nil || len(rows) != 2 {
			t.Fatalf("ListSystem = %d rows (err %v), want 2", len(rows), err)
		}
		if _, err := f.Stores.Entities.CloseSystem(ctx, f.OrgID, closed.ID); err != nil {
			t.Fatalf("close entity: %v", err)
		}
		if n, err := f.Stores.PRMergeability.DeleteInactiveSystem(ctx, f.OrgID); err != nil || n != 1 {
			t.Fatalf("DeleteInactiveSystem = (%d, %v), want 1", n, err)
		}
		rows, _ = f.Stores.PRMergeability.ListSystem(ctx, f.OrgID)
		if len(rows) != 1 || rows[0].EntityID != open.ID {
			t.Errorf("rows after the delete = %+v, want the open pull request's alone", rows)
		}
		if got, err := f.Stores.PRMergeability.GetSystem(ctx, f.OrgID, closed.ID); err != nil || got != nil {
			t.Errorf("GetSystem of a deleted row = (%+v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("Prune_deletes_settled_probes_and_keeps_the_rest", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "10")
		probe(t, f, e.ID, domain.MergeableMergeable, "a")
		admit(t, f, e.ID, domain.MergeabilityProbeSweep)
		if n, err := f.Stores.PRMergeability.PruneSettled(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
			t.Fatalf("prune before an hour ago = (%d, %v), want nothing", n, err)
		}
		if n, err := f.Stores.PRMergeability.PruneSettled(ctx, time.Now().Add(time.Minute)); err != nil || n != 1 {
			t.Fatalf("prune = (%d, %v), want the done probe", n, err)
		}
		if rows := f.Probes(t, e.ID); len(rows) != 1 || rows[0].Status != workitem.StatusReady {
			t.Errorf("probes after the prune = %+v, want the ready one", rows)
		}
	})

	t.Run("Describe_names_the_pull_request", func(t *testing.T) {
		f := mk(t)
		e := pr(t, f, "11")
		admit(t, f, e.ID, domain.MergeabilityProbeHeadMoved)
		id := f.Probes(t, e.ID)[0].ID
		got, err := f.Stores.PRMergeability.(db.WorkKindHandle).Describe(ctx, f.OrgID, []int64{id})
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		subj, ok := got[id]
		if !ok || subj.Label != "octo/api#11" || subj.Detail != "PR 11" || subj.Fields["reason"] != domain.MergeabilityProbeHeadMoved || subj.Fields["entity_id"] != e.ID {
			t.Errorf("subject = %+v, want the pull request's key, title and the probe's reason", subj)
		}
	})
}
