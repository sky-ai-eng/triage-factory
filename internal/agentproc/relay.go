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
// record_denial / record_push are its audit records, sent as KindRelayRecord
// through the run's RecordSender. The exec verb-trace DB ops are added to this
// catalog alongside the relocated agenthost; they share the same envelope.
const (
	OpAuthorizeRepo = "authorize_repo"
	OpRecordDenial  = "record_denial"
	OpRecordPush    = "record_push"
	// OpRecordEgressDenial is the egress proxy's audit record of a refused
	// CONNECT. Same shape as record_denial, one layer down: the proxy
	// runs in the capless sidecar and the audit row is a DB write, so the
	// coordinates travel up and the orchestrator writes.
	OpRecordEgressDenial = "record_egress_denial"
	// OpRecordGHWrite is the gh-channel injector's audit record of a write it
	// forwarded, with the upstream's outcome. It covers every mutating
	// REST method and every GraphQL mutation, on both outcomes, so an edit, a
	// merge, and a refused write all leave a trace.
	// A GraphQL write carries what its request envelope disclosed alongside the
	// wire facts, since its path says only that a POST reached /graphql.
	OpRecordGHWrite = "record_gh_write"
	// OpAuthorizeGHWrite is the gh-channel injector's per-request decision on a
	// shape the refusal policy gates — a merge, a submitted review, a
	// repository creation, or a GraphQL request whose act could not be read.
	// A Call rather than a record for two reasons: the request is held until
	// the answer arrives (a refusal that raced the forward would be no refusal
	// at all), and the denial row is written by the handler before it answers,
	// so the row is in the ledger by the time the agent sees the refusal.
	OpAuthorizeGHWrite = "authorize_gh_write"
	// OpSignDigest asks the control plane to sign one commit's digest as the
	// run's signer. A Call: git waits on the signature to write the commit.
	// The orchestrator queues the request for the control pods, which hold
	// the signing keys, and answers with what they decided, so neither the
	// sidecar nor the orchestrator ever holds a key.
	OpSignDigest = "sign_digest"
	// OpRecordCommitSHA is the sidecar daemon's record of the commit id a
	// signed digest gave the commit, for the signature's commit_signatures
	// row: a push of that commit names its signer from it.
	OpRecordCommitSHA = "record_commit_sha"
	// OpRecordSignRefusal is the sidecar daemon's record of a commit it
	// refused before asking any signer, a committer that is not the run's
	// signer or something that is not a commit. The sidecar has no database
	// and no metrics, so the orchestrator counts it and, for a committer
	// mismatch, writes its commit_sign_denied row.
	OpRecordSignRefusal = "record_sign_refusal"
)

// SignDigestArgs is sign_digest's payload: the hex SHA-512 digest of the commit
// payload the sidecar's daemon checked, and the signer the run was sealed with
// (its kind, "org" or "deployment", and its GitHub account id). It carries no
// org: the orchestrator binds the org, conversation and engagement from its
// own identity for the run, and a control pod signs only for the signer the
// org resolves to when it signs, so a signer named here that is not the org's
// is refused.
type SignDigestArgs struct {
	Digest       string `json:"digest"`
	Signer       string `json:"signer"`
	SignerUserID int64  `json:"signer_user_id"`
}

// SignDigestReply is what came of the request. Outcome is "signed" (Signature
// holds the armored SSH signature), "refused" (Reason names why, and
// RetryAfterSeconds, set only for the org's per-minute budget, says how long
// until it admits another) or "unanswered" (nothing answered in time; the run
// may try the same command again). Limit is the budget a refusal by the
// per-minute or per-claim budget ran into, and SignerStatus and
// SignerStatusReason the signer's status and its reason for a refusal because
// the signer is not active; the agent's refusal line names them. SignatureID
// names the request's commit_signatures row, which the commit id the daemon
// computes is recorded on.
type SignDigestReply struct {
	Outcome            string `json:"outcome"`
	SignatureID        string `json:"signature_id,omitempty"`
	Signature          string `json:"signature,omitempty"`
	Reason             string `json:"reason,omitempty"`
	RetryAfterSeconds  int    `json:"retry_after_seconds,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	SignerStatus       string `json:"signer_status,omitempty"`
	SignerStatusReason string `json:"signer_status_reason,omitempty"`
}

// RecordCommitSHAArgs is record_commit_sha's payload: the signature's row, as
// sign_digest's reply named it, and the commit id the daemon computed from the
// signature. The orchestrator stamps it only on a row of the run's own.
type RecordCommitSHAArgs struct {
	SignatureID string `json:"signature_id"`
	CommitSHA   string `json:"commit_sha"`
}

// RecordSignRefusalArgs is record_sign_refusal's payload: the refusal
// ("committer_mismatch" or "not_a_commit") and, for a committer mismatch, the
// committer the commit named. It names no signer: the orchestrator records the
// refusal against the signer the sidecar reported at bring-up, before the
// agent ran, and binds the org and conversation from its own identity for the
// run.
type RecordSignRefusalArgs struct {
	Reason         string `json:"reason"`
	CommitterName  string `json:"committer_name,omitempty"`
	CommitterEmail string `json:"committer_email,omitempty"`
}

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
// orchestrator binds the conversation from its own ConversationInfo, so a sidecar can
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
	NodeID         string             `json:"node_id,omitempty"`
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
	// NamesReview reports that the request supplied a pending review's id. The
	// gate reads it, so it crosses on authorize_gh_write as well as here.
	NamesReview bool `json:"names_review,omitempty"`
}

// AuthorizeGHWriteArgs is authorize_gh_write's payload: one request the
// injector recognized as a gated shape, described in the same wire facts
// record_gh_write carries — because the orchestrator re-derives the decision
// from the shared classifier rather than trusting the sidecar's reading of it.
// Nothing about the refusal itself crosses the wire in this direction; the
// sidecar says what was asked, and the side holding the policy says what that
// means.
type AuthorizeGHWriteArgs struct {
	Method  string             `json:"method"`
	Path    string             `json:"path"`
	GraphQL *GraphQLWriteFacts `json:"graphql,omitempty"`
}

// AuthorizeGHWriteReply is the decision. Allowed is true only for a request the
// orchestrator's own reading finds ungated — there is no authorization that
// admits a gated one, by design. A refused request has already had its audit
// row written by the time this reply is sent.
type AuthorizeGHWriteReply struct {
	Allowed bool `json:"allowed"`
}

// RecordPushArgs is record_push's payload: one branch ref a receive-pack
// transited plus the upstream's answer for it, mirroring gitproxy.PushedRef
// across the wire. The orchestrator reshapes it into the same branch deliverable /
// push-failed row the in-process backstop writes. Repo is the "owner/repo" the
// proxy parsed from the request path; Created marks a newly-created ref; Status
// is the receive-pack response code; Rejected, Reason and RemoteMessage carry
// the response's report-status refusal of the ref and the remote's message
// text (the push landed only for a 2xx Status with Rejected false).
type RecordPushArgs struct {
	Repo          string `json:"repo"`
	Ref           string `json:"ref"`
	NewSHA        string `json:"new_sha"`
	Created       bool   `json:"created"`
	Status        int    `json:"status"`
	Rejected      bool   `json:"rejected"`
	Reason        string `json:"reason"`
	RemoteMessage string `json:"remote_message"`
}

// RelayDispatcher serves the sidecar's org-bound relay ops. The concrete impl
// (agenthost.RelayServer) holds the run's db.Stores + ConversationInfo + git gate and
// binds identity from ConversationInfo, so a relayed op can never be steered at another
// org. DispatchCall serves a request/response op (KindRelayCall); DispatchRecord
// applies an audit record (KindRelayRecord). The supervisor holds one of these
// for the run's lifetime.
type RelayDispatcher interface {
	// DispatchCall serves (namespace, op) with the request args and returns the
	// op's marshaled result, or an error whose text relays back to the sidecar.
	DispatchCall(ctx context.Context, namespace, op string, args json.RawMessage) (json.RawMessage, error)
	// DispatchRecord applies one audit record. nil tells the sidecar it may
	// forget the record: it was applied, or it never can be (an undecodable
	// record, an op nothing serves), which the dispatcher has already counted.
	// An error tells the sidecar the record was not applied and to send it
	// again, under the same key, so the dispatcher must be able to apply one
	// record twice without writing two rows.
	DispatchRecord(ctx context.Context, namespace, op, key string, args json.RawMessage) error
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
