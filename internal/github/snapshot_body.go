package github

import (
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// snapshotBody decodes a pull request's body for its snapshot: the text, its
// fingerprint and the users and teams it mentions. All three are empty, the
// mentions nil, when the response carried no body.
func snapshotBody(raw json.RawMessage) (body, hash string, mentions []string) {
	hash = domain.JSONBodyHash(raw)
	if hash == "" {
		return "", "", nil
	}
	// Explicit null hashes as cleared but fails this unmarshal; body
	// correctly stays "" since it's never assigned from the zero value.
	_ = json.Unmarshal(raw, &body)
	return body, hash, Mentions(body)
}
