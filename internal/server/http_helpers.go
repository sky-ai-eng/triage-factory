package server

import (
	"context"
	"net/http"

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

func requireOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	return httpx.RequireOrg(w, r)
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
