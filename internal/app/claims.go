package app

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
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

// buildClaims wires the spawner's claim-recovery settings that are resolved
// at boot: the loss budget (TF_MAX_CLAIM_ATTEMPTS), whether the previous
// boot's cells were confirmed torn down (openStores), and the supersession
// exit hook. Every role, every mode: a dispatcher runs on every role that
// executes, and supersession is structurally unreachable outside a shared
// multi-mode registry, so the hook is a safe no-op elsewhere. Runs after
// buildExecution (needs a.spawner).
//
// The claim lease has no knobs to resolve: its timings are constants in
// internal/delegate, ordered at the point they are declared.
func (a *App) buildClaims() error {
	maxLosses, err := delegate.ParseMaxClaimLosses(os.Getenv("TF_MAX_CLAIM_ATTEMPTS"))
	if err != nil {
		return fmt.Errorf("claim loss budget: %w", err)
	}
	a.spawner.SetMaxClaimLosses(maxLosses)
	a.spawner.SetCellsConfirmedClean(a.cellsConfirmedClean)
	a.spawner.SetOnSupersessionFence(func() {
		appLog.Error("exiting after identity-supersession fence completion", "exit_code", ExitCodeIdentitySuperseded)
		os.Exit(ExitCodeIdentitySuperseded)
	})
	return nil
}
