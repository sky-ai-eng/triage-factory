package agentproc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// This file is the orchestrator-neutral half of the sidecar → orchestrator
// relay: the dispatcher interface the supervisor routes the generic envelope
// to, the core op catalog's wire constants + git op payloads, and the
// sidecar-side sender helpers. The op HANDLERS (which hold db.Stores) live in
// the package that has stores — agenthost's RelayServer implements
// RelayDispatcher; agentproc holds none, so it defines only the seam.

// RelayNamespaceCore is the namespace for ops shared by every built-in verb
// surface — the DB-backed reads/writes and the git proxy's push authz/audit.
// Provider-specific ops (a Slack channel authz, a workspace resolve) ride
// their own namespace so the wire protocol never grows per provider.
const RelayNamespaceCore = "core"

// Core op names. authorize_repo answers the git proxy's per-push decision;
// record_denial / record_push are its fire-and-forget audit records. The
// exec verb-trace DB ops are added to this catalog alongside the relocated
// agenthost; they share the same envelope.
const (
	OpAuthorizeRepo = "authorize_repo"
	OpRecordDenial  = "record_denial"
	OpRecordPush    = "record_push"
)

// AuthorizeRepoArgs / AuthorizeRepoReply are the authorize_repo op's payloads
// — the repo a transiting git op targets, and the DB-backed decision: whether
// it is pushable and, if so, the exact refs allowed. An empty AllowedRefs with
// Allowed=true is fetch-only. These mirror gitproxy.Decision across the wire,
// now as a core op rather than a bespoke Kind.
type AuthorizeRepoArgs struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// AuthorizeRepoReply is authorize_repo's result. DenyReason/DenyMessage carry
// gitproxy.Decision's actionable-denial fields across the wire so a sandboxed
// run's denied clone/fetch surfaces the same specific 403 body + audit reason
// (workspace-add vs admin) the in-process path does, instead of the generic
// fallback. Both empty on an allowed decision.
type AuthorizeRepoReply struct {
	Allowed     bool     `json:"allowed"`
	AllowedRefs []string `json:"allowed_refs,omitempty"`
	DenyReason  string   `json:"deny_reason,omitempty"`
	DenyMessage string   `json:"deny_message,omitempty"`
}

// RecordDenialArgs is record_denial's payload: a denied git op for the
// orchestrator to audit through the same host-side recording path the push
// backstop uses.
type RecordDenialArgs struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Op     string `json:"op"`
	Reason string `json:"reason"`
}

// RecordPushArgs is record_push's payload: one branch ref a receive-pack
// transited plus the upstream's final HTTP status, mirroring gitproxy.PushedRef
// across the wire. The orchestrator reshapes it into the same branch artifact /
// push-failed row the in-process backstop writes. Repo is the "owner/repo" the
// proxy parsed from the request path; Created marks a newly-created ref; Status
// is the receive-pack response code (a 2xx means the push landed).
type RecordPushArgs struct {
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	NewSHA  string `json:"new_sha"`
	Created bool   `json:"created"`
	Status  int    `json:"status"`
}

// RelayDispatcher serves the sidecar's org-bound relay ops. The concrete impl
// (agenthost.RelayServer) holds the run's db.Stores + RunInfo + git gate and
// binds identity from RunInfo, so a relayed op can never be steered at another
// org. DispatchCall serves a request/response op (KindRelayCall); DispatchNotify
// serves a fire-and-forget audit op (KindRelayNotify) best-effort. The
// supervisor holds one of these for the run's lifetime.
type RelayDispatcher interface {
	// DispatchCall serves (namespace, op) with the request args and returns the
	// op's marshaled result, or an error whose text relays back to the sidecar.
	DispatchCall(ctx context.Context, namespace, op string, args json.RawMessage) (json.RawMessage, error)
	// DispatchNotify serves a fire-and-forget audit op. It returns nothing — a
	// failure is logged host-side and swallowed, never surfaced to the sidecar.
	DispatchNotify(ctx context.Context, namespace, op string, args json.RawMessage)
}

// CallRelay is the sidecar-side sender for a request/response relay op: marshal
// args, send KindRelayCall over the supervision channel, unmarshal the op's
// result into out. Used by the sidecar's git proxy and (once relocated) the
// agenthost verb trace — every one of them holds only this conn, never a store.
func CallRelay(ctx context.Context, conn *sidecarproto.Conn, namespace, op string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("agentproc: marshal relay %s/%s args: %w", namespace, op, err)
	}
	return conn.Call(ctx, sidecarproto.KindRelayCall, sidecarproto.RelayCallBody{Namespace: namespace, Op: op, Args: raw}, out)
}

// NotifyRelay is the sidecar-side sender for a fire-and-forget relay op. Marshal
// failures are non-fatal (the op is best-effort audit) and returned for logging
// only.
func NotifyRelay(conn *sidecarproto.Conn, namespace, op string, args any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("agentproc: marshal relay %s/%s args: %w", namespace, op, err)
	}
	return conn.Notify(sidecarproto.KindRelayNotify, sidecarproto.RelayCallBody{Namespace: namespace, Op: op, Args: raw})
}
