package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// decodeEnvelope parses a recorded error response into the envelope plus the
// legacy shim key, so every test asserts against what actually went over the
// wire rather than the Go structs that produced it.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (items []ErrorItem, legacy string) {
	t.Helper()
	var body struct {
		Errors []ErrorItem `json:"errors"`
		Error  string      `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body=%q)", err, rec.Body.String())
	}
	return body.Errors, body.Error
}

// TestWriteErrors_Shape pins the envelope contract: an errors list carrying
// reason/message/field, field omitted when unset, and the legacy top-level
// "error" key mirroring the first item's message (the dual-key shim that
// holds until every frontend call site reads the envelope).
func TestWriteErrors_Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrors(rec, http.StatusBadRequest,
		ErrorItem{Reason: ReasonMissingField, Message: "name is required", Field: "name"},
		ErrorItem{Reason: ReasonConflict, Message: "already claimed"},
	)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	items, legacy := decodeEnvelope(t, rec)
	if len(items) != 2 {
		t.Fatalf("len(errors) = %d, want 2", len(items))
	}
	if items[0].Reason != ReasonMissingField || items[0].Message != "name is required" || items[0].Field != "name" {
		t.Errorf("first item = %+v", items[0])
	}
	if items[1].Reason != ReasonConflict || items[1].Field != "" {
		t.Errorf("second item = %+v", items[1])
	}
	if legacy != "name is required" {
		t.Errorf("legacy error key = %q, want the first item's message", legacy)
	}
	// The second item must not serialize a "field" key at all — omitempty is
	// part of the contract ("field appears only for payload-field faults").
	var raw struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw.Errors[1]["field"]; present {
		t.Error(`second item serialized a "field" key; want it omitted`)
	}
}

// TestWriteErrors_NoItems pins the failure-safe arm: a bugged zero-item call
// still produces an error-shaped body, never {"errors":[]} masquerading as
// something a client might read as success.
func TestWriteErrors_NoItems(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrors(rec, http.StatusInternalServerError)
	items, _ := decodeEnvelope(t, rec)
	if len(items) != 1 || items[0].Reason != ReasonInternal {
		t.Errorf("zero-item call produced %+v, want one INTERNAL item", items)
	}
}

// TestHelpers_EmitEnvelope pins that every rewritten helper speaks the
// envelope: reason code, message, status, and the shim key.
func TestHelpers_EmitEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		write      func(w http.ResponseWriter)
		wantStatus int
		wantReason string
		wantMsg    string
	}{
		{"NotFound", func(w http.ResponseWriter) { NotFound(w, "task") }, 404, ReasonNotFound, "task not found"},
		{"BadRequest", func(w http.ResponseWriter) { BadRequest(w, "nope") }, 400, ReasonInvalidBody, "nope"},
		{"WriteUnauth", func(w http.ResponseWriter) { WriteUnauth(w) }, 401, ReasonUnauthenticated, "unauthenticated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.write(rec)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			items, legacy := decodeEnvelope(t, rec)
			if len(items) != 1 || items[0].Reason != tc.wantReason || items[0].Message != tc.wantMsg {
				t.Errorf("items = %+v, want one {%s, %q}", items, tc.wantReason, tc.wantMsg)
			}
			if legacy != tc.wantMsg {
				t.Errorf("legacy error key = %q, want %q", legacy, tc.wantMsg)
			}
		})
	}
}

// TestRequireOrg_Envelope pins the org gate's 409: NO_ACTIVE_ORG through the
// standard envelope. The old body carried the code under the legacy "error"
// key; nothing client-side keyed on that string, so the shim's
// first-message semantics apply here like everywhere else.
func TestRequireOrg_Envelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	if _, ok := RequireOrg(rec, req); ok {
		t.Fatal("RequireOrg with no org in context reported ok")
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	items, _ := decodeEnvelope(t, rec)
	if len(items) != 1 || items[0].Reason != ReasonNoActiveOrg {
		t.Errorf("items = %+v, want one NO_ACTIVE_ORG", items)
	}
}

// TestInternalError_Envelope covers the two contracts InternalError must keep
// while adopting the envelope: local mode exposes the raw error, multi mode
// redacts it to a generic message (raw driver strings must not reach other
// tenants' browsers). The 499 client-gone arm is pinned by
// TestInternalError_ClientGone and unaffected — it writes no body at all.
func TestInternalError_Envelope(t *testing.T) {
	raw := errors.New("dial tcp 10.0.0.5:5432: connection refused")
	cases := []struct {
		name    string
		mode    runmode.Mode
		wantMsg string
	}{
		{"local exposes detail", runmode.ModeLocal, raw.Error()},
		{"multi redacts", runmode.ModeMulti, "internal server error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			restore := logging.SetOutput(&strings.Builder{})
			defer restore()

			rec := httptest.NewRecorder()
			InternalError(rec, "test", raw)
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}
			items, legacy := decodeEnvelope(t, rec)
			if len(items) != 1 || items[0].Reason != ReasonInternal || items[0].Message != tc.wantMsg {
				t.Errorf("items = %+v, want one {INTERNAL, %q}", items, tc.wantMsg)
			}
			if legacy != tc.wantMsg {
				t.Errorf("legacy error key = %q, want %q", legacy, tc.wantMsg)
			}
		})
	}
}

// TestValidation_AccumulatesAllFaults pins the accumulator contract: every
// failing field lands in one response, in recording order, and a fault-free
// Validation writes nothing.
func TestValidation_AccumulatesAllFaults(t *testing.T) {
	var v Validation
	v.Missing("entity_id")
	v.Invalid("action", "must be one of: queue, claim, done")
	v.OutOfRange("limit", "must be at most 500")
	v.Param("team_id", "must be a UUID")

	rec := httptest.NewRecorder()
	if !v.Flush(rec, http.StatusBadRequest) {
		t.Fatal("Flush with faults reported nothing written")
	}
	items, legacy := decodeEnvelope(t, rec)
	wantReasons := []string{ReasonMissingField, ReasonInvalidField, ReasonOutOfRange, ReasonInvalidParam}
	wantFields := []string{"entity_id", "action", "limit", "team_id"}
	if len(items) != len(wantReasons) {
		t.Fatalf("len(items) = %d, want %d", len(items), len(wantReasons))
	}
	for i := range items {
		if items[i].Reason != wantReasons[i] || items[i].Field != wantFields[i] {
			t.Errorf("item %d = %+v, want {%s, field %s}", i, items[i], wantReasons[i], wantFields[i])
		}
	}
	if legacy != "entity_id is required" {
		t.Errorf("legacy error key = %q, want the first fault's message", legacy)
	}

	var clean Validation
	rec = httptest.NewRecorder()
	if clean.Flush(rec, http.StatusBadRequest) {
		t.Error("Flush with no faults claimed to write")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("fault-free Flush wrote a body: %q", rec.Body.String())
	}
}

// TestDecodeJSONStrict pins the strictness DecodeJSON lacks: unknown fields
// 400 with UNKNOWN_FIELD naming the field, type mismatches 400 with
// INVALID_FIELD, the single-value contract carries over, and a clean body
// still decodes.
func TestDecodeJSONStrict(t *testing.T) {
	type payload struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	cases := []struct {
		name       string
		body       string
		wantAccept bool
		wantReason string
		wantField  string
	}{
		{"clean", `{"a":1,"b":"x"}`, true, "", ""},
		{"unknown field", `{"a":1,"typo_field":true}`, false, ReasonUnknownField, "typo_field"},
		{"type mismatch", `{"a":"not a number"}`, false, ReasonInvalidField, "a"},
		{"trailing junk", `{"a":1}{"a":2}`, false, ReasonInvalidBody, ""},
		{"malformed", `{"a":`, false, ReasonInvalidBody, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			var v payload
			got := DecodeJSONStrict(rec, req, &v)
			if got != tc.wantAccept {
				t.Fatalf("accepted=%v, want %v (body=%q)", got, tc.wantAccept, rec.Body.String())
			}
			if tc.wantAccept {
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			items, _ := decodeEnvelope(t, rec)
			if len(items) != 1 || items[0].Reason != tc.wantReason || items[0].Field != tc.wantField {
				t.Errorf("items = %+v, want one {%s, field %q}", items, tc.wantReason, tc.wantField)
			}
		})
	}
}

// TestDecodeJSONStrict_BodyCap pins the size limit: an over-cap body answers
// 413 PAYLOAD_TOO_LARGE, and the cap is per-call configurable via
// DecodeJSONStrictLimit.
func TestDecodeJSONStrict_BodyCap(t *testing.T) {
	type payload struct {
		A string `json:"a"`
	}
	big := `{"a":"` + strings.Repeat("x", 128) + `"}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(big))
	var v payload
	if DecodeJSONStrictLimit(rec, req, &v, 64) {
		t.Fatal("over-cap body decoded")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	items, _ := decodeEnvelope(t, rec)
	if len(items) != 1 || items[0].Reason != ReasonPayloadTooLarge {
		t.Errorf("items = %+v, want one PAYLOAD_TOO_LARGE", items)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/", strings.NewReader(big))
	if !DecodeJSONStrictLimit(rec, req, &v, 4096) {
		t.Errorf("under-cap body rejected: %q", rec.Body.String())
	}
}
