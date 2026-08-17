package agenthost

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// DialSandbox constructs the client for a process that has already resolved
// itself to be the jailed CLI: state lives behind the per-run socket and
// nowhere else, so this returns an IPCClient or an error — never a local
// fallback. A fallback would route writes through this binary's own
// env-derived identity, which in multi mode means the wrong org_id in every
// WHERE clause; surfacing the outage beats corrupting data under the wrong
// tenant scope.
//
// Two failure shapes, both terminal:
//
//   - socket absent → ErrSandboxSocketMissing. Nothing else inside a jail can
//     serve the exec verbs, so there is nothing to degrade to. The caller
//     raises this before any subcommand runs, so an agent reading it in a tool
//     result is not left wondering whether its own command was at fault.
//   - socket present but nobody serving → ErrDaemonUnreachable, from an eager
//     LookupRun. Probing at construction turns a dead daemon into one clear
//     error instead of a late mystery failure partway through a verb.
func DialSandbox(ctx context.Context) (Client, error) {
	return dialSandbox(ctx, DefaultSocketPath)
}

// dialSandbox is DialSandbox with the socket path passed explicitly, so both
// failure shapes are testable without a writable /run.
func dialSandbox(ctx context.Context, socketPath string) (Client, error) {
	if !socketExists(socketPath) {
		return nil, ErrSandboxSocketMissing
	}
	c := Dial(socketPath)
	if _, err := c.LookupRun(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("%w: %v", ErrDaemonUnreachable, err)
	}
	return c, nil
}

// NewLocalFromEnv constructs the client for a process that has already
// resolved itself to be the host CLI: it resolves run identity from the
// environment against the supplied stores and serves every call in-process.
// This is the only construction path that consults runident, and the only one
// that needs stores at all.
//
// It never probes for a socket. /run/tf.sock is a jail-only path — the host
// side of the pair lives at /run/tf/<run_id>.sock — so on this identity there
// is nothing to detect.
func NewLocalFromEnv(ctx context.Context, stores db.Stores) (Client, error) {
	ident, err := runident.ResolveRunIdentityFromEnv(ctx, stores)
	if err != nil {
		// Surface the env-missing case as-is so cmd/exec can keep the
		// existing "must be invoked by the delegated agent" stderr message
		// without rewrapping.
		if errors.Is(err, runident.ErrRunIdentityMissing) || errors.Is(err, runident.ErrRunIdentityNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("agenthost: resolve run identity: %w", err)
	}
	return NewLocal(stores, ConversationInfo{
		OrgID:            ident.OrgID,
		UserID:           ident.UserID,
		ConversationID:   ident.ConversationID,
		TeamID:           ident.TeamID,
		IsEventTriggered: ident.IsEventTriggered,
	}), nil
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
