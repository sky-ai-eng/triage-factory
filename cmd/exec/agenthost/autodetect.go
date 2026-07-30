package agenthost

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// AutoDetect returns the right Client for the current process:
//
//   - If DefaultSocketPath (/run/tf.sock) exists, the process is the
//     sandboxed `triagefactory exec` subprocess. Return an IPCClient
//     pointed at the socket; the daemon owns identity. A daemon-side
//     LookupRun is performed eagerly so a dead-but-present socket
//     fails here with ErrDaemonUnreachable instead of surfacing as a
//     late mystery error inside a subcommand.
//
//   - Otherwise, if the process is inside a sandbox (the marker
//     agentproc.SandboxMarkerEnvVar is set), the absent socket is an
//     outage: return ErrSandboxSocketMissing. Nothing else in a jail
//     can serve the exec verbs, so there is nothing to fall back to.
//
//   - Otherwise the binary is the local-mode CLI. Resolve the run
//     identity from TRIAGE_FACTORY_CONVERSATION_ID, construct a LocalClient
//     against the supplied stores, and return it.
//
// Stores is consulted only on the local branch; the IPC branch
// ignores it (the daemon owns its own stores). Passing stores
// uniformly keeps the call site shape the same regardless of which
// branch wins, which matters because cmd/exec/exec.go opens the DB
// before this is called and the local path needs it.
//
// Fails closed on a present-but-dead socket: returning LocalClient
// in that case would silently route writes through the binary's
// env-derived identity, which in multi-mode means the wrong org_id
// in every WHERE clause. Better to surface the daemon outage than
// to corrupt data with the wrong tenant scope.
func AutoDetect(ctx context.Context, stores db.Stores) (Client, error) {
	return autoDetect(ctx, stores, DefaultSocketPath, inSandbox())
}

// autoDetect is AutoDetect with the two ambient inputs — the socket path and
// the sandbox marker — passed explicitly, so the four-way matrix is testable
// without a writable /run or env that the rest of the suite would inherit.
func autoDetect(ctx context.Context, stores db.Stores, socketPath string, sandboxed bool) (Client, error) {
	if socketExists(socketPath) {
		return autoDetectIPC(ctx, socketPath)
	}
	if sandboxed {
		// Fail closed BEFORE stores is touched. The local branch below would
		// otherwise open a database per TF_MODE — unset in a jail, so it
		// defaults to local and mints a fresh empty SQLite file inside the
		// sandbox — and then blame the run for not existing in it.
		return nil, ErrSandboxSocketMissing
	}
	return autoDetectLocal(ctx, stores)
}

// inSandbox reports whether this process is running inside the agent jail,
// per the marker agentproc's sandbox env assembler sets. Exact match: any
// other value means the variable came from somewhere that isn't the
// assembler, and the only safe reading of that is "not sandboxed".
func inSandbox() bool {
	return os.Getenv(agentproc.SandboxMarkerEnvVar) == agentproc.SandboxMarkerEnvValue
}

func socketExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	// A regular file at /run/tf.sock would be a deployment bug rather
	// than a daemon socket; treat the type check as a defensive guard.
	return info.Mode()&os.ModeSocket != 0
}

func autoDetectIPC(ctx context.Context, socketPath string) (Client, error) {
	c := Dial(socketPath)
	if _, err := c.LookupRun(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("%w: %v", ErrDaemonUnreachable, err)
	}
	return c, nil
}

func autoDetectLocal(ctx context.Context, stores db.Stores) (Client, error) {
	ident, err := runident.ResolveRunIdentityFromEnv(ctx, stores)
	if err != nil {
		// Surface the env-missing case as-is so cmd/exec/exec.go can
		// keep the existing "must be invoked by the delegated agent"
		// stderr message without rewrapping.
		if errors.Is(err, runident.ErrRunIdentityMissing) || errors.Is(err, runident.ErrRunIdentityNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("agenthost: resolve run identity: %w", err)
	}
	return NewLocal(stores, RunInfo{
		OrgID:            ident.OrgID,
		UserID:           ident.UserID,
		RunID:            ident.RunID,
		TeamID:           ident.TeamID,
		IsEventTriggered: ident.IsEventTriggered,
	}), nil
}
