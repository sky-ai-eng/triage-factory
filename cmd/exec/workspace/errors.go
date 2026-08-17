package workspace

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/convident"
)

// translateLookupErr maps the convident error sentinels into the
// workspace-specific sentinels callers (tests + the CLI body) expect.
// Errors that don't match convident's typed sentinels pass through
// wrapped with cmdPrefix ("workspace add:" / "workspace list:") so
// the agent's stderr message correctly identifies the source command.
//
// conversationID is the env-derived run id when the caller has it (the
// ErrConversationIdentityNotFound branch uses it in the message); pass "" for
// LookupConversation call sites that don't know it yet because LookupConversation
// itself is what would have produced it.
func translateLookupErr(cmdPrefix, conversationID string, err error) error {
	switch {
	case errors.Is(err, convident.ErrConversationIdentityMissing):
		return errMissingConversationID
	case errors.Is(err, convident.ErrConversationIdentityNotFound):
		return fmt.Errorf("%w: %s", errRunNotFound, conversationID)
	default:
		return fmt.Errorf("%s: %w", cmdPrefix, err)
	}
}
