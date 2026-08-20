// The resume side of the park-first contract: what a claim does when it lands
// on a conversation whose workspace snapshot has not appeared yet.
//
// A park flips the conversation's status before it writes the blob, so "no
// blob" is a legitimate reading of a perfectly healthy resumable run for as
// long as the capture takes. The lifecycle record (workspace_snapshots) is
// what tells the two apart, and this file is the reader: it waits out a
// persist that is in flight and gives up on one that is not, on evidence
// rather than on a timer alone.

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

// DefaultSnapshotWait bounds a cold resume's wait on an in-flight persist. It
// is a backstop for a writer that hangs without dying, not an expected
// duration: change-scoped capture and streamed compression put a real persist
// in the seconds, and every other exit from the wait — the blob appearing, the
// record turning terminal, the writing executor going silent — fires long
// before this does. Generous on purpose, because the thing it protects is the
// only copy of an agent's uncommitted work.
const DefaultSnapshotWait = 60 * time.Second

// snapshotWaitPoll is how often the wait re-reads the record and the blob
// store. Fast enough that a persist landing is followed within a second and
// cheap enough that the whole wait is a handful of indexed reads.
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

// SetSnapshotWaitTimeout overrides how long a cold resume waits on an
// in-flight workspace persist. Call once at startup (internal/app resolves
// TF_SNAPSHOT_WAIT_SEC); tests inject a short value. A non-positive value is
// ignored, for the same reason the parser has no disable arm.
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

// awaitSnapshotBlob waits for a key's snapshot blob to appear while the
// evidence says one is still coming, and reports whether it did along with how
// long the wait took (which the caller puts on its span — a resume that spent
// 40 seconds here looks identical to a fast one from the provenance alone).
//
// It returns immediately, false, when nothing is owed: no lifecycle record at
// all, or one that is already terminal. Otherwise it polls until any of four
// things happens — the blob appears (true), the record leaves `pending`, the
// engagement that owes the write stops heartbeating, or the bound elapses.
//
// Liveness is read through the writing CLAIM, because that is what the record
// names: the claim resolves to the executor that took it, and the instance
// registry says whether that executor is still beating. Both reads fail OPEN —
// an unresolvable claim or an unreadable registry leaves the writer presumed
// live — because being unable to check is not evidence the persist died, and
// giving up on it discards the agent's uncommitted work. The bound is what
// keeps that safe: a genuinely dead writer costs the full wait rather than
// forever.
func (s *Spawner) awaitSnapshotBlob(ctx context.Context, orgID, keyID string) (appeared bool, waited time.Duration) {
	blobs := s.Storage()
	if blobs == nil {
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

	// The writer is read once and held. A re-read that FAILS must not be
	// allowed to erase who we are waiting on — the liveness check at the top
	// of every iteration needs a claim id, and an unreadable record is exactly
	// the moment there is none to be had.
	writerClaimID := state.WriterClaimID
	bound := s.snapshotWait()
	started := time.Now()
	deadline := started.Add(bound)
	delegateLog.Info("resume: workspace persist in flight; waiting for the snapshot",
		"org", orgID, "key_id", keyID, "writer_claim_id", writerClaimID, "bound", bound)

	tick := time.NewTicker(s.snapshotPoll())
	defer tick.Stop()
	for {
		if !s.snapshotWriterAlive(ctx, orgID, writerClaimID) {
			delegateLog.Info("resume: the engagement that owed this snapshot is gone; giving up on the wait",
				"org", orgID, "key_id", keyID, "writer_claim_id", writerClaimID, "waited", time.Since(started))
			return false, time.Since(started)
		}
		select {
		case <-ctx.Done():
			return false, time.Since(started)
		case <-tick.C:
		}

		if ok, eErr := blobs.Exists(ctx, snapshotKey(orgID, keyID)); eErr != nil {
			// A blob store hiccup is not an answer either way; keep waiting
			// and let the bound decide. The record is re-read below regardless,
			// so a persist that finished during the hiccup still ends the wait.
			delegateLog.Warn("resume: snapshot existence check during the wait failed",
				"org", orgID, "key_id", keyID, "error", eErr)
		} else if ok {
			return true, time.Since(started)
		}

		switch cur, sErr := s.snapshotStateFor(ctx, orgID, keyID); {
		case sErr != nil:
			// Same reasoning as the blob hiccup: unreadable is not terminal.
			delegateLog.Warn("resume: workspace snapshot state re-read during the wait failed",
				"org", orgID, "key_id", keyID, "error", sErr)
		case cur == nil || cur.State != domain.WorkspaceSnapshotPending:
			// A `failed` record — or one a successor's discard removed — is
			// the durable answer this wait exists to receive. Whether the
			// key turned `written` between the blob check above and this read
			// is not worth a second Exists: the caller's own Get is next.
			return false, time.Since(started)
		default:
			// A successor may have taken the key over mid-wait. Follow it:
			// the blob this resume needs is now that engagement's to produce,
			// so its liveness is the one worth watching.
			writerClaimID = cur.WriterClaimID
		}

		if !time.Now().Before(deadline) {
			delegateLog.Warn("resume: gave up waiting for a workspace snapshot that never landed",
				"org", orgID, "key_id", keyID, "writer_claim_id", writerClaimID, "bound", bound,
				"env", "TF_SNAPSHOT_WAIT_SEC")
			return false, time.Since(started)
		}
	}
}

// snapshotWriterAlive reports whether the engagement named on a pending
// lifecycle record still has a process behind it: the claim resolves to an
// executor, and that executor's instance-registry heartbeat is inside the
// placement liveness window.
//
// The window is placement's own, deliberately — the same staleness the claim
// uses to decide a stamped preferred executor is dead. Two different answers
// to "is that executor alive" would mean a resume could wait on a writer the
// claim layer had already written off. Placement is disabled at N=1, where its
// configured window is zero, so the package default stands in.
//
// Fails open at every step: an unwired store, an unresolvable claim, or a
// failed read all answer "alive". See awaitSnapshotBlob for why.
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

// snapshotWriterLiveness is the heartbeat-staleness window the wait treats as
// "still writing" — placement's configured one, or its default when placement
// is off (local N=1) and the config carries no value.
func (s *Spawner) snapshotWriterLiveness() time.Duration {
	if live := s.claimPlacement().Liveness; live > 0 {
		return live
	}
	return placement.DefaultLiveness
}
