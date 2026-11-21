package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// The PATCH field vocabulary. A partial update has to distinguish three things
// a client can say about one field — leave it alone, clear it, set it to this —
// and a typed pointer can only express two: both `absent` and `null` decode to
// nil. So a PATCH body keeps each field's raw bytes, as a Patch[T], which also
// names the type the field takes for the API spec, or as a json.RawMessage
// read through the helpers here.
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
	return patchValue[string](v, raw, field, true)
}

// PatchBool reads a `bool | null` PATCH field.
func PatchBool(v *Validation, raw json.RawMessage, field string) (bool, PatchState) {
	return patchValue[bool](v, raw, field, true)
}

// PatchInt reads an `integer | null` PATCH field. A fractional number is a
// type fault, not a silent truncation.
func PatchInt(v *Validation, raw json.RawMessage, field string) (int, PatchState) {
	return patchValue[int](v, raw, field, true)
}

// PatchFloat reads a `number | null` PATCH field.
func PatchFloat(v *Validation, raw json.RawMessage, field string) (float64, PatchState) {
	return patchValue[float64](v, raw, field, true)
}

// PatchStrings reads a `[]string | null` PATCH field. The list IS the value —
// a set replaces wholesale rather than merging — so PatchSet hands back exactly
// what the body named, empty array included; whether an empty one is legal is
// the field's own question, not this reader's.
func PatchStrings(v *Validation, raw json.RawMessage, field string) ([]string, PatchState) {
	return patchValue[[]string](v, raw, field, true)
}

// Patch is a PATCH body field of type T. It keeps the field's raw bytes the
// way a json.RawMessage field does, so absent and null stay apart, and names
// the type the field takes, which a RawMessage cannot tell the API spec.
type Patch[T any] struct{ raw json.RawMessage }

// UnmarshalJSON keeps the bytes and never fails; absent fields never reach it.
// A type fault in the value is reported by Read, which names the field.
func (p *Patch[T]) UnmarshalJSON(b []byte) error {
	p.raw = append(json.RawMessage(nil), b...)
	return nil
}

// Read reports what the body said about the field, as PatchString and the
// other readers do. A struct T refuses a key it does not declare, as the body
// decode does.
func (p Patch[T]) Read(v *Validation, field string) (T, PatchState) {
	return patchValue[T](v, p.raw, field, true)
}

// ReadSet reads a field a PATCH may set but never clear, so the state it
// returns is PatchAbsent or PatchSet. A null is a fault on the field, "<field>
// cannot be cleared: <why>", recorded on v with the type faults, and a type
// fault's message does not offer null.
func (p Patch[T]) ReadSet(v *Validation, field, why string) (T, PatchState) {
	if string(p.raw) == "null" {
		var zero T
		v.Invalid(field, field+" cannot be cleared: "+why)
		return zero, patchInvalid
	}
	return patchValue[T](v, p.raw, field, false)
}

// patchValue is the shared body of the readers above: absent stays absent, the
// literal null clears, and anything else must decode as T or it's a 400 naming
// the field, in the words the strict body decode uses for the same fault.
// nullable says whether that message offers null as well.
func patchValue[T any](v *Validation, raw json.RawMessage, field string, nullable bool) (T, PatchState) {
	var zero T
	if raw == nil {
		return zero, PatchAbsent
	}
	if string(raw) == "null" {
		return zero, PatchClear
	}
	var out T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		if name, ok := unknownFieldName(err); ok {
			v.Invalid(field, fmt.Sprintf("%s has no field %q", field, name))
			return zero, patchInvalid
		}
		var typeErr *json.UnmarshalTypeError
		errors.As(err, &typeErr)
		// A fault inside a struct or map T is reported on the member at
		// fault, as the body decode reports one, rather than on the whole
		// value.
		if typeErr != nil {
			if sub, subType := jsonFieldPath(reflect.TypeFor[T](), typeErr.Field); sub != "" {
				if subType == nil {
					subType = typeErr.Type
				}
				path := field + "." + sub
				message := path + " has the wrong type"
				if want, _ := jsonType(subType, typeErr); want != "" {
					message = path + " must be " + want
				}
				v.Invalid(path, message)
				return zero, patchInvalid
			}
		}
		want, _ := jsonType(reflect.TypeFor[T](), typeErr)
		message := fmt.Sprintf("%s must be %s", field, want)
		if nullable {
			message += ", or null"
		}
		v.Invalid(field, message)
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
