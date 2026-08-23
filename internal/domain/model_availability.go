package domain

import "time"

// Model availability states — what one probe of one model against one org's
// credentials concluded, and the whole stored vocabulary.
//
// A third outcome exists and is deliberately NOT here: a probe that never got
// an answer (a 5xx, a timeout, a dial failure) writes no row at all. It is
// neither green nor red, because one bad provider minute must not be able to
// hide a model that works. Absence therefore means "never probed, or every
// attempt so far was inconclusive" — one value for two histories, which is
// correct: nothing downstream treats them differently.
const (
	// ModelAvailabilityGreen means a probe invoked this model successfully at
	// least once. Terminal for automatic transitions: only a manual re-test
	// writes the row again, and a re-test that is refused moves it to red.
	// Nothing re-probes on a timer — a model that later loses access surfaces
	// at dispatch, loudly, rather than being polled for.
	ModelAvailabilityGreen = "green"
	// ModelAvailabilityRed means the provider REFUSED the call: it answered,
	// and its answer was about this credential's right to invoke this model
	// (401/403, AccessDeniedException, model-not-found). Cleared only by a
	// later successful test.
	ModelAvailabilityRed = "red"
)

// ModelAvailability is one `model_availability` row: whether one org's
// credentials could actually invoke one model, established by spending a
// minimal real request on it.
//
// It exists because no control-plane API answers the question uniformly.
// Anthropic's /v1/models reflects the key's access; Bedrock's
// ListFoundationModels reports what the REGION carries, not what the account
// was granted, and returns base model ids rather than the inference-profile
// ids that are the only invocable spelling for current Claude models. A probe
// needs no permission the run does not already need — if the probe fails, the
// run would have failed too — and it tests the exact id that will be invoked.
//
// Provider is part of the identity rather than derived from ModelKey: what was
// established is that a CREDENTIAL PATH works, the eager sweep addresses rows
// by provider, and a row must still say which credential it was tested against
// after the build stops offering the key.
type ModelAvailability struct {
	OrgID    string `json:"org_id"`
	Provider string `json:"provider"`
	// ModelKey is the catalog key verbatim — the exact string sent on the wire,
	// never parsed. Carries no foreign key: the catalog is a compiled-in file,
	// not a table, so a row for a key this build no longer offers is inert
	// (nothing iterates to it) rather than invalid.
	ModelKey string `json:"model_key"`
	// State is ModelAvailabilityGreen or ModelAvailabilityRed. There is no
	// stored value for an inconclusive probe; that is what an absent row is.
	State string `json:"state"`
	// CheckedAt is when the probe that produced this state ran, stamped by the
	// write rather than supplied — a caller cannot claim a check it did not do.
	CheckedAt time.Time `json:"checked_at"`
	// Detail is the provider's own refusal, carried so an admin reads why
	// rather than just that. Empty on green: a working model has nothing to
	// explain, and leaving the last refusal on it would be a stale claim.
	Detail string `json:"detail,omitempty"`
}
