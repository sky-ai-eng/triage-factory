package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDecodeJSONStrict_RejectsTrailingData pins the contract that DecodeJSON
// accepts exactly one top-level JSON value (optionally followed by
// whitespace) and rejects anything trailing it with a 400. The trailing
// `}` / `]` cases are the ones dec.More() alone misses, since More()
// reports false at the close of the current array/object.
func TestDecodeJSONStrict_RejectsTrailingData(t *testing.T) {
	type payload struct {
		A int `json:"a"`
	}
	cases := []struct {
		name       string
		body       string
		wantAccept bool
	}{
		{"clean object", `{"a":1}`, true},
		{"trailing whitespace", "{\"a\":1}  \n ", true},
		{"second object", `{"a":1} {"extra":true}`, false},
		{"trailing close brace", `{"a":1}}`, false},
		{"trailing close bracket", `{"a":1}]`, false},
		{"trailing number", `{"a":1}9`, false},
		{"trailing letters", `{"a":1}garbage`, false},
		{"malformed", `{"a":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			var v payload
			got := DecodeJSONStrict(rec, req, &v)
			if got != tc.wantAccept {
				t.Errorf("DecodeJSONStrict(%q) accepted=%v, want %v (status=%d)", tc.body, got, tc.wantAccept, rec.Code)
			}
			if !tc.wantAccept && rec.Code != 400 {
				t.Errorf("DecodeJSONStrict(%q) rejected but status=%d, want 400", tc.body, rec.Code)
			}
		})
	}
}

// TestIsClientGone pins which errors read as "the caller disconnected": a
// canceled context, including when wrapped via %w (the shape db.WithTx + the
// handlers produce). DeadlineExceeded is deliberately NOT client-gone — it
// almost always signals a server-imposed timeout, which must keep its 500 +
// error log rather than be masked as a 499. A real error and nil aren't
// client-gone either.
func TestIsClientGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"canceled", context.Canceled, true},
		{"wrapped canceled", fmt.Errorf("set request.jwt.claims: %w", context.Canceled), true},
		{"double wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", context.Canceled)), true},
		{"deadline excluded", context.DeadlineExceeded, false},
		{"wrapped deadline excluded", fmt.Errorf("team-in-org check: %w", context.DeadlineExceeded), false},
		{"real error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsClientGone(tc.err); got != tc.want {
				t.Errorf("IsClientGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestInternalError_ClientGone is the regression for TFAC-398: a request the
// client aborted mid-flight must NOT log at error level or return a 500 — it
// records a 499 instead, so client-abort noise stays out of 5xx logs and
// metrics. A genuine error still logs and 500s.
func TestInternalError_ClientGone(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantErrLog bool
	}{
		{"canceled", context.Canceled, StatusClientClosedRequest, false},
		{"wrapped canceled", fmt.Errorf("set request.jwt.claims: %w", context.Canceled), StatusClientClosedRequest, false},
		// A server-imposed deadline is NOT client-gone — it stays a logged 500.
		{"deadline still 500", context.DeadlineExceeded, http.StatusInternalServerError, true},
		{"real error", errors.New("boom"), http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			restore := logging.SetOutput(&logs)
			defer restore()

			rec := httptest.NewRecorder()
			InternalError(rec, "test", tc.err)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			// The 499 path is a pure no-op: WriteHeader and return, no body.
			// Pin that so an accidental body write can't creep in.
			if tc.wantStatus == StatusClientClosedRequest && rec.Body.Len() != 0 {
				t.Errorf("499 response should have no body, got %q", rec.Body.String())
			}
			// "handler error" is the error-level line; a client-gone abort
			// must never emit it (the debug breadcrumb is below the default
			// level and so absent from the buffer too).
			if gotErrLog := strings.Contains(logs.String(), "handler error"); gotErrLog != tc.wantErrLog {
				t.Errorf("error-level log present = %v, want %v (logs=%q)", gotErrLog, tc.wantErrLog, logs.String())
			}
		})
	}
}

// TestLocalDetail is the TFAC-409 item-2 contract: the raw error detail is
// appended in local mode (developer convenience) but stripped in multi mode so
// driver/internal/upstream strings don't leak to other tenants' browsers.
func TestLocalDetail(t *testing.T) {
	err := errors.New("dial tcp 10.0.0.5:5432: connection refused")

	cases := []struct {
		name string
		mode runmode.Mode
		err  error
		want string
	}{
		{"local appends detail", runmode.ModeLocal, err, ": dial tcp 10.0.0.5:5432: connection refused"},
		{"multi strips detail", runmode.ModeMulti, err, ""},
		{"nil err local", runmode.ModeLocal, nil, ""},
		{"nil err multi", runmode.ModeMulti, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			if got := LocalDetail(tc.err); got != tc.want {
				t.Errorf("LocalDetail(%v) in %s = %q, want %q", tc.err, tc.mode, got, tc.want)
			}
		})
	}
}
