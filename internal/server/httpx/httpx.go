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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// StatusClientClosedRequest is the non-standard 499 code (the nginx
// convention for "client closed the request before the server answered").
// net/http defines no constant for it. InternalError records it instead of a
// 500 when a handler's error is really a canceled request context, so an
// aborted request stays out of 5xx logs and metrics.
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
	// A canceled request context means the caller went away (navigation,
	// component remount, React StrictMode double-fire) before the handler
	// finished — the cancellation propagated up through whatever query was
	// mid-flight. That's not a server fault: an error-level log pages on-call
	// and a 500 inflates 5xx metrics for a client we can no longer reach.
	// Record a 499 with a debug breadcrumb instead. The header write is
	// best-effort — the socket is usually already closed and net/http drops the
	// write silently.
	//
	// Only context.Canceled qualifies, NOT context.DeadlineExceeded: a client
	// disconnect cancels r.Context() with Canceled, whereas DeadlineExceeded
	// almost always comes from a server-imposed deadline (a handler's
	// context.WithTimeout around a slow query/upstream call) — a real fault the
	// caller may still be waiting on, so it keeps its error log + 500 rather
	// than being masked as a 499.
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
	WriteErrors(w, http.StatusInternalServerError, ErrorItem{Reason: ReasonInternal, Message: msg})
}

// LocalDetail returns ": " + err.Error() in local mode and "" in multi mode.
// It is the redaction seam for handlers that compose a client-facing message
// around an error whose status is NOT a plain 500 — an upstream 502, a 4xx —
// so InternalError (which always writes a 500) doesn't fit. Append it to a
// static, author-controlled prefix:
//
//	writeJSON(w, http.StatusBadGateway, map[string]string{
//	    "error": "failed to fetch repos from GitHub" + httpx.LocalDetail(err)})
//
// In local mode a developer sees the raw driver/upstream detail; in multi mode
// other tenants' browsers see only the static prefix. The caller keeps
// ownership of its own logging — unlike InternalError, this neither logs nor
// touches the status code. nil err yields "" in both modes.
func LocalDetail(err error) string {
	if err == nil || runmode.Current() == runmode.ModeMulti {
		return ""
	}
	return ": " + err.Error()
}

// IsClientGone reports whether err carries a canceled request context — the
// signature of a caller that disconnected while a handler was mid-flight. The
// cancellation surfaces from whatever query was running when it tripped,
// usually wrapped (fmt.Errorf("...: %w", err)), so this matches via errors.Is
// rather than equality. Callers treat a true result as "the caller is gone,"
// not a server error — see InternalError.
//
// Deliberately scoped to context.Canceled only. context.DeadlineExceeded is
// excluded because it almost always signals a server-imposed timeout (a
// handler's context.WithTimeout on a DB/external call), not a vanished client —
// treating that as client-gone would swallow a genuine server fault the caller
// may still be waiting on. Exported alongside IsUniqueViolation as part of the
// kernel's error vocabulary so other choke points can share the one definition
// instead of re-deriving the check.
func IsClientGone(err error) bool {
	return errors.Is(err, context.Canceled)
}

// NotFound writes a 404 with a "<thing> not found" message. Centralized so the
// wording stays consistent across handlers.
func NotFound(w http.ResponseWriter, thing string) {
	WriteErrors(w, http.StatusNotFound, ErrorItem{Reason: ReasonNotFound, Message: thing + " not found"})
}

// BadRequest writes a 400 with the given message under the generic
// INVALID_BODY reason. Handlers with a more specific fault class (a named
// field, a query param, a path id) call WriteErrors with the matching reason
// instead.
func BadRequest(w http.ResponseWriter, msg string) {
	WriteErrors(w, http.StatusBadRequest, ErrorItem{Reason: ReasonInvalidBody, Message: msg})
}

// WriteUnauth writes a JSON 401. Every protected route emits this via the
// session middleware, so it speaks the same envelope as handler errors —
// a plain-text body here would be the one 401 the shared client parser
// can't read.
func WriteUnauth(w http.ResponseWriter) {
	WriteErrors(w, http.StatusUnauthorized, ErrorItem{Reason: ReasonUnauthenticated, Message: "unauthenticated"})
}

// DefaultMaxBodyBytes is DecodeJSONStrict's request-body cap. 1 MiB clears
// every ordinary JSON body by orders of magnitude; routes that genuinely
// carry more (bundle imports, file uploads) use DecodeJSONStrictLimit.
const DefaultMaxBodyBytes = 1 << 20

// DecodeJSONStrict decodes a single top-level JSON value from the request body
// into v (which must be a pointer). Unknown fields are rejected (400
// UNKNOWN_FIELD naming the field) rather than silently ignored, the body is
// capped at DefaultMaxBodyBytes (413 PAYLOAD_TOO_LARGE), and a type mismatch
// on a known field reports 400 INVALID_FIELD with the field named. Any
// trailing content after the value — a second value, junk bytes, a stray }/] —
// is rejected; only trailing whitespace is allowed. On any failure the error
// is already written; callers return immediately.
//
// It is the only body decoder: the lenient DecodeJSON it replaced ignored
// unknown fields, so a misspelled field silently did nothing and every
// kind-discriminated body silently dropped the other kind's fields.
func DecodeJSONStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	return DecodeJSONStrictLimit(w, r, v, DefaultMaxBodyBytes)
}

// DecodeJSONStrictLimit is DecodeJSONStrict with an explicit body cap, for
// the few routes whose legitimate payloads exceed the default.
func DecodeJSONStrictLimit(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return decodeStrict(w, r.Body, v)
}

// BodyBytes reads the request body under the default cap and hands back the
// raw bytes, writing the error response and returning ok=false on failure.
//
// It exists for a body whose schema depends on a discriminator inside itself:
// the strict decode can only run once the flavor is known, and the flavor can
// only be read by decoding. Reading the bytes first lets a handler take a
// lenient look at the discriminator alone and then decode STRICTLY into that
// flavor's struct — so the other flavor's fields come back as UNKNOWN_FIELD
// rather than being silently accepted and dropped, which is what a
// one-struct-for-both body does.
func BodyBytes(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, DefaultMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeDecodeError(w, err)
		return nil, false
	}
	return body, true
}

// DecodeJSONStrictBytes is DecodeJSONStrict against bytes already read (see
// BodyBytes) — same strictness, same error mapping, so the second pass of a
// discriminated decode answers exactly as a single-pass route would.
func DecodeJSONStrictBytes(w http.ResponseWriter, body []byte, v any) bool {
	return decodeStrict(w, bytes.NewReader(body), v)
}

// decodeStrict is the shared decode: unknown fields rejected, exactly one
// top-level JSON value, every failure already written to w.
func decodeStrict(w http.ResponseWriter, src io.Reader, v any) bool {
	dec := json.NewDecoder(src)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeDecodeError(w, err)
		return false
	}
	// The trailing-content check requires the next decode to hit io.EOF rather
	// than using dec.More(): More() reports false at a top-level } or ], so a
	// body like `{...}}` would otherwise be accepted.
	var rest json.RawMessage
	if err := dec.Decode(&rest); err != io.EOF {
		if maxErr := (*http.MaxBytesError)(nil); errors.As(err, &maxErr) {
			writeDecodeError(w, err)
			return false
		}
		WriteErrors(w, http.StatusBadRequest, ErrorItem{
			Reason: ReasonInvalidBody, Message: "request body must be a single JSON value",
		})
		return false
	}
	return true
}

// writeDecodeError maps a strict-decode failure to its envelope response:
// the body-size cap to 413, an unknown field and a type mismatch to 400 with
// the field named, everything else to the generic 400.
func writeDecodeError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		WriteErrors(w, http.StatusRequestEntityTooLarge, ErrorItem{
			Reason:  ReasonPayloadTooLarge,
			Message: fmt.Sprintf("request body exceeds the %d-byte limit", maxErr.Limit),
		})
		return
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		WriteErrors(w, http.StatusBadRequest, ErrorItem{
			Reason:  ReasonInvalidField,
			Message: fmt.Sprintf("%s must be a %s", typeErr.Field, typeErr.Type),
			Field:   typeErr.Field,
		})
		return
	}
	if field, ok := unknownFieldName(err); ok {
		WriteErrors(w, http.StatusBadRequest, ErrorItem{
			Reason:  ReasonUnknownField,
			Message: "unknown field " + strconv.Quote(field),
			Field:   field,
		})
		return
	}
	WriteErrors(w, http.StatusBadRequest, ErrorItem{
		Reason: ReasonInvalidBody, Message: "invalid request body",
	})
}

// unknownFieldName extracts the field from encoding/json's unknown-field
// error. The error is unexported with no structured accessor, so the message
// text — `json: unknown field "<name>"`, stable across Go releases — is the
// only handle; if a release ever reshapes it, strict decoding still rejects
// the request and only the response's field attribution degrades (to the
// generic INVALID_BODY arm).
func unknownFieldName(err error) (string, bool) {
	rest, ok := strings.CutPrefix(err.Error(), `json: unknown field "`)
	if !ok {
		return "", false
	}
	return strings.TrimSuffix(rest, `"`), true
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
	ctxKeyAuthIdentityID
	ctxKeyTokenAuth
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

// WithAuthIdentityID returns ctx carrying the GoTrue login-identity id for the
// request — the verified JWT sub, which equals public.user_identities.auth_user_id.
// The session middleware sets this just before it remaps the claims Subject to
// the PRINCIPAL, capturing which specific login the caller signed in with.
func WithAuthIdentityID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyAuthIdentityID, id)
}

// AuthIdentityIDFrom returns the login-identity id backing this request, or ""
// when absent (local mode has no GoTrue, and any path that didn't go through
// the session middleware carries nothing). Distinct from ClaimsFrom().Subject,
// which the middleware remaps to the principal: one principal holds N login
// identities, so this is the only signal for "which identity is in use" — e.g.
// to mark the current login in GET /api/me/identities, where two linked
// identities commonly share an email and so can't be told apart by email alone.
func AuthIdentityIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyAuthIdentityID).(string)
	return v
}

// TokenAuth marks a request authenticated by an API token rather than a
// session cookie, and names which token and which org it is sealed to. The auth
// middleware sets it on the Bearer path only, so its presence IS the answer to
// "was this a token call" — a session request carries nothing here.
//
// OrgID duplicates what WithOrgID injected, deliberately: the org in context is
// the request's tenant whichever credential supplied it, while this one is the
// one the token can never leave. The org-scope gates compare a path-named org
// against THIS value, so they are asking about the credential, not about
// wherever the request's cursor happens to point.
type TokenAuth struct {
	TokenID string
	OrgID   string
}

// WithTokenAuth returns ctx carrying the token that authenticated this request.
func WithTokenAuth(ctx context.Context, t *TokenAuth) context.Context {
	return context.WithValue(ctx, ctxKeyTokenAuth, t)
}

// TokenAuthFrom returns the API token backing this request, or nil when a
// session cookie (or local mode's synthetic identity) did. Callers that gate on
// credential type — the org-scope probes, the operator console, the session-only
// verbs — read this rather than sniffing the Authorization header, so the answer
// comes from the middleware that actually resolved the credential.
func TokenAuthFrom(ctx context.Context) *TokenAuth {
	v, _ := ctx.Value(ctxKeyTokenAuth).(*TokenAuth)
	return v
}

// RequireOrg returns the active org ID from request context, or writes a 409
// NO_ACTIVE_ORG and returns ok=false. In local mode the shim guarantees a
// sentinel org so the empty branch never fires.
// Usage: orgID, ok := httpx.RequireOrg(w, r); if !ok { return }
func RequireOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID := OrgIDFrom(r.Context())
	if orgID != "" {
		return orgID, true
	}
	WriteErrors(w, http.StatusConflict, ErrorItem{
		Reason:  ReasonNoActiveOrg,
		Message: "no active org selected; call POST /api/me/active-org to choose one",
	})
	return "", false
}
