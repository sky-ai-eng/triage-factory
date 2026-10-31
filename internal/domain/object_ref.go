package domain

import (
	"encoding/base64"
	"errors"
)

// ObjectRef is an object id (ObjectID) held as a value. Its text form, and so
// its JSON, is the id's wire spelling: the bytes in unpadded base64url, which
// carry no "/" or "|" and can sit in a URL path segment or query. Any
// response that copies an ObjectRef field encodes it without asking; getting
// the raw id out takes an explicit string conversion. fmt prints the raw id,
// so log lines stay readable.
type ObjectRef string

// objectRefEncoding decodes strictly: a spelling with non-zero trailing bits
// is refused, so an id ParseObjectRef accepts is one MarshalText could have
// emitted.
var objectRefEncoding = base64.RawURLEncoding.Strict()

// MarshalText renders the wire spelling, "" for "".
func (r ObjectRef) MarshalText() ([]byte, error) {
	return []byte(objectRefEncoding.EncodeToString([]byte(r))), nil
}

// UnmarshalText reads the wire spelling back, refusing what ParseObjectRef
// refuses; "" reads as "". It makes the JSON round-trip, so a response decoded
// into a struct holds the raw id again.
func (r *ObjectRef) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*r = ""
		return nil
	}
	ref, ok := ParseObjectRef(string(text))
	if !ok {
		return errors.New("not an object id")
	}
	*r = ref
	return nil
}

// ParseObjectRef decodes a wire spelling. ok=false when wire is not unpadded
// base64url, or decodes to something without an object id's shape
// (IsObjectID).
func ParseObjectRef(wire string) (ObjectRef, bool) {
	raw, err := objectRefEncoding.DecodeString(wire)
	if err != nil || !IsObjectID(string(raw)) {
		return "", false
	}
	return ObjectRef(raw), true
}
