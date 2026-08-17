// The write-side check on the SDK resume's coordinates.
//
// A woken SDK conversation is re-invoked by session id, in the tree it ran in,
// on the model it started with. All three live on the conversation row, all
// three are written by whoever claims it, and nothing between the write and the
// wake verifies the write happened — the read side just refuses, days later, in
// a composer, naming a field the person has never heard of. That gap has cost a
// night of archaeology once; these assertions make the next one cost a grep.
//
// Loud but never fatal. The engagement in front of us can run perfectly well on
// an incomplete row; it is only the follow-up months of that conversation's life
// that the missing field ends.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// resumeCheckpoint names where in an SDK engagement's life the coordinates are
// checked. It is the only thing that varies between the two calls: a session id
// does not exist until the stream's first system/init, so only a row that has
// come to rest can be held to it.
type resumeCheckpoint string

const (
	// resumeCheckClaim runs at engagement start, where the path and the model
	// are already the claim's responsibility to have written.
	resumeCheckClaim resumeCheckpoint = "claim"
	// resumeCheckRest runs once the stream is over and the row is about to be
	// what a follow-up finds: parked, or terminal on a step a user may still
	// pick up.
	resumeCheckRest resumeCheckpoint = "rest"
)

// missingResumeCoordinates names the SDK resume fields absent from a row, in
// the refusal ladder's own order so the log reads the way the eventual refusal
// will. Session id is checked only at rest, for the reason resumeCheckpoint
// gives — asserting it at claim time would fire on every healthy run.
func missingResumeCoordinates(conv domain.Conversation, at resumeCheckpoint) []string {
	var missing []string
	if at == resumeCheckRest && conv.SessionID == "" {
		missing = append(missing, "sdk_session_id")
	}
	if conv.WorktreePath == "" {
		missing = append(missing, "worktree_path")
	}
	if conv.Model == "" {
		missing = append(missing, "model")
	}
	return missing
}

// assertResumeCoordinates re-reads the conversation and warns once per field a
// wake would refuse it for.
//
// The row is re-read rather than checked against the values in hand, and that
// is the whole point: the bug this exists to catch is code that resolved a
// coordinate correctly and never wrote it down. Checking what the caller
// computed would have agreed with the caller and said nothing.
//
// Called only from the SDK arm, so the check is structurally absent for native
// conversations — their message rows are the resume state and none of these
// fields is load-bearing. A row it cannot read is not a finding: a transient
// read failure says nothing about what was written, so it goes to debug.
func (s *Spawner) assertResumeCoordinates(ctx context.Context, orgID, conversationID string, at resumeCheckpoint) {
	if s.conversations == nil {
		return
	}
	conv, err := s.conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil || conv == nil {
		delegateLog.Debug("could not re-read the conversation to check its resume coordinates",
			"conversation", conversationID, "org_id", orgID, "checkpoint", string(at), "error", err)
		return
	}
	for _, field := range missingResumeCoordinates(*conv, at) {
		delegateLog.Warn("SDK conversation is missing a resume coordinate; resume will refuse this row",
			"conversation", conversationID, "org_id", orgID, "checkpoint", string(at), "field", field)
	}
}
