package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// PRMergeabilityStore owns pull_request_mergeability, a pull request's last
// known mergeability and its changed paths; pr_mergeability_probes, the queue
// of reads that are due, on the shared work-item contract as
// workkinds.PRMergeabilityProbes declares it; and branch_tips, the last tip
// of each base branch the poll's branch watch processed.
//
// The probe worker is the only writer of the row's answer columns, through
// CompleteProbe, and the row's transitions are what emit
// github:pr:conflicts and github:pr:conflicts_resolved: the stored answer is
// those events' re-emit guard, the one the snapshot diff is for every other
// pull request event. The branch watch is the only writer of the row's path
// columns (UpsertPathsSystem) and of branch_tips. Each writer's upsert sets
// its own columns only, so neither overwrites the other's. The poll admits
// probes (AdmitSystem), reads the rows to decide which are due (ListSystem),
// and deletes the rows of entities no longer active (DeleteInactiveSystem).
//
// Every method is a system method on the admin pool in Postgres, org_id bound
// by argument or through the receipt; the SQLite impl asserts the local
// sentinel. The store is never transaction-bound. The holder verbs are exempt
// from the returned-row rule for the reason the re-evaluation queue's are.
type PRMergeabilityStore interface {
	// GetSystem is the pull request's row, or nil when it has none.
	GetSystem(ctx context.Context, orgID, entityID string) (*domain.PRMergeability, error)

	// ListSystem is every row in the org, ordered by entity id. The rows are
	// bounded by the org's active pull requests: DeleteInactiveSystem removes
	// the rest every poll cycle.
	ListSystem(ctx context.Context, orgID string) ([]domain.PRMergeability, error)

	// DeleteInactiveSystem deletes, in one statement, the org's rows whose
	// entity is not active, and reports how many.
	DeleteInactiveSystem(ctx context.Context, orgID string) (int, error)

	// AdmitSystem admits one probe per entry, each keyed on its entity and
	// due workkinds.PRMergeabilityProbeDelay from now, in one transaction.
	// An entry whose pull request already has an unsettled probe collapses
	// into it, which keeps that probe's reason and probe_after. A reason
	// outside domain.MergeabilityProbeReasons is refused before anything is
	// written. admitted counts the entries that inserted a row.
	AdmitSystem(ctx context.Context, orgID string, probes []PRMergeabilityProbe) (admitted int, err error)

	// Claim leases up to n ripe probes of the org and reads, beside each, its
	// own columns, the entity's node id and state, and the owner and name of
	// the repository registry row the entity references. Items is in claim
	// order. Rows the claim settled instead are counted, not returned; a
	// non-nil error still returns the items of rounds that committed.
	Claim(ctx context.Context, owner workitem.Owner, orgID string, n int) (PRMergeabilityClaim, error)

	// RenewLease pushes the lease out and observes a pending cancellation,
	// as every kind's does.
	RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error)

	// CompleteProbe records one answer and completes its probe, in one
	// transaction under the probe row's lock (workitem.Complete, or
	// workitem.CompleteAndAdmit for an UNKNOWN retry):
	//
	//   - no answer (Found false), or an entity that is gone or no longer
	//     active: completed with no write;
	//   - MERGEABLE or CONFLICTING: the row is upserted with the answer, the
	//     head it was read at and the time, and the transition from the
	//     previous answer is enqueued on the durable event queue, stamped
	//     with the org and traceparent;
	//   - UNKNOWN: below workkinds.PRMergeabilityMaxUnknownReads UNKNOWN
	//     reads, the row is untouched, and the probe is completed and a
	//     successor admitted under its key with reason unknown_retry, due
	//     workkinds.PRMergeabilityUnknownRetryDelay from now. At the limit the
	//     probe is completed with nothing owed, and the row's head and
	//     checked_at move to the read's while its answer stays (NULL on a row
	//     the give-up creates), so the poll's next admission for the pull
	//     request is its sweep.
	//
	// A receipt another holder took is workitem.ErrLeaseLost and writes
	// nothing at all, events included; workitem.ErrCancelled means the
	// request was settled instead.
	CompleteProbe(ctx context.Context, r workitem.Receipt, answer PRMergeabilityAnswer, traceparent string) (PRMergeabilityProbeResult, error)

	// Requeue records a failed read under a typed outcome: the probe returns
	// to ready with a backoff retry time, or parks when the outcome is
	// permanent or the budget is spent. parked reports which.
	Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (parked bool, err error)

	// DeferRateLimited returns a claimed probe to ready, due at until, with
	// the attempt its claim charged refunded (workitem.Defer under
	// workkinds.PRMergeabilityDeferRateLimited): GitHub's rate limit on the
	// probe's credential resets at until, which is waiting the probe is owed
	// rather than a failure of its own, whether its read was refused or never
	// sent. The deferral's predicate is always true, because the reason to
	// wait is GitHub's answer, which no row holds.
	DeferRateLimited(ctx context.Context, r workitem.Receipt, until time.Time) error

	// PruneSettled deletes done and cancelled probes settled before the
	// cutoff, across every org, and reports how many. Parked probes are kept
	// for an operator.
	PruneSettled(ctx context.Context, before time.Time) (int, error)

	// ListPathsSystem is the path columns of the rows of the org's entities
	// in entityIDs, ordered by entity id. An entity with no row is absent; a
	// row whose paths were never written has nil Paths.
	ListPathsSystem(ctx context.Context, orgID string, entityIDs []string) ([]domain.PRPaths, error)

	// UpsertPathsSystem writes one pull request's changed paths, the head
	// they were read at and whether they are complete, and returns the path
	// columns as persisted. A pull request with no row gets one with no
	// answer. The answer columns are never touched.
	UpsertPathsSystem(ctx context.Context, orgID string, paths domain.PRPaths) (domain.PRPaths, error)

	// ListBranchTipsSystem is every branch_tips row of the org, ordered by
	// repository and branch. The rows are bounded by the base branches the
	// org's open pull requests target: DeleteBranchTipsExceptSystem removes
	// the rest every poll cycle.
	ListBranchTipsSystem(ctx context.Context, orgID string) ([]domain.BranchTip, error)

	// UpsertBranchTipSystem writes one branch's tip, ETag and observed time,
	// and returns the row as persisted.
	UpsertBranchTipSystem(ctx context.Context, orgID string, tip domain.BranchTip) (domain.BranchTip, error)

	// DeleteBranchTipsExceptSystem deletes, in one transaction, the org's
	// branch_tips rows of branches not in keep, and reports how many.
	DeleteBranchTipsExceptSystem(ctx context.Context, orgID string, keep []domain.BranchRef) (int, error)
}

// PRMergeabilityProbe is one admission: the pull request entity and why its
// mergeability is due to be read.
type PRMergeabilityProbe struct {
	EntityID string
	Reason   string
}

// ClaimedPRMergeabilityProbe is one leased probe and what reading it needs.
// NodeID, RepositoryID, Owner and Repo are empty, and Active false, when the
// entity row or its repository's registry row is gone.
type ClaimedPRMergeabilityProbe struct {
	Receipt      workitem.Receipt
	EntityID     string
	Reason       string
	UnknownReads int
	// NodeID is the pull request's GraphQL node id, the entity's external_id.
	NodeID string
	Active bool
	// RepositoryID is the entity's repository registry row; Owner and Repo
	// are that row's current name.
	RepositoryID string
	Owner        string
	Repo         string
}

// PRMergeabilityClaim is one Claim call's work, with workitem.ClaimResult's
// counts passed through for the worker's log.
type PRMergeabilityClaim struct {
	Items     []ClaimedPRMergeabilityProbe
	Cancelled int
	Parked    int
	Reclaimed int
}

// PRMergeabilityAnswer is what GitHub said about one probed pull request.
type PRMergeabilityAnswer struct {
	// Found is false when GitHub returned no node for the id, or a node that
	// is not a pull request.
	Found bool
	// Mergeable is MERGEABLE, CONFLICTING or UNKNOWN.
	Mergeable string
	// HeadSHA is the pull request's head (headRefOid) at the read.
	HeadSHA string
}

// PRMergeabilityOutcome is which arm a CompleteProbe took.
type PRMergeabilityOutcome string

const (
	// PRMergeabilityRecorded is a known answer, written to the row.
	PRMergeabilityRecorded PRMergeabilityOutcome = "recorded"
	// PRMergeabilityRetried is an UNKNOWN answer whose successor was admitted.
	PRMergeabilityRetried PRMergeabilityOutcome = "unknown_retry"
	// PRMergeabilityGaveUp is an UNKNOWN answer at the retry limit, recorded
	// on the row as a read with no answer.
	PRMergeabilityGaveUp PRMergeabilityOutcome = "unknown_gave_up"
	// PRMergeabilitySkipped is a probe completed with no write: no answer, or
	// an entity that is gone or no longer active.
	PRMergeabilitySkipped PRMergeabilityOutcome = "skipped"
)

// PRMergeabilityProbeResult is what one CompleteProbe committed.
type PRMergeabilityProbeResult struct {
	Outcome PRMergeabilityOutcome
	// Previous is the row's answer before a recorded one; empty when the
	// pull request had no row.
	Previous string
	// Row is the row a recorded answer or a give-up wrote, as the write
	// returned it.
	Row *domain.PRMergeability
	// Events are the transitions enqueued with the answer, each stamped
	// with the id the enqueue minted.
	Events []domain.Event
	// SuccessorID is the retry a PRMergeabilityRetried completion admitted.
	SuccessorID int64
}

// ValidatePRMergeabilityProbes refuses an admission batch naming no entity
// or a reason outside the kind's vocabulary, so both dialects refuse the same
// batches before writing anything.
func ValidatePRMergeabilityProbes(probes []PRMergeabilityProbe) error {
	for _, p := range probes {
		if p.EntityID == "" {
			return fmt.Errorf("db: mergeability probe has no entity")
		}
		if !slices.Contains(domain.MergeabilityProbeReasons, p.Reason) {
			return fmt.Errorf("db: mergeability probe for entity %s has unknown reason %q", p.EntityID, p.Reason)
		}
	}
	return nil
}

// ValidatePRMergeabilityAnswer refuses an answer GitHub does not give, so
// both dialects refuse the same answers before opening a transaction.
func ValidatePRMergeabilityAnswer(a PRMergeabilityAnswer) error {
	if a.Found && !KnownMergeable(a.Mergeable) && a.Mergeable != domain.MergeableUnknown {
		return fmt.Errorf("db: mergeability answer %q is not one GitHub gives", a.Mergeable)
	}
	return nil
}

// PRMergeabilityProbeCols is the kind's own columns for one admission, shared
// by both dialects so the two produce identical rows. probeAfter is bound by
// the caller in its dialect's storage shape.
func PRMergeabilityProbeCols(entityID, reason string, probeAfter any, unknownReads int) map[string]any {
	return map[string]any{
		"entity_id":     entityID,
		"reason":        reason,
		"probe_after":   probeAfter,
		"unknown_reads": unknownReads,
	}
}

// PRMergeabilityRateLimitHolds is DeferRateLimited's deferral predicate in
// both dialects: the rate limit is GitHub's answer to the worker, so there is
// nothing under the row lock to check.
func PRMergeabilityRateLimitHolds(*sql.Tx) (bool, error) { return true, nil }

// KnownMergeable reports whether a GitHub answer is one the row stores.
func KnownMergeable(v string) bool {
	return v == domain.MergeableMergeable || v == domain.MergeableConflicting
}

// PRMergeabilityTransition is the event a stored answer moving from prev to
// curr emits, if any: github:pr:conflicts when it becomes CONFLICTING from
// anything else (no row included), github:pr:conflicts_resolved when it
// moves from CONFLICTING to MERGEABLE. The metadata is the entity's stored
// snapshot (snapshotJSON) with the entity's repository row and the head the
// answer was read at. occurred_at stays zero: GitHub computes mergeability
// asynchronously and exposes no transition time. Shared by both dialects so
// the two emit identical events.
func PRMergeabilityTransition(orgID, entityID, repositoryID, snapshotJSON, prev, curr, headSHA string, now time.Time) (domain.Event, bool, error) {
	var eventType string
	switch {
	case prev != domain.MergeableConflicting && curr == domain.MergeableConflicting:
		eventType = domain.EventGitHubPRConflicts
	case prev == domain.MergeableConflicting && curr == domain.MergeableMergeable:
		eventType = domain.EventGitHubPRConflictsResolved
	default:
		return domain.Event{}, false, nil
	}
	var snap domain.PRSnapshot
	if snapshotJSON != "" && snapshotJSON != "{}" {
		if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
			return domain.Event{}, false, fmt.Errorf("db: entity %s snapshot: %w", entityID, err)
		}
	}
	var meta any
	if eventType == domain.EventGitHubPRConflicts {
		meta = events.GitHubPRConflictsMetadata{
			Author: snap.Author, AuthorID: snap.AuthorID,
			Repo: snap.Repo, RepositoryID: repositoryID, PRNumber: snap.Number,
			IsDraft: snap.IsDraft, HeadSHA: headSHA, Labels: snap.Labels,
		}
	} else {
		meta = events.GitHubPRConflictsResolvedMetadata{
			Author: snap.Author, AuthorID: snap.AuthorID,
			Repo: snap.Repo, RepositoryID: repositoryID, PRNumber: snap.Number,
			IsDraft: snap.IsDraft, HeadSHA: headSHA, Labels: snap.Labels,
		}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return domain.Event{}, false, err
	}
	eid := entityID
	return domain.Event{
		OrgID:        orgID,
		EventType:    eventType,
		EntityID:     &eid,
		MetadataJSON: string(metaJSON),
		CreatedAt:    now,
	}, true, nil
}

// PRMergeabilityProbeSubject is the WorkSubject both dialects describe a
// probe with: the pull request's key ("owner/repo#18"), or the bare entity id
// when the entity row is gone, and its title.
func PRMergeabilityProbeSubject(entityID, reason string, unknownReads int, sourceID, title string) WorkSubject {
	label := sourceID
	if label == "" {
		label = entityID
	}
	return WorkSubject{Label: label, Detail: title, Fields: map[string]string{
		"entity_id":     entityID,
		"reason":        reason,
		"unknown_reads": strconv.Itoa(unknownReads),
	}}
}

// ValidatePRPaths refuses a path write naming no entity or head, or carrying
// a path that is empty or whose change is not a PatchStatus value, so both
// dialects refuse the same writes.
func ValidatePRPaths(p domain.PRPaths) error {
	if p.EntityID == "" {
		return fmt.Errorf("db: changed paths name no entity")
	}
	if p.HeadSHA == "" {
		return fmt.Errorf("db: changed paths of entity %s name no head", p.EntityID)
	}
	for _, c := range p.Paths {
		if c.Path == "" {
			return fmt.Errorf("db: changed paths of entity %s hold an empty path", p.EntityID)
		}
		if !slices.Contains(changeTypes, c.Change) {
			return fmt.Errorf("db: changed path %q of entity %s has unknown change %q", c.Path, p.EntityID, c.Change)
		}
	}
	return nil
}

var changeTypes = []string{
	domain.PathAdded, domain.PathDeleted, domain.PathRenamed,
	domain.PathCopied, domain.PathModified, domain.PathChanged,
}

// EncodePRPaths is paths_json for a path list, shared by both dialects so the
// two store identical text. A nil list encodes as an empty array: a written
// list is a read, even of a pull request that changes nothing.
func EncodePRPaths(paths []domain.ChangedPath) (string, error) {
	if paths == nil {
		paths = []domain.ChangedPath{}
	}
	b, err := json.Marshal(paths)
	return string(b), err
}

// DecodePRPaths reads paths_json back; NULL (never written) is nil.
func DecodePRPaths(raw sql.NullString) ([]domain.ChangedPath, error) {
	if !raw.Valid {
		return nil, nil
	}
	paths := []domain.ChangedPath{}
	if err := json.Unmarshal([]byte(raw.String), &paths); err != nil {
		return nil, fmt.Errorf("db: paths_json: %w", err)
	}
	return paths, nil
}

// ValidateBranchTip refuses a tip naming no repository, branch or tip, or
// with no observed time, so both dialects refuse the same writes.
func ValidateBranchTip(tip domain.BranchTip) error {
	switch {
	case tip.RepositoryID == "" || tip.Branch == "":
		return fmt.Errorf("db: branch tip names no repository or branch")
	case tip.TipSHA == "":
		return fmt.Errorf("db: branch tip of %s names no tip", tip.Branch)
	case tip.ObservedAt.IsZero():
		return fmt.Errorf("db: branch tip of %s has no observed time", tip.Branch)
	}
	return nil
}

// BranchTipsToDelete is the stored tips whose branch is not in keep, shared by
// both dialects' DeleteBranchTipsExceptSystem.
func BranchTipsToDelete(stored []domain.BranchTip, keep []domain.BranchRef) []domain.BranchRef {
	kept := make(map[domain.BranchRef]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	var out []domain.BranchRef
	for _, tip := range stored {
		if !kept[tip.Ref()] {
			out = append(out, tip.Ref())
		}
	}
	return out
}
