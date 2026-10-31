package agenthost

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
)

// RecordGHChannelWrite records one write the real-`gh` channel forwarded: the
// audit row every write leaves (GHChannelWriteAction) and, for a comment the
// write posted or deleted, that comment's `comment` deliverable
// (ghChannelCommentDeliverable), composed in one write through the shared funnel.
//
// Exported because local mode records it without a LocalClient in hand (the
// delegate holds stores + ConversationInfo), while the sandbox path reaches it
// through LocalClient.RecordGHChannelWrite on the side that holds the stores:
// one function, so a write recorded from either mode leaves the same rows.
// credential is the tier the injector attached upstream, and obs.Identity what
// GitHub answered it for the write's objects, which the rows are linked by.
// Best-effort.
func RecordGHChannelWrite(ctx context.Context, stores db.Stores, info ConversationInfo, obs ghwrite.Observation, credential string) {
	act := GHChannelWriteAction(obs, credential)
	recordExternalWrite(ctx, stores, info, ghChannelCommentDeliverable(ctx, stores, info, obs), act, domain.EntityContainer{}, obs.Identity)
}

// ghChannelCommentDeliverable builds the `comment` deliverable a comment write
// through the channel leaves, or nil when the write is not an accepted post or
// delete of a comment, or names no comment a row can be keyed on. An edit
// leaves none: the row stores no body, so an edit has nothing on it to change,
// and its audit row records it.
//
// The row is the one the exec verbs write for the same comment, keyed on the
// comment's numbering and its number (domain.GitHubCommentKey), so a
// comment posted through one door and deleted through the other is one row. A
// create's number is in its response; a REST delete names it in its path. A
// GraphQL delete names the comment by node id alone, and its response never
// returns the number, so the row is found by the node id the post stored. A
// node id no row carries leaves no deliverable: TF did not post that comment, or
// its post was never recorded, and the audit row still records the delete. A
// delete keyed by number leaves none for a comment TF did not post either
// (RecordExternalWrite).
func ghChannelCommentDeliverable(ctx context.Context, stores db.Stores, info ConversationInfo, obs ghwrite.Observation) *domain.Deliverable {
	if !obs.Succeeded() {
		return nil
	}
	shape, ok := ghwrite.Resolve(obs)
	if !ok || shape.Comment == "" {
		return nil
	}
	var state string
	switch shape.Action {
	case domain.ActionCommentPosted:
		state = domain.DeliverableStateCommentPosted
	case domain.ActionCommentDeleted, domain.ActionReviewCommentDeleted:
		state = domain.DeliverableStateCommentDeleted
	default:
		return nil
	}
	a := &domain.Deliverable{
		Provider: domain.ProviderGitHub,
		Kind:     domain.DeliverableKindComment,
		NodeID:   shape.NodeID,
		URL:      obs.URL,
		State:    state,
	}
	// owner/repo#N only: a delete names the comment by its own id and carries
	// no PR number, and a repo-level target would overwrite the post's.
	if shape.Owner != "" && shape.Number > 0 {
		a.Target = shape.Target()
	}
	if shape.InReplyTo > 0 {
		detail, _ := json.Marshal(map[string]any{"in_reply_to": shape.InReplyTo})
		a.DetailsJSON = string(detail)
	}

	if id, err := strconv.ParseInt(shape.ExternalID, 10, 64); err == nil && id > 0 {
		a.ExternalID = strconv.FormatInt(id, 10)
		a.DedupKey = domain.GitHubCommentKey(shape.Comment, id)
		return a
	}
	if a.NodeID == "" {
		return nil
	}
	existing := commentDeliverableByNodeID(ctx, stores, info, a.NodeID)
	if existing == nil {
		agenthostLog.Debug("gh channel comment write names a node id no deliverable carries; recording the action only",
			"conversation", info.ConversationID, "action", shape.Action)
		return nil
	}
	a.DedupKey = existing.DedupKey
	a.ExternalID = existing.ExternalID
	return a
}

// commentDeliverableByNodeID finds the GitHub comment deliverable carrying nodeID in
// the org's current GitHub scope, or nil when none does or the read failed. A
// failure is logged and costs the deliverable, never the audit row beside it.
func commentDeliverableByNodeID(ctx context.Context, stores db.Stores, info ConversationInfo, nodeID string) *domain.Deliverable {
	if stores.Deliverables == nil || stores.Orgs == nil {
		return nil
	}
	settings, err := stores.Orgs.GetSettingsSystem(ctx, info.OrgID)
	if err != nil {
		agenthostLog.Warn("gh channel comment write: load org settings for the node id lookup failed",
			"conversation", info.ConversationID, "error", err)
		return nil
	}
	scope := domain.ExternalObjectScope(domain.ProviderGitHub, settings)
	if scope == "" {
		return nil
	}
	found, err := stores.Deliverables.GetByNodeIDSystem(ctx, info.OrgID, scope, nodeID)
	if err != nil {
		agenthostLog.Warn("gh channel comment write: node id lookup failed",
			"conversation", info.ConversationID, "error", err)
		return nil
	}
	if found == nil || found.Provider != domain.ProviderGitHub || found.Kind != domain.DeliverableKindComment {
		return nil
	}
	return found
}
