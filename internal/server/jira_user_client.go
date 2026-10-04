package server

import (
	"errors"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jiraoauth"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// writeJiraUserClientError answers a request whose acting user's Jira client
// could not be resolved, by who can fix what stopped it.
//
// Only the user connecting again fixes a missing or refused credential, so both
// are 409 NOT_CONFIGURED, the refusal saying why. A Cloud OAuth mint the token
// endpoint did not answer, or rate limited, clears on a retry; a rate limit is
// 429 RATE_LIMITED, the status that reason has wherever it is answered. One
// it refused for any reason but the user's grant is about the org's OAuth app,
// which an admin fixes. Anything else is this server's fault.
//
// The upstream answers are read only off the mint's own error types: a secret
// store read that failed on a network error is classified Transient too, and
// is not Jira's.
func writeJiraUserClientError(w http.ResponseWriter, scope string, err error) {
	switch {
	case errors.Is(err, jira.ErrJiraUserCredentialRefused):
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: "Jira no longer accepts your connection: it was revoked or expired. Connect your Jira again to act on tickets.",
		})
		return
	case errors.Is(err, jira.ErrNoJiraUserCredential):
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: "connect your Jira to act on tickets",
		})
		return
	}

	var class upstream.Class
	var statusErr *jiraoauth.StatusError
	var transportErr *upstream.TransportError
	switch {
	case errors.As(err, &statusErr):
		class = statusErr.Class
	case errors.As(err, &transportErr):
		class = upstream.Transient
	default:
		internalError(w, scope, err)
		return
	}
	serverLog.Warn("could not mint the acting user's Jira access token", "scope", scope, "class", class, "error", err)
	switch class {
	case upstream.RateLimited:
		httpx.WriteErrors(w, http.StatusTooManyRequests, httpx.ErrorItem{
			Reason:  httpx.ReasonRateLimited,
			Message: "Jira is rate limiting sign-in requests; try again shortly" + httpx.LocalDetail(err),
		})
	case upstream.Transient:
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
			Reason:  httpx.ReasonUpstreamUnavailable,
			Message: "Jira's sign-in service did not answer; try again" + httpx.LocalDetail(err),
		})
	default:
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
			Reason:  httpx.ReasonUpstreamRejected,
			Message: "Jira refused to issue an access token for this org's Jira app; an org admin should check the app's client ID and secret" + httpx.LocalDetail(err),
		})
	}
}
