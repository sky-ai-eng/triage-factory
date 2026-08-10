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
	// OpRecordObservation is the gh-channel injector's fire-and-forget report of
	// an artifact-bearing mutation the real gh performed (a PR created, a review
	// posted). The exec-verb channel self-reports its writes; the gh channel has
	// no verb layer, so the injector observes the two mutation shapes and relays
	// the coordinates. The orchestrator (which holds the DB and domain types)
	// builds and upserts the artifact row — the capless sidecar never does.
	OpRecordObservation = "record_observation"
	// OpRecordEgressDenial is the egress proxy's fire-and-forget report of a
	// refused CONNECT. Same shape as record_denial, one layer down: the proxy
	// runs in the capless sidecar and the audit row is a DB write, so the
	// coordinates travel up and the orchestrator writes.
	OpRecordEgressDenial = "record_egress_denial"
	// OpRecordGHWrite is the gh-channel injector's fire-and-forget report of a
	// write it forwarded, with the upstream's outcome. Distinct from
	// record_observation, which reports only the artifact-bearing creates: this
	// one covers every mutating REST method and every GraphQL mutation, on both
	// outcomes, so an edit, a merge, and a refused write all leave a trace.
	// A GraphQL write carries what its request envelope disclosed alongside the
	// wire facts, since its path says only that a POST reached /graphql.
	OpRecordGHWrite = "record_gh_write"
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
// fallback. Both empty on an allowed decision. ProtectedRefs does the same job
// one level down, for a REF-level denial: it names the refs the team's
// base-branch push policy excluded, so the sandbox's receive-pack gate can say
// "that is the base branch" rather than a flat "ref not allowed". It rides an
// ALLOWED reply (the refusal it explains happens per-ref, after the repo was
// authorized) and is empty when the policy permits base-branch pushes.
type AuthorizeRepoReply struct {
	Allowed       bool     `json:"allowed"`
	AllowedRefs   []string `json:"allowed_refs,omitempty"`
	ProtectedRefs []string `json:"protected_refs,omitempty"`
	DenyReason    string   `json:"deny_reason,omitempty"`
	DenyMessage   string   `json:"deny_message,omitempty"`
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

// RecordEgressDenialArgs is record_egress_denial's payload: the CONNECT
// authority the sandbox asked for (host:port) and the policy reason it was
// refused, mirroring egressproxy.DeniedConnect across the wire. The
// orchestrator binds the conversation from its own RunInfo, so a sidecar can
// never attribute a probe to another run.
type RecordEgressDenialArgs struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// RecordGHWriteArgs is record_gh_write's payload: the method, upstream path,
// and response status of one write the gh-channel injector forwarded, plus the
// created object's id and link when the shape is one whose RESPONSE names an
// object (a posted comment or reply). The orchestrator classifies these into
// the semantic act; the sidecar only carries wire facts.
//
// A REST write is fully named by method+path and carries no GraphQL member. A
// GraphQL write's path says only "/graphql", so what names it rides in GraphQL
// instead — read from the request envelope, which is the only place it appears.
type RecordGHWriteArgs struct {
	Method         string             `json:"method"`
	Path           string             `json:"path"`
	Status         int                `json:"status"`
	ExternalID     string             `json:"external_id,omitempty"`
	URL            string             `json:"url,omitempty"`
	Errored        bool               `json:"errored,omitempty"`
	ResponseUnread bool               `json:"response_unread,omitempty"`
	GraphQL        *GraphQLWriteFacts `json:"graphql,omitempty"`
}

// GraphQLWriteFacts mirrors ghwrite.GraphQLFacts across the wire. It is spelled
// out here rather than imported so the relay protocol stays a description of
// what crosses the socket, independent of the classifier's own types — the same
// reason the REST members are flat fields and not a shared struct.
//
// Names and ids only. The request these come from also carried the comment
// bodies and review text the agent composed, and none of it is extracted, so
// none of it can cross this hop.
type GraphQLWriteFacts struct {
	// Operation is the operation name the document gave, empty if anonymous.
	Operation string `json:"operation,omitempty"`
	// Mutations are the top-level field names the mutation selected, aliases
	// already resolved to the fields actually invoked.
	Mutations []string `json:"mutations,omitempty"`
	// Subject is the node id the variables named for the object acted on.
	Subject string `json:"subject,omitempty"`
	// Unreadable names why the request resolved to no single act, empty when it
	// resolved cleanly.
	Unreadable string `json:"unreadable,omitempty"`
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

// RecordObservationArgs is record_observation's payload: the coordinates of an
// artifact-bearing mutation the gh-channel injector saw complete. Kind is
// "pull_request" or "review". A PR created through gh's porcelain (GraphQL)
// carries only Number/NodeID/URL, since that is gh's whole selection set — the
// reconciler fills the rest; one created through `gh api` (REST) also carries
// Head/Base/Title/Body/Draft from the 201 response. For a review post, Number
// comes from the request path and ReviewID/ReviewState/URL from the response.
// The orchestrator binds ConversationID/OrgID/TeamID from its own RunInfo (the
// sidecar never names them), so a sidecar cannot attribute an artifact to
// another run.
type RecordObservationArgs struct {
	Kind        string `json:"kind"`
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	NodeID      string `json:"node_id,omitempty"`
	Head        string `json:"head,omitempty"`
	Base        string `json:"base,omitempty"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
	Body        string `json:"body,omitempty"`
	Draft       bool   `json:"draft,omitempty"`
	ReviewID    int    `json:"review_id,omitempty"`
	ReviewState string `json:"review_state,omitempty"`
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
