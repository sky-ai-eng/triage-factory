package github

import (
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func snapshotBody(raw json.RawMessage) (body, hash string) {
	hash = domain.JSONBodyHash(raw)
	if hash == "" {
		return "", ""
	}
	// Explicit null hashes as cleared but fails this unmarshal; body
	// correctly stays "" since it's never assigned from the zero value.
	_ = json.Unmarshal(raw, &body)
	return body, hash
}
