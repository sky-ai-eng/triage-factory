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
// write posted, edited or deleted, that comment's `comment` artifact
// (ghChannelCommentArtifact), composed in one write through the shared funnel.
//
// Exported because local mode records it without a LocalClient in hand (the
// delegate holds stores + ConversationInfo), while the sandbox path reaches it
// through LocalClient.RecordGHChannelWrite on the side that holds the stores:
// one function, so a write recorded from either mode leaves the same rows.
// credential is the tier the injector attached upstream. Best-effort.
func RecordGHChannelWrite(ctx context.Context, stores db.Stores, info ConversationInfo, obs ghwrite.Observation, credential string) {
	act := GHChannelWriteAction(obs, credential)
	RecordExternalWrite(ctx, stores, info, ghChannelCommentArtifact(ctx, stores, info, obs), act)
}

// ghChannelCommentArtifact builds the `comment` artifact a comment write
// through the channel leaves, or nil when the write is not an accepted post,
// edit or delete of a comment, or names no comment a row can be keyed on.
//
// The row is the one the exec verbs write for the same comment, keyed on the
// comment's numbering and its number (domain.GitHubCommentDedupKey), so a
// comment posted through one door and edited through the other is one row. A
// create's number is in its response; an edit or a delete names it in its REST
// path, or in a GraphQL response that selected it. A GraphQL edit or delete
// whose response did not return the number — deleteIssueComment's never does —
// names the comment by node id alone, so the row is found by the node id an
// earlier write stored. A node id no row carries leaves no artifact: there is
// no number to key a new row on, and the audit row still records the act.
func ghChannelCommentArtifact(ctx context.Context, stores db.Stores, info ConversationInfo, obs ghwrite.Observation) *domain.Artifact {
	if !obs.Succeeded() {
		return nil
	}
	shape, ok := ghwrite.Resolve(obs)
	if !ok || shape.Comment == "" {
		return nil
	}
	state := domain.ArtifactStateCommentPosted
	if shape.Action == domain.ActionCommentDeleted || shape.Action == domain.ActionReviewCommentDeleted {
		state = domain.ArtifactStateCommentDeleted
	}
	a := &domain.Artifact{
		Provider: domain.ArtifactProviderGitHub,
		Kind:     domain.ArtifactKindComment,
		NodeID:   shape.NodeID,
		URL:      obs.URL,
		State:    state,
		// Only a post says who wrote the comment. An edit or a delete reaches
		// a person's comment as readily as TF's own, and the store keeps an
		// earlier post's mark when a later write leaves it unset.
		PostedByTF: shape.Action == domain.ActionCommentPosted,
	}
	// owner/repo#N only: a path that names the comment by its own id carries
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
		a.DedupKey = domain.GitHubCommentDedupKey(shape.Comment, id)
		return a
	}
	if a.NodeID == "" {
		return nil
	}
	existing := commentArtifactByNodeID(ctx, stores, info, a.NodeID)
	if existing == nil {
		agenthostLog.Debug("gh channel comment write names a node id no artifact carries; recording the action only",
			"conversation", info.ConversationID, "action", shape.Action)
		return nil
	}
	a.DedupKey = existing.DedupKey
	a.ExternalID = existing.ExternalID
	return a
}

// commentArtifactByNodeID finds the GitHub comment artifact carrying nodeID in
// the org's current GitHub scope, or nil when none does or the read failed. A
// failure is logged and costs the artifact, never the audit row beside it.
func commentArtifactByNodeID(ctx context.Context, stores db.Stores, info ConversationInfo, nodeID string) *domain.Artifact {
	if stores.Artifacts == nil || stores.Orgs == nil {
		return nil
	}
	settings, err := stores.Orgs.GetSettingsSystem(ctx, info.OrgID)
	if err != nil {
		agenthostLog.Warn("gh channel comment write: load org settings for the node id lookup failed",
			"conversation", info.ConversationID, "error", err)
		return nil
	}
	scope := domain.ExternalObjectScope(domain.ArtifactProviderGitHub, settings)
	if scope == "" {
		return nil
	}
	found, err := stores.Artifacts.GetByNodeIDSystem(ctx, info.OrgID, scope, nodeID)
	if err != nil {
		agenthostLog.Warn("gh channel comment write: node id lookup failed",
			"conversation", info.ConversationID, "error", err)
		return nil
	}
	if found == nil || found.Provider != domain.ArtifactProviderGitHub || found.Kind != domain.ArtifactKindComment {
		return nil
	}
	return found
}
