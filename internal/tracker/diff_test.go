package tracker

import (
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

const testEntityID = "entity-123"
const testUser = "aidan"

// testDoneStatuses matches the pre-existing hardcoded terminal set, kept so
// the Jira diff tests continue exercising the Done/Closed/Resolved branch.
var testDoneStatuses = []string{"Done", "Closed", "Resolved"}

// --- Helpers ----------------------------------------------------------------

func eventTypes(evts []domain.Event) []string {
	var out []string
	for _, e := range evts {
		out = append(out, e.EventType)
	}
	return out
}

func findEvent(evts []domain.Event, eventType string) *domain.Event {
	for i := range evts {
		if evts[i].EventType == eventType {
			return &evts[i]
		}
	}
	return nil
}

func findEvents(evts []domain.Event, eventType string) []domain.Event {
	var out []domain.Event
	for _, e := range evts {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

func assertEntityID(t *testing.T, evt domain.Event) {
	t.Helper()
	if evt.EntityID == nil || *evt.EntityID != testEntityID {
		t.Errorf("event %s: expected EntityID=%q, got %v", evt.EventType, testEntityID, evt.EntityID)
	}
}

func decodeMetadata[T any](t *testing.T, evt domain.Event) T {
	t.Helper()
	var m T
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
		t.Fatalf("failed to decode metadata for %s: %v", evt.EventType, err)
	}
	return m
}

// basePRSnapshot returns a minimal open PR snapshot for use as a "previous" state.
func basePRSnapshot() domain.PRSnapshot {
	return domain.PRSnapshot{
		Number:    42,
		Title:     "Test PR",
		Author:    testUser,
		Repo:      "owner/repo",
		URL:       "https://github.com/owner/repo/pull/42",
		State:     "OPEN",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{}, // empty but non-nil = known prior state
		Labels:    []string{},
	}
}

// --- First discovery --------------------------------------------------------

func TestDiff_FirstDiscovery_OpenPR_NoEvents(t *testing.T) {
	// First discovery of an open PR should emit no events — events fire
	// on the NEXT poll when we can meaningfully diff.
	curr := basePRSnapshot()
	evts := DiffPRSnapshots(domain.PRSnapshot{}, curr, testEntityID, testUser, nil)
	if len(evts) != 0 {
		t.Errorf("expected 0 events on first discovery of open PR, got %d: %v", len(evts), eventTypes(evts))
	}
}

func TestDiff_FirstDiscovery_MergedPR_EmitsMerged(t *testing.T) {
	curr := basePRSnapshot()
	curr.Merged = true
	evts := DiffPRSnapshots(domain.PRSnapshot{}, curr, testEntityID, testUser, nil)
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRMerged {
		t.Errorf("expected [pr:merged], got %v", eventTypes(evts))
	}
	assertEntityID(t, evts[0])
}

func TestDiff_FirstDiscovery_ClosedPR_EmitsClosed(t *testing.T) {
	curr := basePRSnapshot()
	curr.State = "CLOSED"
	evts := DiffPRSnapshots(domain.PRSnapshot{}, curr, testEntityID, testUser, nil)
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRClosed {
		t.Errorf("expected [pr:closed], got %v", eventTypes(evts))
	}
}

// --- CI per-check events ----------------------------------------------------

func TestDiff_CI_NewFailingCheck_EmitsPerCheck(t *testing.T) {
	prev := basePRSnapshot()
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure", WorkflowRunID: 555},
		{ID: 2, Name: "test", Conclusion: "failure"}, // third-party CI — no workflow run
		{ID: 3, Name: "lint", Conclusion: "success", WorkflowRunID: 777},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)

	failed := findEvents(evts, domain.EventGitHubPRCICheckFailed)
	passed := findEvents(evts, domain.EventGitHubPRCICheckPassed)

	if len(failed) != 2 {
		t.Errorf("expected 2 ci_check_failed events, got %d", len(failed))
	}
	if len(passed) != 1 {
		t.Errorf("expected 1 ci_check_passed event, got %d", len(passed))
	}

	// Verify metadata on the first failed check — includes workflow_run_id
	// for Actions-backed checks so auto-delegated CI-fix prompts can
	// download logs without a discovery round-trip.
	meta := decodeMetadata[events.GitHubPRCICheckFailedMetadata](t, failed[0])
	if !meta.AuthorIsSelf {
		t.Error("expected AuthorIsSelf=true")
	}
	if meta.Repo != "owner/repo" {
		t.Errorf("expected Repo=owner/repo, got %s", meta.Repo)
	}
	if meta.WorkflowRunID != 555 {
		t.Errorf("expected WorkflowRunID=555 on first failed check, got %d", meta.WorkflowRunID)
	}

	// Second failed check has no workflow run ID (third-party CI shape).
	meta2 := decodeMetadata[events.GitHubPRCICheckFailedMetadata](t, failed[1])
	if meta2.WorkflowRunID != 0 {
		t.Errorf("expected WorkflowRunID=0 for third-party check, got %d", meta2.WorkflowRunID)
	}

	// Passed check carries its workflow run ID too.
	passedMeta := decodeMetadata[events.GitHubPRCICheckPassedMetadata](t, passed[0])
	if passedMeta.WorkflowRunID != 777 {
		t.Errorf("expected WorkflowRunID=777 on passed check, got %d", passedMeta.WorkflowRunID)
	}
}

func TestDiff_CI_SameFailingCheckID_NoEvent(t *testing.T) {
	// If the same check-run ID was already failing, don't re-emit.
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"},
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"}, // same ID, still failing
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	failed := findEvents(evts, domain.EventGitHubPRCICheckFailed)
	if len(failed) != 0 {
		t.Errorf("expected 0 ci_check_failed (same ID still failing), got %d", len(failed))
	}
}

func TestDiff_CI_NewExecutionSameCheck_EmitsEvent(t *testing.T) {
	// A new check-run ID for the same check name (retry/new commit) should fire.
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"},
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"}, // old, kept
		{ID: 2, Name: "build", Conclusion: "failure"}, // new execution
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	failed := findEvents(evts, domain.EventGitHubPRCICheckFailed)
	if len(failed) != 1 {
		t.Errorf("expected 1 ci_check_failed (new execution ID), got %d", len(failed))
	}
}

func TestDiff_CI_PendingToFailure_EmitsEvent(t *testing.T) {
	// Check was pending (not failing) last poll, now failed = new signal.
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: ""}, // pending
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	failed := findEvents(evts, domain.EventGitHubPRCICheckFailed)
	if len(failed) != 1 {
		t.Errorf("expected 1 ci_check_failed (pending→failure), got %d", len(failed))
	}
}

func TestDiff_CI_NilPrevCheckRuns_TreatedAsEmpty(t *testing.T) {
	// nil prev.CheckRuns (discovery snapshot) is treated as empty — every
	// check in curr is new. This ensures failing CI on a newly-tracked
	// entity fires on the first full refresh.
	prev := basePRSnapshot()
	prev.CheckRuns = nil // discovery snapshot
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"},
		{ID: 2, Name: "lint", Conclusion: "success"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	failed := findEvents(evts, domain.EventGitHubPRCICheckFailed)
	passed := findEvents(evts, domain.EventGitHubPRCICheckPassed)
	if len(failed) != 1 {
		t.Errorf("expected 1 ci_check_failed (nil prev = empty), got %d", len(failed))
	}
	if len(passed) != 1 {
		t.Errorf("expected 1 ci_check_passed (nil prev = empty), got %d", len(passed))
	}
}

func TestDiff_CI_NilCurrCheckRuns_SkipsCISection(t *testing.T) {
	// nil curr.CheckRuns means the refresh didn't return CI data (e.g.,
	// lightweight terminal fragment). Skip the CI section entirely.
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"},
	}
	curr := basePRSnapshot()
	curr.CheckRuns = nil

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if len(findEvents(evts, domain.EventGitHubPRCICheckPassed)) != 0 {
		t.Error("should not emit ci events when curr.CheckRuns is nil")
	}
	if len(findEvents(evts, domain.EventGitHubPRCICheckFailed)) != 0 {
		t.Error("should not emit ci events when curr.CheckRuns is nil")
	}
}

func TestDiff_CI_FailureToSuccess_EmitsCheckPassed(t *testing.T) {
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "failure"},
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "build", Conclusion: "success"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	passed := findEvents(evts, domain.EventGitHubPRCICheckPassed)
	if len(passed) != 1 {
		t.Errorf("expected 1 ci_check_passed, got %d", len(passed))
	}
	// Should NOT emit ci_check_failed.
	if len(findEvents(evts, domain.EventGitHubPRCICheckFailed)) != 0 {
		t.Error("should not emit ci_check_failed when check is now passing")
	}
}

func TestDiff_CI_FailureToSkipped_EmitsCheckPassed(t *testing.T) {
	// A check that was failing and transitions to skipped (e.g., path filter
	// excludes the changed files on a new commit) should emit ci_check_passed
	// so ci_check_failed tasks can close via inline check.
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "integration", Conclusion: "failure"},
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "integration", Conclusion: "skipped"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	passed := findEvents(evts, domain.EventGitHubPRCICheckPassed)
	if len(passed) != 1 {
		t.Errorf("expected 1 ci_check_passed (failure→skipped), got %d", len(passed))
	}
	meta := decodeMetadata[events.GitHubPRCICheckPassedMetadata](t, passed[0])
	if meta.Conclusion != "skipped" {
		t.Errorf("expected Conclusion=skipped, got %s", meta.Conclusion)
	}
}

func TestDiff_CI_FailureToNeutral_EmitsCheckPassed(t *testing.T) {
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "lint", Conclusion: "failure"},
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "lint", Conclusion: "neutral"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	passed := findEvents(evts, domain.EventGitHubPRCICheckPassed)
	if len(passed) != 1 {
		t.Errorf("expected 1 ci_check_passed (failure→neutral), got %d", len(passed))
	}
}

func TestDiff_CI_SkippedToSkipped_NoEvent(t *testing.T) {
	// Already non-failing → still non-failing = no event.
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "optional", Conclusion: "skipped"},
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "optional", Conclusion: "skipped"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if len(findEvents(evts, domain.EventGitHubPRCICheckPassed)) != 0 {
		t.Error("should not emit ci_check_passed when already non-failing")
	}
}

func TestDiff_CI_PendingToSkipped_EmitsCheckPassed(t *testing.T) {
	// Pending (empty conclusion) → skipped = new non-failing conclusion.
	prev := basePRSnapshot()
	prev.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "optional", Conclusion: ""}, // pending
	}
	curr := basePRSnapshot()
	curr.CheckRuns = []domain.CheckRun{
		{ID: 1, Name: "optional", Conclusion: "skipped"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	passed := findEvents(evts, domain.EventGitHubPRCICheckPassed)
	if len(passed) != 1 {
		t.Errorf("expected 1 ci_check_passed (pending→skipped), got %d", len(passed))
	}
}

// --- Reviews ----------------------------------------------------------------

func TestDiff_Review_NewChangesRequested(t *testing.T) {
	prev := basePRSnapshot()
	curr := basePRSnapshot()
	curr.Reviews = []domain.ReviewState{
		{Author: "alice", State: "CHANGES_REQUESTED"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	evt := findEvent(evts, domain.EventGitHubPRReviewChangesRequested)
	if evt == nil {
		t.Fatal("expected review_changes_requested event")
	}
	meta := decodeMetadata[events.GitHubPRReviewChangesRequestedMetadata](t, *evt)
	if meta.Reviewer != "alice" {
		t.Errorf("expected Reviewer=alice, got %s", meta.Reviewer)
	}
	if meta.ReviewerIsSelf {
		t.Error("expected ReviewerIsSelf=false for alice")
	}
	if !meta.AuthorIsSelf {
		t.Error("expected AuthorIsSelf=true (PR author is testUser)")
	}
}

func TestDiff_Review_SameState_NoEvent(t *testing.T) {
	prev := basePRSnapshot()
	prev.Reviews = []domain.ReviewState{
		{Author: "alice", State: "APPROVED"},
	}
	curr := basePRSnapshot()
	curr.Reviews = []domain.ReviewState{
		{Author: "alice", State: "APPROVED"}, // unchanged
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	reviewEvts := findEvents(evts, domain.EventGitHubPRReviewApproved)
	if len(reviewEvts) != 0 {
		t.Errorf("expected no review events (state unchanged), got %d", len(reviewEvts))
	}
}

func TestDiff_Review_SelfReview_EmitsSubmitted(t *testing.T) {
	prev := basePRSnapshot()
	prev.Author = "bob" // not self — so this is someone else's PR
	curr := basePRSnapshot()
	curr.Author = "bob"
	curr.Reviews = []domain.ReviewState{
		{Author: testUser, State: "COMMENTED"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)

	// Should emit review_commented (the specific type) AND review_submitted (I reviewed).
	commented := findEvent(evts, domain.EventGitHubPRReviewCommented)
	submitted := findEvent(evts, domain.EventGitHubPRReviewSubmitted)
	if commented == nil {
		t.Error("expected review_commented event")
	}
	if submitted == nil {
		t.Fatal("expected review_submitted event for self-review")
	}
	meta := decodeMetadata[events.GitHubPRReviewSubmittedMetadata](t, *submitted)
	if !meta.ReviewerIsSelf {
		t.Error("expected ReviewerIsSelf=true on submitted event")
	}
	if meta.ReviewType != "commented" {
		t.Errorf("expected ReviewType=commented, got %s", meta.ReviewType)
	}
}

func TestDiff_Review_MultipleReviewers_IndependentEvents(t *testing.T) {
	prev := basePRSnapshot()
	prev.Reviews = []domain.ReviewState{
		{Author: "alice", State: "COMMENTED"},
	}
	curr := basePRSnapshot()
	curr.Reviews = []domain.ReviewState{
		{Author: "alice", State: "APPROVED"},        // changed
		{Author: "bob", State: "CHANGES_REQUESTED"}, // new
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	approved := findEvents(evts, domain.EventGitHubPRReviewApproved)
	changes := findEvents(evts, domain.EventGitHubPRReviewChangesRequested)

	if len(approved) != 1 {
		t.Errorf("expected 1 approved event (alice), got %d", len(approved))
	}
	if len(changes) != 1 {
		t.Errorf("expected 1 changes_requested event (bob), got %d", len(changes))
	}
}

// --- Review requests --------------------------------------------------------

func TestDiff_ReviewRequested_ForSelf(t *testing.T) {
	prev := basePRSnapshot()
	prev.Author = "bob"
	curr := basePRSnapshot()
	curr.Author = "bob"
	curr.ReviewRequests = []string{testUser}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRReviewRequested) == nil {
		t.Error("expected review_requested event when self added to requests")
	}
}

func TestDiff_ReviewRequested_ForOther_NoEvent(t *testing.T) {
	prev := basePRSnapshot()
	curr := basePRSnapshot()
	curr.ReviewRequests = []string{"someone-else"}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRReviewRequested) != nil {
		t.Error("should not emit review_requested when the request is for someone else")
	}
}

func TestDiff_ReviewRequested_AlreadyPresent_NoEvent(t *testing.T) {
	prev := basePRSnapshot()
	prev.Author = "bob"
	prev.ReviewRequests = []string{testUser}
	curr := basePRSnapshot()
	curr.Author = "bob"
	curr.ReviewRequests = []string{testUser}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRReviewRequested) != nil {
		t.Error("should not re-emit review_requested when already in list")
	}
}

func TestDiff_ReviewRequested_ViaTeam(t *testing.T) {
	// Reviewer list contains only the team ("eng/pulsar"), not the user's
	// login. With the user's team memberships passed in, this should still
	// fire review_requested.
	prev := basePRSnapshot()
	prev.Author = "bob"
	curr := basePRSnapshot()
	curr.Author = "bob"
	curr.ReviewRequests = []string{"eng/pulsar"}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, []string{"eng/pulsar"})
	if findEvent(evts, domain.EventGitHubPRReviewRequested) == nil {
		t.Error("expected review_requested event when user's team is added to requests")
	}
}

func TestDiff_ReviewRequested_ViaTeam_AlreadyPresent_NoEvent(t *testing.T) {
	// Team was already requested — no transition, no re-emit.
	prev := basePRSnapshot()
	prev.Author = "bob"
	prev.ReviewRequests = []string{"eng/pulsar"}
	curr := basePRSnapshot()
	curr.Author = "bob"
	curr.ReviewRequests = []string{"eng/pulsar"}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, []string{"eng/pulsar"})
	if findEvent(evts, domain.EventGitHubPRReviewRequested) != nil {
		t.Error("should not re-emit review_requested when team was already requested")
	}
}

func TestDiff_ReviewRequested_OtherTeam_NoEvent(t *testing.T) {
	// A team we're not on is requested — no event.
	prev := basePRSnapshot()
	curr := basePRSnapshot()
	curr.ReviewRequests = []string{"eng/other-team"}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, []string{"eng/pulsar"})
	if findEvent(evts, domain.EventGitHubPRReviewRequested) != nil {
		t.Error("should not emit review_requested for a team the user isn't on")
	}
}

func TestDiff_ReviewRequested_SelfAuthoredViaTeam_NoEvent(t *testing.T) {
	// PR authored by the session user; their team gets auto-assigned via
	// CODEOWNERS. This is a reviewer-pool artifact, not an ask — no event.
	prev := basePRSnapshot()
	prev.Author = testUser
	curr := basePRSnapshot()
	curr.Author = testUser
	curr.ReviewRequests = []string{"eng/pulsar"}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, []string{"eng/pulsar"})
	if findEvent(evts, domain.EventGitHubPRReviewRequested) != nil {
		t.Error("should not emit review_requested when PR is self-authored (team auto-assigned to own PR)")
	}
}

// --- Labels -----------------------------------------------------------------

func TestDiff_Labels_AddAndRemove(t *testing.T) {
	prev := basePRSnapshot()
	prev.Labels = []string{"wip", "bug"}
	curr := basePRSnapshot()
	curr.Labels = []string{"bug", "urgent"} // wip removed, urgent added

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)

	added := findEvents(evts, domain.EventGitHubPRLabelAdded)
	removed := findEvents(evts, domain.EventGitHubPRLabelRemoved)

	if len(added) != 1 {
		t.Errorf("expected 1 label_added, got %d", len(added))
	}
	if len(removed) != 1 {
		t.Errorf("expected 1 label_removed, got %d", len(removed))
	}

	// Check dedup_key is set to the label name.
	if added[0].DedupKey != "urgent" {
		t.Errorf("expected dedup_key=urgent, got %s", added[0].DedupKey)
	}
	if removed[0].DedupKey != "wip" {
		t.Errorf("expected dedup_key=wip, got %s", removed[0].DedupKey)
	}

	// Verify metadata has the label snapshot AFTER the change.
	meta := decodeMetadata[events.GitHubPRLabelAddedMetadata](t, added[0])
	if meta.LabelName != "urgent" {
		t.Errorf("expected LabelName=urgent, got %s", meta.LabelName)
	}
}

func TestDiff_Labels_NoChange_NoEvents(t *testing.T) {
	prev := basePRSnapshot()
	prev.Labels = []string{"bug", "wip"}
	curr := basePRSnapshot()
	curr.Labels = []string{"wip", "bug"} // same set, different order

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if len(findEvents(evts, domain.EventGitHubPRLabelAdded)) != 0 {
		t.Error("should not emit label_added when labels are the same set")
	}
	if len(findEvents(evts, domain.EventGitHubPRLabelRemoved)) != 0 {
		t.Error("should not emit label_removed when labels are the same set")
	}
}

// --- State transitions ------------------------------------------------------

func TestDiff_Merged(t *testing.T) {
	prev := basePRSnapshot()
	curr := basePRSnapshot()
	curr.Merged = true

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRMerged) == nil {
		t.Error("expected pr:merged event")
	}
}

func TestDiff_Closed(t *testing.T) {
	prev := basePRSnapshot()
	curr := basePRSnapshot()
	curr.State = "CLOSED"

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRClosed) == nil {
		t.Error("expected pr:closed event")
	}
}

func TestDiff_ReadyForReview(t *testing.T) {
	prev := basePRSnapshot()
	prev.IsDraft = true
	curr := basePRSnapshot()
	curr.IsDraft = false

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRReadyForReview) == nil {
		t.Error("expected pr:ready_for_review event")
	}
}

func TestDiff_NewCommits(t *testing.T) {
	prev := basePRSnapshot()
	prev.HeadSHA = "aaa"
	curr := basePRSnapshot()
	curr.HeadSHA = "bbb"

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	evt := findEvent(evts, domain.EventGitHubPRNewCommits)
	if evt == nil {
		t.Fatal("expected pr:new_commits event")
	}
	meta := decodeMetadata[events.GitHubPRNewCommitsMetadata](t, *evt)
	if meta.PrevHeadSHA != "aaa" || meta.HeadSHA != "bbb" {
		t.Errorf("wrong SHAs: prev=%s head=%s", meta.PrevHeadSHA, meta.HeadSHA)
	}
}

func TestDiff_NewCommits_EmptyPrev_NoEvent(t *testing.T) {
	prev := basePRSnapshot()
	prev.HeadSHA = "" // unknown prior
	curr := basePRSnapshot()
	curr.HeadSHA = "bbb"

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRNewCommits) != nil {
		t.Error("should not emit new_commits when prev SHA is empty")
	}
}

func TestDiff_Conflicts(t *testing.T) {
	prev := basePRSnapshot()
	prev.Mergeable = "MERGEABLE"
	curr := basePRSnapshot()
	curr.Mergeable = "CONFLICTING"

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRConflicts) == nil {
		t.Error("expected pr:conflicts event")
	}
}

func TestDiff_Conflicts_AlreadyConflicting_NoEvent(t *testing.T) {
	prev := basePRSnapshot()
	prev.Mergeable = "CONFLICTING"
	curr := basePRSnapshot()
	curr.Mergeable = "CONFLICTING"

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	if findEvent(evts, domain.EventGitHubPRConflicts) != nil {
		t.Error("should not re-emit conflicts when already conflicting")
	}
}

// --- Metadata: Labels snapshot on every event -------------------------------

func TestDiff_AllPREvents_CarryLabels(t *testing.T) {
	prev := basePRSnapshot()
	prev.HeadSHA = "aaa"
	curr := basePRSnapshot()
	curr.HeadSHA = "bbb"
	curr.Labels = []string{"self-review", "wip"}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	// new_commits should fire; check its metadata has Labels.
	evt := findEvent(evts, domain.EventGitHubPRNewCommits)
	if evt == nil {
		t.Fatal("expected new_commits event")
	}
	meta := decodeMetadata[events.GitHubPRNewCommitsMetadata](t, *evt)
	if len(meta.Labels) != 2 {
		t.Errorf("expected 2 labels in metadata, got %d", len(meta.Labels))
	}
}

// --- Metadata: AuthorIsSelf -------------------------------------------------

func TestDiff_AuthorIsSelf_True(t *testing.T) {
	prev := basePRSnapshot()
	prev.HeadSHA = "aaa"
	curr := basePRSnapshot()
	curr.HeadSHA = "bbb"
	curr.Author = testUser

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	evt := findEvent(evts, domain.EventGitHubPRNewCommits)
	if evt == nil {
		t.Fatal("expected event")
	}
	meta := decodeMetadata[events.GitHubPRNewCommitsMetadata](t, *evt)
	if !meta.AuthorIsSelf {
		t.Error("expected AuthorIsSelf=true when Author matches username")
	}
}

func TestDiff_AuthorIsSelf_False(t *testing.T) {
	prev := basePRSnapshot()
	prev.HeadSHA = "aaa"
	curr := basePRSnapshot()
	curr.HeadSHA = "bbb"
	curr.Author = "someone-else"

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)
	evt := findEvent(evts, domain.EventGitHubPRNewCommits)
	if evt == nil {
		t.Fatal("expected event")
	}
	meta := decodeMetadata[events.GitHubPRNewCommitsMetadata](t, *evt)
	if meta.AuthorIsSelf {
		t.Error("expected AuthorIsSelf=false when Author differs from username")
	}
}

// --- Jira -------------------------------------------------------------------

func TestDiffJira_FirstDiscovery_Assigned(t *testing.T) {
	curr := domain.JiraSnapshot{
		Key:      "SKY-123",
		Summary:  "Fix the thing",
		Status:   "In Progress",
		Assignee: testUser,
		Priority: "High",
	}
	evts := DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, testUser, testDoneStatuses)
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueAssigned {
		t.Errorf("expected [jira:issue:assigned], got %v", eventTypes(evts))
	}
	meta := decodeMetadata[events.JiraIssueAssignedMetadata](t, evts[0])
	if !meta.AssigneeIsSelf {
		t.Error("expected AssigneeIsSelf=true")
	}
}

func TestDiffJira_FirstDiscovery_Available(t *testing.T) {
	curr := domain.JiraSnapshot{
		Key:    "SKY-124",
		Status: "To Do",
		// no Assignee
	}
	evts := DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, testUser, testDoneStatuses)
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueAvailable {
		t.Errorf("expected [jira:issue:available], got %v", eventTypes(evts))
	}
}

func TestDiffJira_FirstDiscovery_Completed(t *testing.T) {
	curr := domain.JiraSnapshot{Key: "SKY-125", Status: "Done"}
	evts := DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, testUser, testDoneStatuses)
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueCompleted {
		t.Errorf("expected [jira:issue:completed], got %v", eventTypes(evts))
	}
}

func TestDiffJira_StatusChanged_DedupKey(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "In Review"}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	evt := findEvent(evts, domain.EventJiraIssueStatusChanged)
	if evt == nil {
		t.Fatal("expected status_changed event")
	}
	// dedup_key should be the NEW status name (open-set discriminator).
	if evt.DedupKey != "In Review" {
		t.Errorf("expected dedup_key='In Review', got %q", evt.DedupKey)
	}
}

func TestDiffJira_StatusChanged_Terminal_AlsoEmitsCompleted(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "In Progress"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "Done"}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	types := eventTypes(evts)
	hasStatusChanged := false
	hasCompleted := false
	for _, t := range types {
		if t == domain.EventJiraIssueStatusChanged {
			hasStatusChanged = true
		}
		if t == domain.EventJiraIssueCompleted {
			hasCompleted = true
		}
	}
	if !hasStatusChanged {
		t.Error("expected status_changed event")
	}
	if !hasCompleted {
		t.Error("expected completed event for terminal status")
	}
}

func TestDiffJira_Reassigned(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Assignee: testUser}
	curr := domain.JiraSnapshot{Key: "SKY-1", Assignee: "bob"}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	evt := findEvent(evts, domain.EventJiraIssueAssigned)
	if evt == nil {
		t.Fatal("expected assigned event on reassignment")
	}
	meta := decodeMetadata[events.JiraIssueAssignedMetadata](t, *evt)
	if meta.AssigneeIsSelf {
		t.Error("expected AssigneeIsSelf=false when reassigned to bob")
	}
}

func TestDiffJira_Unassigned(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Assignee: testUser}
	curr := domain.JiraSnapshot{Key: "SKY-1", Assignee: ""}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueAvailable) == nil {
		t.Error("expected available event when assignee removed")
	}
}

func TestDiffJira_PriorityChanged_DedupKey(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Priority: "Low"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Priority: "High"}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	evt := findEvent(evts, domain.EventJiraIssuePriorityChanged)
	if evt == nil {
		t.Fatal("expected priority_changed event")
	}
	if evt.DedupKey != "High" {
		t.Errorf("expected dedup_key='High', got %q", evt.DedupKey)
	}
}

func TestDiffJira_NewComment(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 3}
	curr := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 5}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueCommented) == nil {
		t.Error("expected commented event when comment count increases")
	}
}

func TestDiffJira_CommentCountDecrease_NoEvent(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 5}
	curr := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 3} // deleted comments

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueCommented) != nil {
		t.Error("should not emit commented when count decreases")
	}
}

// --- Jira: subtask gating (SKY-173) ----------------------------------------

func TestDiffJira_FirstDiscovery_OpenSubtasks_NoEvents(t *testing.T) {
	// Parent ticket discovered with open subtasks: suppress assigned/available
	// so it doesn't clutter the queue. Entity is still created (tracker.go
	// handles that); this test just confirms no events fire.
	curr := domain.JiraSnapshot{
		Key:              "SKY-200",
		Summary:          "Epic: Rebuild auth",
		Status:           "In Progress",
		Assignee:         testUser,
		Priority:         "High",
		OpenSubtaskCount: 2,
	}
	evts := DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, testUser, testDoneStatuses)
	if len(evts) != 0 {
		t.Errorf("expected 0 events for parent-with-subtasks on first discovery, got %d: %v",
			len(evts), eventTypes(evts))
	}
}

func TestDiffJira_FirstDiscovery_OpenSubtasks_AvailableSuppressed(t *testing.T) {
	// Same gate for the unassigned/available path — a parent in pickup
	// status with open subtasks shouldn't surface either.
	curr := domain.JiraSnapshot{
		Key:              "SKY-201",
		Status:           "To Do",
		OpenSubtaskCount: 1,
		// no Assignee — would normally emit Available
	}
	evts := DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, testUser, testDoneStatuses)
	if len(evts) != 0 {
		t.Errorf("expected 0 events for unassigned parent-with-subtasks, got %d: %v",
			len(evts), eventTypes(evts))
	}
}

func TestDiffJira_FirstDiscovery_TerminalWithSubtasks_EmitsCompleted(t *testing.T) {
	// Terminal parents still emit Completed regardless of subtasks — that
	// branch runs before the subtask gate. The entity-lifecycle handler
	// uses Completed to close the entity, and we want that to happen even
	// if the parent had dangling subtasks when it was force-closed.
	curr := domain.JiraSnapshot{
		Key:              "SKY-202",
		Status:           "Done",
		Assignee:         testUser,
		OpenSubtaskCount: 3,
	}
	evts := DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, testUser, testDoneStatuses)
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueCompleted {
		t.Errorf("expected [jira:issue:completed] even with open subtasks, got %v", eventTypes(evts))
	}
}

func TestDiffJira_BecameAtomic_DownwardTransition(t *testing.T) {
	// Parent had one open subtask, subtask closes -> count goes 1 -> 0.
	// Belated discovery path: fire became_atomic so the task-rule can
	// create the queued task that was suppressed on first poll.
	prev := domain.JiraSnapshot{
		Key:              "SKY-300",
		Status:           "In Progress",
		Assignee:         testUser,
		OpenSubtaskCount: 1,
	}
	curr := domain.JiraSnapshot{
		Key:              "SKY-300",
		Status:           "In Progress",
		Assignee:         testUser,
		Priority:         "High",
		Summary:          "Rework the auth flow",
		IssueType:        "Story",
		OpenSubtaskCount: 0,
	}
	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	evt := findEvent(evts, domain.EventJiraIssueBecameAtomic)
	if evt == nil {
		t.Fatalf("expected became_atomic on 1->0 transition, got events: %v", eventTypes(evts))
	}
	meta := decodeMetadata[events.JiraIssueBecameAtomicMetadata](t, *evt)
	if !meta.AssigneeIsSelf {
		t.Error("expected AssigneeIsSelf=true")
	}
	if meta.IssueKey != "SKY-300" || meta.Project != "SKY" {
		t.Errorf("unexpected key/project: %s / %s", meta.IssueKey, meta.Project)
	}
	if meta.Priority != "High" || meta.IssueType != "Story" {
		t.Errorf("metadata missing snapshot fields: priority=%q type=%q", meta.Priority, meta.IssueType)
	}
}

func TestDiffJira_BecameAtomic_NoTransition_NoEvent(t *testing.T) {
	// Never had subtasks (0->0): the ticket was atomic throughout. No
	// became_atomic event should fire — it's strictly a downward-transition
	// signal, not a repeat-on-every-poll heartbeat.
	prev := domain.JiraSnapshot{Key: "SKY-301", OpenSubtaskCount: 0, Assignee: testUser}
	curr := domain.JiraSnapshot{Key: "SKY-301", OpenSubtaskCount: 0, Assignee: testUser}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueBecameAtomic) != nil {
		t.Error("should not emit became_atomic when prev count was already 0")
	}
}

func TestDiffJira_BecameAtomic_AddedThenRemoved_NoEventOnAdd(t *testing.T) {
	// Upward transition (0 -> N, subtasks appear on an atomic ticket) is
	// deliberately NOT an event — we don't retroactively do anything to
	// the existing task, the UI pill surfaces the change instead. Confirm
	// no became_atomic fires here.
	prev := domain.JiraSnapshot{Key: "SKY-302", OpenSubtaskCount: 0, Assignee: testUser}
	curr := domain.JiraSnapshot{Key: "SKY-302", OpenSubtaskCount: 2, Assignee: testUser}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueBecameAtomic) != nil {
		t.Error("became_atomic should only fire on downward transition, not upward")
	}
}

func TestDiffJira_Reassigned_WithOpenSubtasks_NoEvent(t *testing.T) {
	// Reassignment on a parent that still has open subtasks: the subtask
	// gate has to apply to this path too, not just first-discovery —
	// otherwise a non-atomic parent could sneak a task into the queue via
	// reassignment after the initial suppression. became_atomic handles
	// the belated-discovery path once the decomposition collapses.
	prev := domain.JiraSnapshot{
		Key:              "SKY-310",
		Assignee:         "",
		OpenSubtaskCount: 2,
	}
	curr := domain.JiraSnapshot{
		Key:              "SKY-310",
		Assignee:         testUser,
		OpenSubtaskCount: 2,
	}
	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueAssigned) != nil {
		t.Error("should not emit assigned on reassignment while parent still has open subtasks")
	}
	if findEvent(evts, domain.EventJiraIssueAvailable) != nil {
		t.Error("should not emit available on reassignment while parent still has open subtasks")
	}
}

func TestDiffJira_Unassigned_WithOpenSubtasks_NoEvent(t *testing.T) {
	// Same gate on the opposite direction (assigned -> unassigned).
	prev := domain.JiraSnapshot{
		Key:              "SKY-311",
		Assignee:         testUser,
		OpenSubtaskCount: 1,
	}
	curr := domain.JiraSnapshot{
		Key:              "SKY-311",
		Assignee:         "",
		OpenSubtaskCount: 1,
	}
	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueAvailable) != nil {
		t.Error("should not emit available on unassignment while parent still has open subtasks")
	}
}

func TestDiffJira_Reassigned_NoSubtasks_StillFires(t *testing.T) {
	// Negative control: the subtask gate only applies when subtasks are
	// present. Atomic tickets still emit assigned on reassignment.
	prev := domain.JiraSnapshot{Key: "SKY-312", Assignee: "", OpenSubtaskCount: 0}
	curr := domain.JiraSnapshot{Key: "SKY-312", Assignee: testUser, OpenSubtaskCount: 0}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueAssigned) == nil {
		t.Error("expected assigned on reassignment of atomic ticket")
	}
}

func TestDiffJira_BecameAtomic_TerminalStatus_NoEvent(t *testing.T) {
	// Edge: subtasks close at the same poll the parent moves to a terminal
	// status. The completed path handles termination; firing became_atomic
	// would race with CloseEntity and potentially create a task on a ticket
	// that's about to be closed. Suppress became_atomic when curr status
	// is terminal — the completed event is the one that matters.
	prev := domain.JiraSnapshot{
		Key:              "SKY-303",
		Status:           "In Progress",
		Assignee:         testUser,
		OpenSubtaskCount: 2,
	}
	curr := domain.JiraSnapshot{
		Key:              "SKY-303",
		Status:           "Done",
		Assignee:         testUser,
		OpenSubtaskCount: 0,
	}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testUser, testDoneStatuses)
	if findEvent(evts, domain.EventJiraIssueBecameAtomic) != nil {
		t.Error("should not emit became_atomic when transitioning to terminal status")
	}
	// status_changed + completed are the right signals for this transition.
	if findEvent(evts, domain.EventJiraIssueCompleted) == nil {
		t.Error("expected completed event on transition to Done")
	}
}

// --- Compound scenario: multiple changes in one poll ------------------------

func TestDiff_CompoundPoll_CIAndNewCommitsAndLabels(t *testing.T) {
	prev := basePRSnapshot()
	prev.HeadSHA = "aaa"
	prev.Labels = []string{"wip"}

	curr := basePRSnapshot()
	curr.HeadSHA = "bbb"
	curr.Labels = []string{"wip", "ready"}
	curr.CheckRuns = []domain.CheckRun{
		{ID: 10, Name: "build", Conclusion: "failure"},
		{ID: 11, Name: "test", Conclusion: "success"},
	}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testUser, nil)

	// Should see: new_commits + ci_check_failed + ci_check_passed + label_added
	types := eventTypes(evts)
	expected := map[string]bool{
		domain.EventGitHubPRNewCommits:    false,
		domain.EventGitHubPRCICheckFailed: false,
		domain.EventGitHubPRCICheckPassed: false,
		domain.EventGitHubPRLabelAdded:    false,
	}
	for _, et := range types {
		if _, ok := expected[et]; ok {
			expected[et] = true
		}
	}
	for et, found := range expected {
		if !found {
			t.Errorf("missing expected event type: %s (got: %v)", et, types)
		}
	}
}

// --- extractProject helper --------------------------------------------------

func TestExtractProject(t *testing.T) {
	cases := []struct{ key, want string }{
		{"SKY-123", "SKY"},
		{"PROJ-1", "PROJ"},
		{"NOHYPHEN", "NOHYPHEN"},
	}
	for _, tc := range cases {
		got := extractProject(tc.key)
		if got != tc.want {
			t.Errorf("extractProject(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
