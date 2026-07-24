package agenthost

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
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
	if socketExists(DefaultSocketPath) {
		return autoDetectIPC(ctx, DefaultSocketPath)
	}
	return autoDetectLocal(ctx, stores)
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
