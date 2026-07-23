package curator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EncodeStringSliceBaseline serializes a pinned_repos snapshot for
// storage in a pending 'injection:context' message's metadata
// baseline_value. Caller is the projects PATCH handler; the curator
// runtime reads it back via decodeStringSlice. Always returns a JSON array string ("[]" for
// nil/empty), never an empty string — the column is NOT NULL.
func EncodeStringSliceBaseline(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode baseline: %w", err)
	}
	return string(b), nil
}

// EncodeNullableStringBaseline serializes a tracker-key snapshot. An
// unset tracker is stored as JSON null so the round-trip distinguishes
// it from the empty string (which never occurs after handler-level
// trim, but the type stays honest).
func EncodeNullableStringBaseline(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "null", nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode baseline: %w", err)
	}
	return string(b), nil
}

func decodeStringSlice(raw string) ([]string, error) {
	out := []string{}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeNullableString(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return "", nil
	}
	var out string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", err
	}
	return out, nil
}

// stringSliceDiff returns the symmetric set difference of two slices
// as (added, removed). Order in the output mirrors `current` for
// `added` and `baseline` for `removed` so the rendered diff reads
// stable across calls. Duplicates within a slice are deduped on the
// way in — the API layer normalizes pinned_repos already, but this
// keeps the renderer robust if that ever loosens.
func stringSliceDiff(baseline, current []string) (added, removed []string) {
	bset := map[string]struct{}{}
	for _, v := range baseline {
		bset[v] = struct{}{}
	}
	cset := map[string]struct{}{}
	for _, v := range current {
		cset[v] = struct{}{}
	}
	for _, v := range current {
		if _, ok := bset[v]; ok {
			continue
		}
		if _, seen := indexOf(added, v); seen {
			continue
		}
		added = append(added, v)
	}
	for _, v := range baseline {
		if _, ok := cset[v]; ok {
			continue
		}
		if _, seen := indexOf(removed, v); seen {
			continue
		}
		removed = append(removed, v)
	}
	return added, removed
}

func indexOf(s []string, v string) (int, bool) {
	for i, x := range s {
		if x == v {
			return i, true
		}
	}
	return -1, false
}

// joinQuoted formats a list of strings as `"a", "b", "c"` for inclusion
// in a human-readable diff line. Single-element lists render without
// trailing punctuation so the caller can append "; removed ..." or "."
// without grammar warts.
func joinQuoted(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return strings.Join(quoted, ", ")
}
