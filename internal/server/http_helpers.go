package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// internalError logs the error with the given scope tag and writes a
// 500 to the client. In local mode the raw err.Error() is returned so a
// developer staring at their own machine can read it; in multi mode the
// client sees a generic message and the detail stays in server logs
// only — raw Go errors (driver messages, file paths, internal IDs)
// must not leak to other tenants' browsers.
//
// scope is the short subsystem tag that previously appeared in inline
// log.Printf calls (e.g. "tasks", "projects", "reviews").
func internalError(w http.ResponseWriter, scope string, err error) {
	log.Printf("[%s] %v", scope, err)
	msg := err.Error()
	if runmode.Current() == runmode.ModeMulti {
		msg = "internal server error"
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
}

// notFound writes a 404 with a "<thing> not found" message. Centralized
// so the wording stays consistent across handlers.
func notFound(w http.ResponseWriter, thing string) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": thing + " not found"})
}

// badRequest writes a 400 with the given message.
func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

// isUniqueViolation reports whether err is a unique-constraint failure on
// either driver — SQLite returns "UNIQUE constraint failed: ...", Postgres
// "duplicate key value violates unique constraint ...". Handlers that pre-check
// a uniqueness invariant for a clean 409 in the common case still need this to
// catch the narrow concurrent-writer race where two requests pass the
// (separate-transaction) pre-check before either writes; the index is the
// authority and surfaces here rather than as a raw 500. The driver strings leak
// schema/index names, so callers translate to a generic message and log the raw
// error.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint") || strings.Contains(s, "duplicate key")
}

// requireOrg returns the active org ID from request context. In multi
// mode an empty value means the user has no active org (zero
// memberships, or POST /api/me/active-org hasn't been called yet);
// the response is 409 with the stable "no_active_org" error code so
// the frontend can prompt the user to pick one. In local mode the
// shim guarantees a sentinel orgID so the empty branch never fires.
//
// Usage: `orgID, ok := s.requireOrg(w, r); if !ok { return }`.
func (s *Server) requireOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID := OrgIDFrom(r.Context())
	if orgID != "" {
		return orgID, true
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":   "no_active_org",
		"message": "no active org selected; call POST /api/me/active-org to choose one",
	})
	return "", false
}

// decodeJSON decodes the request body into v. On failure it writes a
// 400 with the given message (or "invalid request body" if msg is empty)
// and returns false; callers should `return` immediately.
//
// Use:
//
//	var req CreateFooReq
//	if !decodeJSON(w, r, &req, "") {
//	    return
//	}
//
// v must be a pointer. Anonymous-struct request shapes are supported by
// passing &req where req is the local var.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, msg string) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if msg == "" {
			msg = "invalid request body"
		}
		badRequest(w, msg)
		return false
	}
	if dec.More() {
		if msg == "" {
			msg = "invalid request body"
		}
		badRequest(w, msg)
		return false
	}
	return true
}
