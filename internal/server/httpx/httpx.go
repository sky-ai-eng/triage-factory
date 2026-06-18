// Package httpx is the shared HTTP kernel for the Triage Factory server: the
// JSON response writers, the request-body decoder, and the request-scoped
// identity accessors (verified JWT claims + active org). It is the leaf
// package every per-domain handler imports, so it depends only on other leaf
// packages (auth/verify, runmode) and never on package server. That one-way
// edge is what lets handlers move into their own subpackages without an
// import cycle: the root composes and registers them, they reach back only
// as far as this kernel.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// StatusClientClosedRequest is the non-standard 499 code (the nginx
// convention for "client closed the request before the server answered").
// net/http defines no constant for it. InternalError records it instead of a
// 500 when a handler's error is really a canceled/expired request context, so
// an aborted request stays out of 5xx logs and metrics.
const StatusClientClosedRequest = 499

// --- JSON responses ---

// WriteJSON writes v as a JSON body with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// InternalError logs the error under the scope tag and writes a 500. In local
// mode the raw err.Error() is returned so a developer staring at their own
// machine can read it; in multi mode the client sees a generic message and the
// detail stays in the server log only — raw Go errors (driver messages, file
// paths, internal IDs) must not leak to other tenants' browsers. scope is the
// short subsystem tag (e.g. "tasks", "projects", "reviews").
func InternalError(w http.ResponseWriter, scope string, err error) {
	// A canceled or timed-out request context means the caller went away
	// (navigation, component remount, React StrictMode double-fire) before the
	// handler finished — the canceled context propagated up through whatever
	// query was mid-flight. That's not a server fault: an error-level log pages
	// on-call and a 500 inflates 5xx metrics for a client we can no longer
	// reach. Record a 499 with a debug breadcrumb instead. The header write is
	// best-effort — the socket is usually already closed and net/http drops the
	// write silently.
	if IsClientGone(err) {
		logging.Component(scope).Debug("request canceled by client", "error", err)
		w.WriteHeader(StatusClientClosedRequest)
		return
	}
	logging.Component(scope).Error("handler error", "error", err)
	msg := err.Error()
	if runmode.Current() == runmode.ModeMulti {
		msg = "internal server error"
	}
	WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
}

// IsClientGone reports whether err carries a canceled or deadline-exceeded
// request context — the signature of a caller that disconnected (or a request
// whose deadline elapsed) while a handler was mid-flight. The canceled context
// surfaces from whatever query was running when it tripped, usually wrapped
// (fmt.Errorf("...: %w", err)), so this matches via errors.Is rather than
// equality. Callers treat a true result as "the caller is gone," not a server
// error — see InternalError. Exported alongside IsUniqueViolation as part of
// the kernel's error vocabulary so other choke points can share the one
// definition instead of re-deriving the check.
func IsClientGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// NotFound writes a 404 with a "<thing> not found" message. Centralized so the
// wording stays consistent across handlers.
func NotFound(w http.ResponseWriter, thing string) {
	WriteJSON(w, http.StatusNotFound, map[string]string{"error": thing + " not found"})
}

// BadRequest writes a 400 with the given message.
func BadRequest(w http.ResponseWriter, msg string) {
	WriteJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

// WriteUnauth writes a 401.
func WriteUnauth(w http.ResponseWriter) {
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}

// DecodeJSON decodes a single top-level JSON value from the request body into
// v (which must be a pointer). It rejects any trailing content after that
// value — a second value, junk bytes, or a stray }/] — and allows only
// trailing whitespace. On failure it writes a 400 (msg, or "invalid request
// body" when msg is empty) and returns false; callers should return
// immediately.
//
// The trailing check requires the next decode to hit io.EOF rather than using
// dec.More(): More() reports false at a top-level } or ], so a body like
// `{...}}` would otherwise be accepted.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any, msg string) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if msg == "" {
			msg = "invalid request body"
		}
		BadRequest(w, msg)
		return false
	}
	var rest json.RawMessage
	if err := dec.Decode(&rest); err != io.EOF {
		if msg == "" {
			msg = "invalid request body"
		}
		BadRequest(w, msg)
		return false
	}
	return true
}

// IsUniqueViolation reports whether err is a unique-constraint failure on
// either driver — SQLite returns "UNIQUE constraint failed: ...", Postgres
// "duplicate key value violates unique constraint ...". Handlers that pre-check
// a uniqueness invariant for a clean 409 still need this to catch the narrow
// concurrent-writer race where two requests pass the (separate-transaction)
// pre-check before either writes; the index is the authority and surfaces here
// rather than as a raw 500. The driver strings leak schema/index names, so
// callers translate to a generic message and log the raw error.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint") || strings.Contains(s, "duplicate key")
}

// --- request-scoped identity ---

// ctxKey is unexported so identity can only be read or written through the
// accessors below — prevents accidental shadowing across the package boundary.
type ctxKey int

const (
	ctxKeyClaims ctxKey = iota
	ctxKeyOrgID
)

// WithClaims returns ctx carrying the verified JWT claims. The session
// middleware sets this once per request.
func WithClaims(ctx context.Context, c *verify.Claims) context.Context {
	return context.WithValue(ctx, ctxKeyClaims, c)
}

// ClaimsFrom returns the verified JWT claims set by the session middleware, or
// nil if the request didn't pass through it. Handlers that depend on a claim
// should fail closed on nil; the middleware would have already rejected an
// unauthenticated request, so nil inside a protected handler indicates a
// route-registration bug.
func ClaimsFrom(ctx context.Context) *verify.Claims {
	v, _ := ctx.Value(ctxKeyClaims).(*verify.Claims)
	return v
}

// WithOrgID returns ctx carrying the active org for the request.
func WithOrgID(ctx context.Context, orgID string) context.Context {
	return context.WithValue(ctx, ctxKeyOrgID, orgID)
}

// OrgIDFrom returns the active org for the request. Empty string when the
// caller is multi-mode and has no active org (zero memberships, or the
// active-org switch hasn't been called) — handlers that require an org should
// gate via RequireOrg so the SPA can prompt the user to pick/join one.
func OrgIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyOrgID).(string)
	return v
}

// RequireOrg returns the active org ID from request context, or writes a 409
// with the stable "no_active_org" error code and returns ok=false. In local
// mode the shim guarantees a sentinel org so the empty branch never fires.
// Usage: orgID, ok := httpx.RequireOrg(w, r); if !ok { return }
func RequireOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID := OrgIDFrom(r.Context())
	if orgID != "" {
		return orgID, true
	}
	WriteJSON(w, http.StatusConflict, map[string]string{
		"error":   "no_active_org",
		"message": "no active org selected; call POST /api/me/active-org to choose one",
	})
	return "", false
}
