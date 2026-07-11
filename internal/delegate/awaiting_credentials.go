package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// awaitingCredentialsTimeout is how long a claimed run waits for its
// sealed bundle before releasing the claim back to 'queued' (TFAC-614) —
// same order of magnitude as internal/routing's drainingStaleAfter. Brain
// down at claim time is the case this bounds: the run parks, times out,
// and requeues — no work is lost, just delayed a start the brain couldn't
// have serviced anyway.
const awaitingCredentialsTimeout = 2 * time.Minute

// awaitingCredentialsPollInterval is how often the wait re-checks
// run_credentials. A doorbell (the cred_request tf_ctl notification fired
// by MarkAwaitingCredentials) makes the common case near-instant; this
// poll is the backstop for a dropped notification, bounded by
// awaitingCredentialsTimeout either way.
const awaitingCredentialsPollInterval = 500 * time.Millisecond

// awaitCredentials is the executor-role gate between claim and workspace
// setup (TFAC-614). On every other role (RoleAll, RoleControl, local) it is
// a pure passthrough — credentials keep resolving directly against the
// live secret store exactly as before this ticket, zero behavior change.
//
// On TF_ROLE=executor: parks the run in status='awaiting_credentials',
// nudges the brain's credential provisioner (MarkAwaitingCredentials fires
// the cred_request tf_ctl doorbell), and polls run_credentials until a
// bundle sealed for THIS instance's current boot epoch appears. A bundle
// whose boot_epoch doesn't match is never handed to credseal.Open — either
// it's stale (a prior boot of this same instance; that boot's keypair is
// gone) or the brain hasn't caught up to this claim yet — either way the
// only correct move is to keep waiting for a fresh one, never attempt the
// decrypt.
//
// Returns ok=false when the deadline elapses first: the claim has already
// been released back to 'queued' (RequeueAwaitingCredentials) and the
// caller must return immediately without any further processing — the run
// gets re-claimed (by this instance or a sibling) and re-requests from
// scratch. Returns ok=false with no DB write on ctx cancellation
// (dispatcher shutdown) — same "leave it for the next boot's reconcile"
// convention dispatchClaimedRun's other pre-agent setup reads follow.
func (s *Spawner) awaitCredentials(ctx context.Context, orgID string, run *domain.AgentRun) (context.Context, bool) {
	if runmode.Role() != runmode.RoleExecutor {
		return ctx, true
	}
	if s.runCredentials == nil || s.runQueue == nil {
		// Not wired (a test fixture) — degrade like every other nil-store
		// seam in this package: behave as if the gate doesn't exist.
		return ctx, true
	}

	if _, err := s.runQueue.MarkAwaitingCredentials(ctx, orgID, run.ID); err != nil {
		dispatchLog.Warn("mark awaiting-credentials failed; falling through to poll (backstop sweep still finds this run once its status catches up)", "run", run.ID, "error", err)
	}

	_, myBootEpoch := s.executorIdentity()
	timeout, pollInterval := s.awaitingCredentialsKnobs()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if bundle, ok := s.tryUnsealBundle(ctx, orgID, run.ID, myBootEpoch); ok {
			return credbundle.WithBundle(ctx, bundle), true
		}

		select {
		case <-ctx.Done():
			return ctx, false
		case <-ticker.C:
			if time.Now().After(deadline) {
				matched, err := s.runQueue.RequeueAwaitingCredentials(ctx, orgID, run.ID)
				if err != nil {
					dispatchLog.Warn("requeue after awaiting-credentials timeout failed", "run", run.ID, "error", err)
				} else if matched {
					dispatchLog.Warn("awaiting-credentials timed out; released claim back to queued", "run", run.ID, "timeout", timeout)
				}
				return ctx, false
			}
		}
	}
}

// tryUnsealBundle reads run_credentials for runID and, if it holds a
// bundle sealed for wantBootEpoch, unseals and parses it. ok=false on any
// miss (no row yet, a stale-epoch row, a read/unseal/parse error) — every
// case the caller just keeps polling.
func (s *Spawner) tryUnsealBundle(ctx context.Context, orgID, runID string, wantBootEpoch int64) (*credbundle.Bundle, bool) {
	_, bootEpoch, sealed, ok, err := s.runCredentials.Get(ctx, orgID, runID)
	if err != nil {
		dispatchLog.Warn("read run credential bundle failed; retrying", "run", runID, "error", err)
		return nil, false
	}
	if !ok || bootEpoch != wantBootEpoch {
		return nil, false
	}
	kp := s.sealingKeyPair()
	if kp == nil {
		dispatchLog.Warn("no sealing keypair wired; cannot unseal run credential bundle", "run", runID)
		return nil, false
	}
	plaintext, err := kp.Open(sealed)
	if err != nil {
		dispatchLog.Warn("unseal run credential bundle failed", "run", runID, "error", err)
		return nil, false
	}
	bundle, err := credbundle.Unmarshal(plaintext)
	if err != nil {
		dispatchLog.Warn("parse run credential bundle failed", "run", runID, "error", err)
		return nil, false
	}
	return bundle, true
}
