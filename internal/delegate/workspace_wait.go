// The resume side of the park-first contract: what a claim does when it lands
// on a conversation whose workspace snapshot has not appeared yet.
//
// A park flips the status before it writes the blob, so "no blob" is a
// legitimate reading of a healthy resumable run for as long as the capture
// takes. The lifecycle record (workspace_snapshots) tells the two apart, and
// this file is its reader: wait out a persist that is in flight, give up on
// one that is not, on evidence rather than on a timer alone.

package delegate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
)

// DefaultSnapshotWait bounds a cold resume's wait on an in-flight persist: a
// backstop for a writer that hangs without dying, not an expected duration.
// Every other exit — the blob appearing, the record turning terminal, the
// writing executor going silent — fires long before this does, and it is
// generous on purpose because what it protects is the only copy of an agent's
// uncommitted work.
const DefaultSnapshotWait = 60 * time.Second

// snapshotWaitPoll is how often the wait re-reads the record and the blob
// store: fast enough to follow a landing persist within a second, cheap enough
// that the whole wait is a handful of indexed reads.
const snapshotWaitPoll = time.Second

// ParseSnapshotWaitTimeout interprets the TF_SNAPSHOT_WAIT_SEC env value.
// Empty → the default. Non-numeric or non-positive → the default plus an error
// the caller logs; a bad value must not brick boot. There is deliberately no
// "0 disables" arm: a zero wait would turn every park-first window into a
// fresh-workspace fallback, discarding work the writer was about to store.
func ParseSnapshotWaitTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultSnapshotWait, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultSnapshotWait, fmt.Errorf(
			"invalid TF_SNAPSHOT_WAIT_SEC %q (want a positive integer number of seconds); using default %s",
			raw, DefaultSnapshotWait)
	}
	return time.Duration(n) * time.Second, nil
}

// SetSnapshotWaitTimeout overrides that bound. Call once at startup
// (internal/app resolves TF_SNAPSHOT_WAIT_SEC); tests inject a short value. A
// non-positive value is ignored, for the parser's reason above.
func (s *Spawner) SetSnapshotWaitTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotWaitTimeout = d
}

// snapshotWait returns the effective wait bound, falling back to the default
// when unset.
func (s *Spawner) snapshotWait() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshotWaitTimeout > 0 {
		return s.snapshotWaitTimeout
	}
	return DefaultSnapshotWait
}

// snapshotPoll returns the effective poll interval for that wait.
func (s *Spawner) snapshotPoll() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshotWaitPollInterval > 0 {
		return s.snapshotWaitPollInterval
	}
	return snapshotWaitPoll
}

// awaitSnapshotBlob waits for a key's snapshot blob while the evidence says
// one is still coming, reporting whether it appeared and how long that took
// (the caller puts the duration on its span).
//
// The blob is the authority and the record only decides whether to keep
// waiting for one, so every pass looks at the blob FIRST and every give-up
// looks once more on its way out. That ordering is load-bearing: the upload
// lands before the record's completing CAS and before a dying writer stops
// answering, so each condition that ends the wait is a moment the blob may
// just have arrived.
//
// Returns immediately when nothing is owed — no record, or a terminal one.
func (s *Spawner) awaitSnapshotBlob(ctx context.Context, orgID, keyID string) (appeared bool, waited time.Duration) {
	if s.Storage() == nil {
		return false, 0
	}
	state, err := s.snapshotStateFor(ctx, orgID, keyID)
	if err != nil {
		delegateLog.Warn("resume: workspace snapshot state read failed; not waiting for a persist",
			"org", orgID, "key_id", keyID, "error", err)
		return false, 0
	}
	if state == nil || state.State != domain.WorkspaceSnapshotPending {
		return false, 0
	}

	// The writer is held across iterations: a re-read that fails must not
	// erase who we are waiting on, and an unreadable record is exactly when
	// there is no fresh answer to be had.
	writerClaimID := state.WriterClaimID
	bound := s.snapshotWait()
	started := time.Now()
	deadline := started.Add(bound)
	delegateLog.Info("resume: workspace persist in flight; waiting for the snapshot",
		"org", orgID, "key_id", keyID, "writer_claim_id", writerClaimID, "bound", bound)

	tick := time.NewTicker(s.snapshotPoll())
	defer tick.Stop()
	for {
		if s.snapshotBlobExists(ctx, orgID, keyID) {
			return true, time.Since(started)
		}

		giveUp := ""
		switch cur, sErr := s.snapshotStateFor(ctx, orgID, keyID); {
		case sErr != nil:
			// Unreadable is not terminal; the bound still applies.
			delegateLog.Warn("resume: workspace snapshot state re-read during the wait failed",
				"org", orgID, "key_id", keyID, "error", sErr)
		case cur == nil || cur.State != domain.WorkspaceSnapshotPending:
			giveUp = "the persist is no longer pending"
		default:
			// A successor may have taken the key over mid-wait. Follow it: the
			// blob this resume needs is that engagement's to produce now.
			writerClaimID = cur.WriterClaimID
		}
		if giveUp == "" && !s.snapshotWriterAlive(ctx, orgID, writerClaimID) {
			giveUp = "the engagement that owed it is gone"
		}
		if giveUp == "" && !time.Now().Before(deadline) {
			giveUp = "the wait bound elapsed"
		}
		if giveUp != "" {
			if s.snapshotBlobExists(ctx, orgID, keyID) {
				return true, time.Since(started)
			}
			delegateLog.Warn("resume: stopped waiting for a workspace snapshot that never landed",
				"org", orgID, "key_id", keyID, "writer_claim_id", writerClaimID,
				"reason", giveUp, "bound", bound, "waited", time.Since(started))
			return false, time.Since(started)
		}

		select {
		case <-ctx.Done():
			return false, time.Since(started)
		case <-tick.C:
		}
	}
}

// snapshotBlobExists is the wait's one real question. A store error answers
// "not yet": it is not evidence the blob is missing, and the wait's bound is
// what keeps that from being unbounded.
func (s *Spawner) snapshotBlobExists(ctx context.Context, orgID, keyID string) bool {
	blobs := s.Storage()
	if blobs == nil {
		return false
	}
	ok, err := blobs.Exists(ctx, snapshotKey(orgID, keyID))
	if err != nil {
		delegateLog.Warn("resume: snapshot existence check during the wait failed",
			"org", orgID, "key_id", keyID, "error", err)
		return false
	}
	return ok
}

// snapshotWriterAlive reports whether the engagement named on a pending record
// still has a process behind it: its claim resolves to an executor, and that
// executor's heartbeat is inside the placement liveness window. Placement's
// own window, deliberately — two different answers to "is that executor alive"
// would let a resume wait on a writer the claim layer had already written off.
//
// Fails open at every step: an unwired store or a failed read answers "alive",
// because being unable to check is not evidence the persist died, and the
// bound is what keeps that from being unbounded.
func (s *Spawner) snapshotWriterAlive(ctx context.Context, orgID, writerClaimID string) bool {
	if s.conversationQueue == nil || s.instances == nil || writerClaimID == "" {
		return true
	}
	executorID, ok, err := s.conversationQueue.ClaimExecutorSystem(ctx, orgID, writerClaimID)
	if err != nil {
		delegateLog.Warn("resume: could not resolve the snapshot writer's executor; presuming it is still writing",
			"org", orgID, "writer_claim_id", writerClaimID, "error", err)
		return true
	}
	if !ok || executorID == "" {
		// No claim row behind the record. Nothing is producing this blob, and
		// unlike the failures above that is a fact rather than a gap.
		return false
	}
	inst, err := s.instances.Get(ctx, executorID)
	if err != nil {
		delegateLog.Warn("resume: could not read the snapshot writer's instance; presuming it is still writing",
			"org", orgID, "writer_claim_id", writerClaimID, "executor", executorID, "error", err)
		return true
	}
	if inst == nil {
		return false
	}
	return time.Since(inst.LastHeartbeatAt) <= s.snapshotWriterLiveness()
}

// snapshotWriterLiveness is the staleness window the wait treats as "still
// writing": placement's configured one, or its default when placement is off
// (local N=1) and the config carries no value.
func (s *Spawner) snapshotWriterLiveness() time.Duration {
	if live := s.claimPlacement().Liveness; live > 0 {
		return live
	}
	return placement.DefaultLiveness
}
