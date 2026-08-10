package ghinjector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
)

// The injector's one traffic policy: three families of write are refused before
// they are forwarded, and so is any GraphQL request whose act could not be
// established. What is refused and why lives in internal/ghwrite — the same
// table the audit builder reads — so a shape cannot be named one thing by the
// log and another by the gate. This file is only the enforcement: recognize,
// ask, refuse.
//
// It sits here rather than in the command-level matcher the delegated runtime
// applies because this is the choke point. The matcher sees what a model typed;
// this sees what actually happened, which is not the same thing — a document
// shaped to dodge a string match reaches GitHub, any client holding the per-run
// placeholder (curl included) bypasses the matcher entirely, and the SDK runtime
// has no matcher seam at all.

// refusalStatus is what a gated request gets. 403 rather than 405 or 400: the
// request is well-formed and the endpoint exists, and what happened is that
// this caller may not do it.
const refusalStatus = http.StatusForbidden

// refusalBody is the synthesized response. `message` is where both `gh` and the
// GitHub API clients look for the human-readable half, so the refusal reads as
// an ordinary API error rather than as a broken proxy; the structured member
// carries the parts a model can act on without parsing prose.
type refusalBody struct {
	Message       string              `json:"message"`
	TriageFactory ghwrite.Explanation `json:"triage_factory"`
}

// refused decides one request and, when the decision is to refuse, answers it.
// It reports whether the response has already been written, in which case the
// caller must not forward.
//
// facts is what the GraphQL capture read, nil for every REST request and for a
// GraphQL request proven to be a read.
func (s *Server) refused(w http.ResponseWriter, r *http.Request, facts *ghwrite.GraphQLFacts) bool {
	req, checkable := gateRequest(r, facts)
	if !checkable {
		return false
	}
	ref, gated := ghwrite.Gate(req)
	if !gated {
		return false
	}
	// The decision is the orchestrator's, not this process's: it owns the
	// policy and the database the refusal is recorded in, and this process is
	// the capless one holding a live credential. A hook that is absent, errors,
	// or times out yields false — a gated shape whose decision could not be
	// obtained is refused, since the alternative makes a broken relay the way
	// through.
	if s.cfg.AuthorizeWrite != nil && s.cfg.AuthorizeWrite(r.Context(), req, ref) {
		return false
	}

	if ref.Reason == ghwrite.GateReasonUnreadable {
		// Anomalous by construction: no document the pinned `gh` sends comes
		// close to the buffering cap, uses an undefined fragment, or arrives
		// without an operation to select. A non-zero rate here is either a real
		// client this broke or someone probing the boundary, and those need
		// telling apart by someone reading logs.
		injectorLog.Warn("refused a gh-channel request whose act could not be read",
			"run", s.cfg.RunID, "reason", ref.Unreadable, "path", req.Path,
			"content_length", r.ContentLength)
	} else {
		injectorLog.Info("refused a gh-channel write",
			"run", s.cfg.RunID, "reason", ref.Reason, "act", ref.Action, "mutation", ref.Mutation)
	}

	explanation := ref.Explain()
	body, err := json.Marshal(refusalBody{
		Message:       explanation.Policy + " " + explanation.NextStep,
		TriageFactory: explanation,
	})
	if err != nil {
		// Nothing here is caller-supplied — the explanation is composed from
		// constants — so this cannot happen for a reason the agent controls.
		// The refusal still stands; only its readability degrades.
		body = []byte(`{"message":"This request was refused by Triage Factory."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(refusalStatus)
	_, _ = w.Write(body)
	return true
}

// gateRequest reshapes an inbound request into what the policy reads. ok is
// false for a request the policy cannot apply to at all — a GraphQL request
// already established as a read, which is nearly all of that endpoint's
// traffic.
//
// The REST path is the upstream one, with the GHE prefix `gh` emits stripped
// exactly as rewrite strips it. That keeps a denial row's path in the same
// vocabulary as every other gh-channel audit row's, so the two can be read side
// by side.
func gateRequest(r *http.Request, facts *ghwrite.GraphQLFacts) (ghwrite.Request, bool) {
	if isGraphQLPath(r.URL.Path) {
		if facts == nil {
			return ghwrite.Request{}, false
		}
		return ghwrite.Request{Method: r.Method, Path: r.URL.Path, GraphQL: facts}, true
	}
	return ghwrite.Request{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, restPrefix)}, true
}

// AuthorizeWrite is the gate's decision hook: the injector has recognized a
// gated shape and asks whether it may transit anyway. True lets it through.
//
// Multi mode relays it to the orchestrator; local mode calls the same decision
// in process. Neither ever returns true in V1 — the refusal admits no exception
// — and the boolean exists because the decision belongs on the side that holds
// the policy and the audit log, not because there is an authorized case to
// express. The orchestrator re-derives the shape from the shared table rather
// than trusting this side's reading, so a bug here can only refuse more.
//
// The hook is also where the refusal is RECORDED, which is why it is a call and
// not a notification: a denial is the security record of the request, and one
// that rode a fire-and-forget channel could be lost without a trace.
type AuthorizeWrite func(ctx context.Context, req ghwrite.Request, ref ghwrite.Refusal) bool
