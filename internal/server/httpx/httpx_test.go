package httpx

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

// textRef decodes from a JSON string through UnmarshalText, though it is a
// struct, as domain.ObjectRef does.
type textRef struct{ s string }

func (r *textRef) UnmarshalText(b []byte) error {
	r.s = string(b)
	return nil
}

// TestDecodeJSONStrict_TypeFaultNamesTheJSONField pins that a wrong-typed
// field is reported by its JSON name alone, however the body struct reaches
// it, and that the message names the JSON type the field takes rather than a
// Go type. encoding/json puts the Go name of each embedded struct on the
// path, so without the rewrite PageRequest's fields come back as
// "PageRequest.page_size".
func TestDecodeJSONStrict_TypeFaultNamesTheJSONField(t *testing.T) {
	// Exported, since encoding/json cannot allocate an embedded pointer to
	// an unexported struct.
	type Deep struct {
		B int `json:"b"`
	}
	type inner struct {
		*Deep
	}
	type lane struct {
		Statuses []string `json:"statuses"`
		Inner    inner    `json:"inner"`
	}
	type Filter struct {
		X int `json:"x"`
	}
	type tagged struct {
		Ys []int `json:"ys"`
	}
	type payload struct {
		lane
		PageRequest
		Filter
		tagged `json:"tagged"`

		Items  []inner           `json:"items"`
		ByKey  map[string]Deep   `json:"by_key"`
		Labels map[string]string `json:"labels"`
		ByID   map[int]string    `json:"by_id"`
		Ns     []int             `json:"ns"`
		NN     [][]int           `json:"nn"`
		Small  int8              `json:"small"`
		U      uint              `json:"u"`
		Low    float32           `json:"low"`
		Bytes  []byte            `json:"bytes"`
		On     *bool             `json:"on"`
		Ratio  float64           `json:"ratio"`
		Ref    textRef           `json:"ref"`
		A      any               `json:"a"`
		M      map[string]any    `json:"m"`
		L      []any             `json:"l"`
		// Spelled like the embedded Filter's Go name, and a JSON key all the same.
		Collides int `json:"Filter"`
		Count    int `json:"Count"`
	}
	cases := []struct {
		body        string
		wantField   string
		wantMessage string
	}{
		{`{"page_size": "x"}`, "page_size", "page_size must be a whole number"},
		// An integer refuses a number for its range, or for being written
		// with a decimal point or exponent even when it is whole.
		{`{"page_size": 99999999999999999999}`, "page_size", "page_size must be a whole number from -9223372036854775808 to 9223372036854775807"},
		{`{"page_size": 1e3}`, "page_size", "page_size must be a whole number written without a decimal point or exponent"},
		{`{"page_size": 1.0}`, "page_size", "page_size must be a whole number written without a decimal point or exponent"},
		{`{"page_size": 1.5}`, "page_size", "page_size must be a whole number written without a decimal point or exponent"},
		{`{"small": 200}`, "small", "small must be a whole number from -128 to 127"},
		{`{"u": -1}`, "u", "u must be a non-negative whole number written without a minus sign"},
		{`{"u": -0}`, "u", "u must be a non-negative whole number written without a minus sign"},
		{`{"u": 18446744073709551616}`, "u", "u must be a non-negative whole number no greater than 18446744073709551615"},
		{`{"u": 1e3}`, "u", "u must be a non-negative whole number written without a decimal point or exponent"},
		{`{"ns": [1, 1e3]}`, "ns", "ns must be an array of whole numbers written without a decimal point or exponent"},
		{`{"nn": [[1.5]]}`, "nn", "nn must be an array of arrays of whole numbers written without a decimal point or exponent"},
		// A key that is not a JSON number gets no rule, whitespace included.
		{`{"by_id": {"1 ": "x"}}`, "by_id", "by_id must be an object of strings whose keys are whole numbers"},
		{`{"by_id": {"99999999999999999999": "x"}}`, "by_id", "by_id must be an object of strings whose keys are whole numbers from -9223372036854775808 to 9223372036854775807"},
		// A float refuses a number only for its range.
		{`{"ratio": 1e400}`, "ratio", "ratio must be a number from -1.7976931348623157e+308 to 1.7976931348623157e+308"},
		{`{"low": 1e39}`, "low", "low must be a number from -3.4028235e+38 to 3.4028235e+38"},
		// A free-form field reads a number as a float64, so it refuses one
		// beyond float64's range, however deep in the value the number is.
		{`{"a": 1e400}`, "a", "a must be any JSON value with every number from -1.7976931348623157e+308 to 1.7976931348623157e+308"},
		{`{"a": {"x": [1, -1e400]}}`, "a", "a must be any JSON value with every number from -1.7976931348623157e+308 to 1.7976931348623157e+308"},
		{`{"m": {"x": 1e400}}`, "m", "m must be an object of JSON values with every number from -1.7976931348623157e+308 to 1.7976931348623157e+308"},
		{`{"l": [1, 1e400]}`, "l", "l must be an array of JSON values with every number from -1.7976931348623157e+308 to 1.7976931348623157e+308"},
		{`{"page_token": 7}`, "page_token", "page_token must be a string"},
		{`{"statuses": "x"}`, "statuses", "statuses must be an array of strings"},
		// A bad element reports the list's type, not the element's.
		{`{"statuses": ["a", 1]}`, "statuses", "statuses must be an array of strings"},
		{`{"x": "x"}`, "x", "x must be a whole number"},
		{`{"inner": "x"}`, "inner", "inner must be an object"},
		{`{"inner": {"b": "x"}}`, "inner.b", "inner.b must be a whole number"},
		{`{"items": [{"b": "x"}]}`, "items.b", "items.b must be a whole number"},
		{`{"by_key": {"k": {"b": "x"}}}`, "by_key.b", "by_key.b must be a whole number"},
		{`{"labels": {"k": 1}}`, "labels", "labels must be an object of strings"},
		{`{"by_id": {"abc": "x"}}`, "by_id", "by_id must be an object of strings whose keys are whole numbers"},
		{`{"bytes": 7}`, "bytes", "bytes must be a base64 string"},
		// An unexported anonymous struct with a tag name is an ordinary field,
		// and the bad element reports the list it is in.
		{`{"tagged": {"ys": [1, "x"]}}`, "tagged.ys", "tagged.ys must be an array of whole numbers"},
		{`{"on": "yes"}`, "on", "on must be true or false"},
		{`{"ratio": "x"}`, "ratio", "ratio must be a number"},
		{`{"ref": 7}`, "ref", "ref must be a string"},
		{`{"Filter": "x"}`, "Filter", "Filter must be a whole number"},
		{`{"Count": "x"}`, "Count", "Count must be a whole number"},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			var v payload
			if DecodeJSONStrict(rec, req, &v) {
				t.Fatalf("DecodeJSONStrict(%s) accepted a wrong-typed field", tc.body)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			var env struct {
				Errors []ErrorItem `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
			}
			want := ErrorItem{Reason: ReasonInvalidField, Message: tc.wantMessage, Field: tc.wantField}
			if len(env.Errors) != 1 || env.Errors[0] != want {
				t.Errorf("errors = %+v, want [%+v]", env.Errors, want)
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
		{"tx done attributed to a canceled ctx", fmt.Errorf("%w (%w)", sql.ErrTxDone, context.Canceled), true},
		{"tx done alone", sql.ErrTxDone, false},
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
