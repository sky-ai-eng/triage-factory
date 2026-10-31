package server

import (
	"encoding/base64"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The wire spelling of an object id (domain.ObjectID) is its bytes in
// unpadded base64url, so an id carries no "/" or "|" and can sit in a URL
// path segment or query. Every HTTP read that emits an object id encodes it
// with wireObjectID, and every route that takes one decodes it with
// parseWireObjectID; below the handlers, ids stay raw.
//
// Decoding is strict: a spelling with non-zero trailing bits is refused, so
// an id the server accepts is one it could have emitted.
var objectIDEncoding = base64.RawURLEncoding.Strict()

// wireObjectID is id's wire spelling, "" for "".
func wireObjectID(id string) string {
	if id == "" {
		return ""
	}
	return objectIDEncoding.EncodeToString([]byte(id))
}

// parseWireObjectID decodes a wire object id to the raw id. ok=false when s
// is not unpadded base64url, or decodes to something without an object id's
// shape (domain.IsObjectID).
func parseWireObjectID(s string) (string, bool) {
	raw, err := objectIDEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	id := string(raw)
	return id, domain.IsObjectID(id)
}
