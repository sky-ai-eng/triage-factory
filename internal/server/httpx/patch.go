package httpx

import (
	"encoding/json"
	"fmt"
)

// The PATCH field vocabulary. A partial update has to distinguish three things
// a client can say about one field — leave it alone, clear it, set it to this —
// and a typed pointer can only express two: both `absent` and `null` decode to
// nil. So a PATCH body declares its fields as json.RawMessage and reads them
// through the helpers here, which is the convention PATCH /api/repos/{id}
// established and every settings PATCH now follows.
//
// The helpers all take the request's Validation, so a body with several bad
// fields reports all of them at once instead of the first one hit.

// PatchState is what a PATCH body said about one field.
type PatchState int

const (
	// PatchAbsent — the field wasn't in the body. Keep the stored value.
	PatchAbsent PatchState = iota
	// PatchClear — the field was explicitly null. Clear it, or reset it to
	// its default; which of the two is the resource's business.
	PatchClear
	// PatchSet — the field carried a value, returned alongside this state.
	PatchSet
	// patchInvalid — the field was present but the wrong JSON type. The
	// fault is already recorded on the caller's Validation; the state is
	// unexported because callers must not branch on it (their switch handles
	// Set and Clear, and the Flush ahead of the write stops the request).
	patchInvalid
)

// PatchString reads a `string | null` PATCH field.
func PatchString(v *Validation, raw json.RawMessage, field string) (string, PatchState) {
	return patchValue[string](v, raw, field, "a string")
}

// PatchBool reads a `bool | null` PATCH field.
func PatchBool(v *Validation, raw json.RawMessage, field string) (bool, PatchState) {
	return patchValue[bool](v, raw, field, "true or false")
}

// PatchInt reads an `integer | null` PATCH field. A fractional number is a
// type fault, not a silent truncation.
func PatchInt(v *Validation, raw json.RawMessage, field string) (int, PatchState) {
	return patchValue[int](v, raw, field, "a whole number")
}

// PatchFloat reads a `number | null` PATCH field.
func PatchFloat(v *Validation, raw json.RawMessage, field string) (float64, PatchState) {
	return patchValue[float64](v, raw, field, "a number")
}

// PatchStrings reads a `[]string | null` PATCH field. The list IS the value —
// a set replaces wholesale rather than merging — so PatchSet hands back exactly
// what the body named, empty array included; whether an empty one is legal is
// the field's own question, not this reader's.
func PatchStrings(v *Validation, raw json.RawMessage, field string) ([]string, PatchState) {
	return patchValue[[]string](v, raw, field, "an array of strings")
}

// patchValue is the shared body of the readers above: absent stays absent, the
// literal null clears, and anything else must decode as T or it's a 400 naming
// the field.
func patchValue[T any](v *Validation, raw json.RawMessage, field, want string) (T, PatchState) {
	var zero T
	if raw == nil {
		return zero, PatchAbsent
	}
	if string(raw) == "null" {
		return zero, PatchClear
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		v.Invalid(field, fmt.Sprintf("%s must be %s or null", field, want))
		return zero, patchInvalid
	}
	return out, PatchSet
}

// PatchNamed returns whether any of the given fields was named in the body at
// all. A PATCH that names nothing wrote nothing, and answering "saved" for it
// is the one response a client cannot tell from a real write.
func PatchNamed(raws ...json.RawMessage) bool {
	for _, raw := range raws {
		if raw != nil {
			return true
		}
	}
	return false
}
