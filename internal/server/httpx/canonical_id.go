package httpx

import (
	"fmt"

	"github.com/google/uuid"
)

// CanonicalIDFilterField validates a list body's optional id filter on field.
// Empty means no filter; anything else must be a uuid in canonical form
// (lowercase and hyphenated, without braces or a urn: prefix). noun names
// what the id addresses ("user", "team").
//
// Canonical rather than merely parseable because the stores bind the filter as
// sent and compare it with ids stored in that form (SQLite compares the text),
// so another spelling uuid.Parse accepts would match nothing and answer an
// empty page for a filter the caller believes is right. The rows a list
// serves carry their ids in this form, so a caller filters by the value it
// read back.
func CanonicalIDFilterField(v *Validation, field, value, noun string) {
	if value == "" {
		return
	}
	if parsed, err := uuid.Parse(value); err != nil || parsed.String() != value {
		v.Invalid(field, fmt.Sprintf("must be a %s id in canonical form", noun))
	}
}
