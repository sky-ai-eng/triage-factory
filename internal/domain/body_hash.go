package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// JSONBodyHash fingerprints a complete source body (a JSON string or Jira ADF
// object). Empty means unknown, so an omitted field cannot look like a deletion.
// Explicit null and an empty string both mean cleared. Canonical JSON ignores
// object-key order and wire escaping, but retains ADF links and formatting.
func JSONBodyHash(raw json.RawMessage) string {
	var body any
	if len(raw) == 0 || json.Unmarshal(raw, &body) != nil {
		return ""
	}
	if body == nil {
		body = ""
	}
	switch body.(type) {
	case string, map[string]any:
	default:
		return ""
	}
	canonical, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
