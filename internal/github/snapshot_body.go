package github

import (
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func snapshotBody(raw json.RawMessage) (body, hash string) {
	if len(raw) == 0 || json.Unmarshal(raw, &body) != nil {
		return "", ""
	}
	return body, domain.JSONBodyHash(raw)
}
