package app

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/reaper"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ExitCodeIdentitySuperseded is the distinct process exit code an instance
// uses when fence completion runs to its end: its identity was superseded
// (another process re-registered this instance id with a newer boot_epoch —
// a duplicated state root), its live sandboxes were killed, and it must not
// keep running (spec §4.1(4), decision log's "epoch-war restart loop is the
// alarm, by design"). Compose/systemd restarts the process; it re-registers,
// bumps the epoch, and fences whichever OTHER copy is still holding the
// duplicated state root — two processes sharing one state root crash-loop
// forever, which IS the operator signal (no backoff, no arbitration; see
// docs/self-hosting/install.md's duplicated-state-root remediation).
const ExitCodeIdentitySuperseded = 3

// buildReaper resolves the fleet-reaper knobs (TF_SELF_FENCE_SEC,
// TF_REAPER_STALE_SEC, TF_MAX_CLAIM_ATTEMPTS) and the per-claim lease knobs
// (TF_CLAIM_RENEW_SEC, TF_CLAIM_SELF_FENCE_SEC, TF_CLAIM_TAKEOVER_SEC), wires
// the spawner's partition self-fence deadline, its claim-lease timings and
// its supersession exit hook (every role, every mode — supersession is
// structurally unreachable outside a shared multi-mode registry, so this is a
// safe no-op elsewhere), and — for brain-capable roles in multi mode only —
// constructs the reaper Store startBrain/stopBrain drive. Runs after
// buildExecution (needs a.spawner).
//
// The claim knobs are resolved here, beside the instance ones, because the
// two orderings are siblings and a reader comparing them should find them
// together. They remain independent pairs: instance heartbeat → instance
// fence → heartbeat reap, and claim renewal → claim fence → lease expiry.
func (a *App) buildReaper() error {
	selfFence, err := delegate.ParseSelfFenceDeadline(os.Getenv("TF_SELF_FENCE_SEC"))
	if err != nil {
		return fmt.Errorf("self-fence deadline: %w", err)
	}
	staleThreshold, err := reaper.ParseStaleThreshold(os.Getenv("TF_REAPER_STALE_SEC"))
	if err != nil {
		return fmt.Errorf("reaper stale threshold: %w", err)
	}
	// The strict ordering IS the correctness property (spec's pre-made
	// decisions): a partitioned executor must have provably stopped
	// claiming and killed its sandboxes before the reaper may requeue its
	// rows. Refuse to boot with a configuration that can't guarantee that,
	// mirroring lease.KnobsFromEnv's demoteDeadline < ttl check.
	if selfFence >= staleThreshold {
		return fmt.Errorf("self-fence deadline (%s) must be strictly less than the reaper staleness threshold (%s)", selfFence, staleThreshold)
	}
	maxAttempts, err := reaper.ParseMaxAttempts(os.Getenv("TF_MAX_CLAIM_ATTEMPTS"))
	if err != nil {
		return fmt.Errorf("claim max attempts: %w", err)
	}

	claimRenew, err := delegate.ParseClaimRenewInterval(os.Getenv("TF_CLAIM_RENEW_SEC"))
	if err != nil {
		return fmt.Errorf("claim lease: %w", err)
	}
	claimSelfFence, err := delegate.ParseClaimSelfFenceDeadline(os.Getenv("TF_CLAIM_SELF_FENCE_SEC"))
	if err != nil {
		return fmt.Errorf("claim lease: %w", err)
	}
	claimLease, err := delegate.ParseClaimLease(os.Getenv("TF_CLAIM_TAKEOVER_SEC"))
	if err != nil {
		return fmt.Errorf("claim lease: %w", err)
	}
	// The same correctness property the pair above carries, one level finer: a
	// holder that cannot renew must have killed its own cell before the lease
	// it can no longer prove lapses and a successor may take the conversation.
	// A configuration that cannot guarantee that is refused rather than run.
	if claimRenew >= claimSelfFence || claimSelfFence >= claimLease {
		return fmt.Errorf("claim lease knobs must satisfy TF_CLAIM_RENEW_SEC (%s) < TF_CLAIM_SELF_FENCE_SEC (%s) < TF_CLAIM_TAKEOVER_SEC (%s)",
			claimRenew, claimSelfFence, claimLease)
	}

	a.spawner.SetSelfFenceDeadline(selfFence)
	a.spawner.SetClaimLease(claimRenew, claimSelfFence, claimLease)
	a.spawner.SetOnSupersessionFence(func() {
		appLog.Error("exiting after identity-supersession fence completion", "exit_code", ExitCodeIdentitySuperseded)
		os.Exit(ExitCodeIdentitySuperseded)
	})

	if runmode.Current() != runmode.ModeMulti || !a.plan.brain {
		return nil
	}
	a.reaperStore = reaper.NewPostgresStore(a.database)
	a.reaperStaleThreshold = staleThreshold
	a.reaperMaxAttempts = maxAttempts
	return nil
}
