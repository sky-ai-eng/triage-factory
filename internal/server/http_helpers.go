package server

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
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

func decodeJSON(w http.ResponseWriter, r *http.Request, v any, msg string) bool {
	return httpx.DecodeJSON(w, r, v, msg)
}

func isUniqueViolation(err error) bool { return httpx.IsUniqueViolation(err) }

func localDetail(err error) string { return httpx.LocalDetail(err) }

func requireOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	return httpx.RequireOrg(w, r)
}

// writeNotConfigured answers a route that requires an integration the org
// hasn't set up: 409 NOT_CONFIGURED. A 409 rather than a 400 because the
// fault is server-side state, not the request — and never a 500, which is
// reserved for a backend that failed to *read* the configuration.
func writeNotConfigured(w http.ResponseWriter, msg string) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonNotConfigured,
		Message: msg,
	})
}

// forbidden writes a 403: the caller is authenticated and can see the
// resource, but the action is refused. Callers that must NOT disclose the
// resource's existence use notFound instead — the disclosure rule decides
// which of the two a denial gets, so the two helpers stay distinct.
func forbidden(w http.ResponseWriter, msg string) {
	httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
		Reason:  httpx.ReasonForbidden,
		Message: msg,
	})
}

// conflict writes a 409 under the generic CONFLICT reason — the request is
// well-formed but the resource's current state refuses it. A conflict with a
// named class (a duplicate name, an already-terminal row, a missing
// integration) uses its own reason via WriteErrors instead.
func conflict(w http.ResponseWriter, msg string) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonConflict,
		Message: msg,
	})
}

// uuidPathOr404 validates a {param} path value as a UUID and answers 404 for
// a malformed one. On Postgres these ids are uuid columns, so a malformed
// value would otherwise surface as SQLSTATE 22P02 from the first store call →
// 500. Answering "not found" keeps the response identical across dialects
// (SQLite stores the same ids as TEXT and simply matches nothing) and matches
// the disclosure rule: a malformed id names nothing the caller may learn
// about. thing is the noun for the message ("project", "conversation").
func uuidPathOr404(w http.ResponseWriter, r *http.Request, param, thing string) (string, bool) {
	id := r.PathValue(param)
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, thing)
		return "", false
	}
	return id, true
}

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
