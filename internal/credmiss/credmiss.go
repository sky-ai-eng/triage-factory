// Package credmiss answers a request that a credential proxy could not find a
// credential for. The git proxy, the REST API proxy and the gh injector each
// resolve a credential per request and share this one answer: a status chosen
// by whether a retry can succeed, a JSON body naming the reason, and one log
// line per reason for the operator.
//
// The reason comes from credbundle.MissReason, a closed vocabulary. It is the
// only part of the failure that leaves the proxy. The error itself is never
// logged or returned, because it can carry App or credential-setup detail, and
// the repository the request named is never logged, because it is tenant data.
package credmiss

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
)

// Status is the HTTP status a credential miss is answered with.
//
// A run's bundle carries the credentials it was provisioned for, so a request
// for a repository it has no token for, or for a gh or Jira credential the org
// never supplied, misses on every retry. Those answer 403: git and every
// GitHub client read it as a refusal rather than an outage, and the remedy is
// a different action (a `workspace add`, which waits for its own re-seal, or
// an admin binding the credential), never the same request again. A missing
// bundle may still arrive and an expiring token may be refreshed, so those
// keep the 502 a retry answers, as does any failure without a named reason.
func Status(reason string) int {
	switch reason {
	case credbundle.MissNoRepoToken, credbundle.MissNoCLIToken, credbundle.MissNoJiraCredential:
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

// Responder writes one proxy's credential-miss answers and logs each distinct
// reason the first time it occurs. A proxy instance serves one run, so in
// practice that is one line per run per reason; an agent retrying in a loop
// gets the same answer every time and adds nothing to the log.
type Responder struct {
	proxy          string
	conversationID string
	log            *slog.Logger

	mu     sync.Mutex
	logged map[string]bool
}

// NewResponder builds the responder for one proxy instance. proxy names the
// proxy in the body and the log line; conversationID attributes the log line
// and may be empty.
func NewResponder(proxy, conversationID string, log *slog.Logger) *Responder {
	return &Responder{proxy: proxy, conversationID: conversationID, log: log, logged: map[string]bool{}}
}

// body is the answer's shape. `message` is the member gh prints and GitHub API
// clients read, and a JSON body is what lets an HTTP client tell this 403
// apart from the HTML page a proxy in front of an outage returns.
type body struct {
	Message string `json:"message"`
}

// Respond answers a request whose credential lookup failed with err.
func (r *Responder) Respond(w http.ResponseWriter, err error) {
	reason := credbundle.MissReason(err)
	if r.firstTime(reason) {
		r.log.Warn("credential lookup failed",
			"proxy", r.proxy, "conversation", r.conversationID, "reason", reason)
	}
	// Marshalling a struct of one string cannot fail.
	b, _ := json.Marshal(body{Message: r.proxy + ": no credential for this request (" + reason + ")"})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(Status(reason))
	_, _ = w.Write(b)
}

func (r *Responder) firstTime(reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logged[reason] {
		return false
	}
	r.logged[reason] = true
	return true
}
