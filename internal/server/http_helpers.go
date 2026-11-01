package server

import (
	"context"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// This file is a thin shim over internal/server/httpx, the shared HTTP kernel
// that per-domain subpackages import directly. The wrappers let the handlers
// still living in package server keep their short, unqualified names; as each
// domain moves into its own subpackage its call sites switch to httpx.*, and
// the matching wrapper here is deleted once its last in-package caller is gone.
// New package-server code may call either form.

func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }

func internalError(w http.ResponseWriter, scope string, err error) {
	httpx.InternalError(w, scope, err)
}

func notFound(w http.ResponseWriter, thing string) { httpx.NotFound(w, thing) }

func badRequest(w http.ResponseWriter, msg string) { httpx.BadRequest(w, msg) }

func writeUnauth(w http.ResponseWriter) { httpx.WriteUnauth(w) }

func isUniqueViolation(err error) bool { return httpx.IsUniqueViolation(err) }

func localDetail(err error) string { return httpx.LocalDetail(err) }

func requireOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	return httpx.RequireOrg(w, r)
}

func writeNotConfigured(w http.ResponseWriter, msg string) { httpx.WriteNotConfigured(w, msg) }

func notConfiguredFault(msg string) *httpx.Fault { return httpx.NotConfiguredFault(msg) }

// writeSourceDisabled writes the 409 for a request that needs a source an org
// admin turned off (eventsource.ErrDisabled): every Jira or Linear client is
// refused for the org while it is off, so the request cannot do its work, and
// nothing it changes would let it.
func writeSourceDisabled(w http.ResponseWriter, kind string) {
	httpx.WriteFault(w, sourceDisabledFault(kind))
}

func sourceDisabledFault(kind string) *httpx.Fault {
	return httpx.NewFault(http.StatusConflict, httpx.ReasonSourceDisabled,
		eventsource.Label(kind)+" is turned off for this organization by an org admin; ask an org admin to turn it back on")
}

func forbidden(w http.ResponseWriter, msg string) { httpx.Forbidden(w, msg) }

func conflict(w http.ResponseWriter, msg string) { httpx.Conflict(w, msg) }

func uuidPathOr404(w http.ResponseWriter, r *http.Request, param, thing string) (string, bool) {
	return httpx.UUIDPathOr404(w, r, param, thing)
}

func teamIDFilterField(v *httpx.Validation, teamID string) { httpx.TeamIDFilterField(v, teamID) }

// requireOrg is the Server-method form of the package-level org gate, so
// handlers written as Server methods use the same path.
func (s *Server) requireOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	return httpx.RequireOrg(w, r)
}

// ClaimsFrom and OrgIDFrom forward to httpx so existing package-server call
// sites keep their short names. Session identity stays in package server
// (SessionFrom in middleware.go) — only the auth and orgs handlers read it,
// and those live in the root package.
func ClaimsFrom(ctx context.Context) *verify.Claims { return httpx.ClaimsFrom(ctx) }

func OrgIDFrom(ctx context.Context) string { return httpx.OrgIDFrom(ctx) }

func authIdentityIDFrom(ctx context.Context) string { return httpx.AuthIdentityIDFrom(ctx) }
