package httpx

import "net/http"

// Reason codes for ErrorItem.Reason — the stable machine vocabulary clients
// switch on. Append-only: a shipped code is a wire contract, so it is never
// renamed or removed, and handlers never inline one as a string literal (the
// ratchet test in package server enforces that). Prefer a generic code plus
// Field over minting a per-route code; add here only when no existing code
// names the fault class.
const (
	// 400-class: the request is malformed at the protocol/shape level.
	ReasonInvalidBody  = "INVALID_BODY"  // unparseable/trailing body, or no more-specific code applies
	ReasonUnknownField = "UNKNOWN_FIELD" // strict decoding met a field the schema doesn't have
	ReasonMissingField = "MISSING_FIELD" // a required body field is absent
	ReasonInvalidField = "INVALID_FIELD" // a body field is present but unusable (wrong type, bad enum, bad grammar)
	ReasonInvalidParam = "INVALID_PARAM" // a query parameter fails strict parsing
	ReasonInvalidID    = "INVALID_ID"    // a path id fails its format (non-uuid, non-positive number)

	// AuthN/AuthZ.
	ReasonUnauthenticated = "UNAUTHENTICATED"
	ReasonForbidden       = "FORBIDDEN"
	ReasonTeamArchived    = "TEAM_ARCHIVED"
	ReasonNotFound        = "NOT_FOUND" // nonexistent, or not visible per the disclosure rule
	ReasonModeUnsupported = "MODE_UNSUPPORTED"

	// 409-class: the request is fine, the resource's current state refuses it.
	ReasonConflict        = "CONFLICT"
	ReasonDuplicateName   = "DUPLICATE_NAME"
	ReasonAlreadyTerminal = "ALREADY_TERMINAL"
	ReasonNoActiveOrg     = "NO_ACTIVE_ORG"
	ReasonNotConfigured   = "NOT_CONFIGURED" // a required integration isn't set up
	ReasonVersionConflict = "VERSION_CONFLICT"

	// 422-class: well-formed but semantically invalid for this data.
	ReasonOutOfRange   = "OUT_OF_RANGE"
	ReasonCrossTeamRef = "CROSS_TEAM_REF"

	// Partial success reported as an error: the delegation's claim stamped
	// but the agent run could not be spawned (bad blueprint reference at
	// 422, spawn/DB fault at 500). Clients key the claim-survived retry
	// affordance on this reason, never on a bare status — a 500 without it
	// means the claim may not have landed at all.
	ReasonSpawnFailed = "SPAWN_FAILED"

	// Upstream and transport.
	ReasonUpstreamRejected    = "UPSTREAM_REJECTED"
	ReasonUpstreamUnavailable = "UPSTREAM_UNAVAILABLE"
	ReasonRateLimited         = "RATE_LIMITED"
	ReasonPayloadTooLarge     = "PAYLOAD_TOO_LARGE"
	ReasonInternal            = "INTERNAL"
)

// ErrorItem is one fault in an error response. Reason is a stable machine
// code from the registry above; Message is human prose; Field names the
// payload field at fault and is set only when one specific field is.
type ErrorItem struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// errorEnvelope is the wire shape WriteErrors emits. Errors is the contract;
// Error duplicates the first item's message under the pre-envelope top-level
// key because dozens of frontend call sites and tests still read `.error`
// today — dual emission lets every shared helper flip at once without
// sweeping every consumer in the same change. The legacy key is removed at
// the end of the mechanical handler sweep, once the last frontend call site
// reads the envelope through the shared parser.
type errorEnvelope struct {
	Errors []ErrorItem `json:"errors"`
	Error  string      `json:"error"`
}

// WriteErrors writes the error envelope: {"errors": [...]} plus the legacy
// dual-key shim (see errorEnvelope). Always a list — a handler validating a
// multi-field body accumulates every failing field (see Validation) and
// flushes them in one response. The HTTP status is blanket for the response.
func WriteErrors(w http.ResponseWriter, status int, items ...ErrorItem) {
	if len(items) == 0 {
		// A caller reporting an error with no items is a bug, but answering
		// with an empty list would hand clients a success-shaped failure.
		items = []ErrorItem{{Reason: ReasonInternal, Message: "unspecified error"}}
	}
	WriteJSON(w, status, errorEnvelope{Errors: items, Error: items[0].Message})
}

// Validation accumulates field faults so a multi-field body reports every
// failure at once instead of the first one hit. Validate all fields, then:
//
//	var v httpx.Validation
//	if req.Name == "" {
//	    v.Missing("name")
//	}
//	if !validPosture(req.Posture) {
//	    v.Invalid("posture", "must be one of: strict, relaxed")
//	}
//	if v.Flush(w, http.StatusBadRequest) {
//	    return
//	}
type Validation struct {
	items []ErrorItem
}

// Missing records a required body field that is absent.
func (v *Validation) Missing(field string) {
	v.items = append(v.items, ErrorItem{Reason: ReasonMissingField, Message: field + " is required", Field: field})
}

// Invalid records a body field that is present but unusable.
func (v *Validation) Invalid(field, msg string) {
	v.items = append(v.items, ErrorItem{Reason: ReasonInvalidField, Message: msg, Field: field})
}

// OutOfRange records a field whose value is outside its permitted range.
func (v *Validation) OutOfRange(field, msg string) {
	v.items = append(v.items, ErrorItem{Reason: ReasonOutOfRange, Message: msg, Field: field})
}

// Param records a query parameter that fails strict parsing.
func (v *Validation) Param(name, msg string) {
	v.items = append(v.items, ErrorItem{Reason: ReasonInvalidParam, Message: msg, Field: name})
}

// Add records a fully-specified item, for faults the named helpers don't cover.
func (v *Validation) Add(item ErrorItem) {
	v.items = append(v.items, item)
}

// Flush writes every accumulated item with the given status and reports
// whether it wrote anything; the caller returns on true. A Validation with no
// recorded faults writes nothing.
func (v *Validation) Flush(w http.ResponseWriter, status int) bool {
	if len(v.items) == 0 {
		return false
	}
	WriteErrors(w, status, v.items...)
	return true
}
