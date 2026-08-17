// Package convident (conversation identity) holds the shared identity
// helper used at the entry point of every `triagefactory exec ...`
// subcommand to resolve the (orgID, userID, conversationID) triple from
// the TRIAGE_FACTORY_CONVERSATION_ID env var the delegate spawner sets.
// Every field it returns is read off the conversations row that env var
// names, which is what the package and its type are named for.
//
// Lives in its own package (not in cmd/exec) so subcommand packages —
// chain, gh, workspace — can import the helper without forming an
// import cycle through cmd/exec's top-level dispatch.
//
// The pattern matches what internal/delegate/run.go established for
// the spawner-side bookkeeping: branch on the conversation's trigger_type so
// manual conversations route through synthetic-claims (carrying the human's
// identity) and event-triggered conversations route through admin-pool
// `...System` methods (no human identity exists).
//
// This helper backs cmd/exec/agenthost's LocalClient, which every
// subcommand reaches through agenthost.NewLocalFromEnv rather than
// calling here directly. It is host-side only: the jailed CLI never
// resolves identity itself — it talks to a host daemon over IPC
// (agenthost.IPCClient), which owns identity and the DB.
package convident

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ConversationIDEnvVar is the env var name the delegate spawner sets on
// the agent subprocess and `triagefactory exec ...` reads at startup.
// Hardcoded to match internal/delegate/run.go's runAgent, which
// handles the spawner-side injection.
const ConversationIDEnvVar = "TRIAGE_FACTORY_CONVERSATION_ID"

// ErrConversationIdentityMissing is returned by ResolveConversationIdentity when the
// TRIAGE_FACTORY_CONVERSATION_ID env var is unset. Surfaces as a clear
// "spawner bug" message — an agent invoking these commands without
// the env var present means the spawner failed to inject it.
var ErrConversationIdentityMissing = errors.New("TRIAGE_FACTORY_CONVERSATION_ID not set; this command must be invoked by the delegated agent spawner")

// ErrConversationIdentityNotFound is returned by ResolveConversationIdentity when the
// supplied conversationID doesn't match a row in the conversations table. Surfaces
// as a clear "stale env var / spawner bug" message in subcommand
// stderr. Subcommands errors.Is against this sentinel when they want
// to remap to their own package-level "not found" sentinels.
var ErrConversationIdentityNotFound = errors.New("TRIAGE_FACTORY_CONVERSATION_ID points at a conversation that does not exist; check spawner injection")

// ConversationIdentity is the resolved (orgID, userID, conversationID) triple for a
// cmd/exec subcommand invocation. Returned by ResolveConversationIdentity at
// every subcommand's entry point so the body can branch on
// IsEventTriggered to pick its store-routing strategy.
type ConversationIdentity struct {
	// OrgID is the conversation's owning org, read from the conversations row
	// keyed by TRIAGE_FACTORY_CONVERSATION_ID. In local mode this collapses
	// to runmode.LocalDefaultOrgID (the single seeded tenant); in
	// multi mode it carries the real tenant UUID so every
	// subcommand write attributes to the correct org.
	OrgID string

	// UserID is the conversation's creator_user_id — non-empty for manual
	// conversations (the human who pressed delegate / swiped agent), empty
	// for event-triggered conversations (no human asked for the work).
	// Manual subcommand callers wrap their writes in
	// SyntheticClaimsWithTx using this value; event-triggered
	// callers route through `...System` admin-pool methods.
	UserID string

	// ConversationID is TRIAGE_FACTORY_CONVERSATION_ID — the conversation the
	// subprocess is acting on behalf of. Stamped into the conversation_id
	// column of the rows a verb writes: artifacts, conversation_worktrees,
	// messages.
	ConversationID string

	// TeamID is the conversation's owning team (conversations.team_id, NOT NULL), read
	// straight off the conversation row GetSystem already loads — no task hop.
	// Carried onto the local-mode ConversationInfo (TFAC-458) so the capture
	// writers can stamp artifacts.team_id (NOT NULL per TFAC-455 F1).
	TeamID string

	// IsEventTriggered is true when the conversation was spawned by an
	// auto-delegation trigger rather than by a human action. The
	// discriminator that picks synthetic-claims vs admin-pool
	// routing in every subcommand. Mirrors the same trigger_type
	// branch internal/delegate/run.go uses for spawner-side
	// bookkeeping.
	IsEventTriggered bool
}

// ResolveConversationIdentityFromEnv is the CLI entry-point helper that reads
// TRIAGE_FACTORY_CONVERSATION_ID from the process env and delegates to
// ResolveConversationIdentity. Subcommands' top-level functions use this; the
// lower-level orchestration body of each subcommand takes the conversationID
// as a parameter so tests can drive routing without poking at env.
func ResolveConversationIdentityFromEnv(ctx context.Context, stores db.Stores) (ConversationIdentity, error) {
	return ResolveConversationIdentity(ctx, stores, os.Getenv(ConversationIDEnvVar))
}

// ResolveConversationIdentity looks up the conversation via the admin pool (we don't
// have user claims yet) and returns the routing-relevant identity
// fields. Empty conversationID surfaces as ErrConversationIdentityMissing — callers
// reading from env should validate up front and not pass "".
//
// Two admin-pool reads: first resolve the conversation's owning org by
// conversationID alone (the agent subprocess only has TRIAGE_FACTORY_CONVERSATION_ID
// in env, never the orgID), then load the full conversation row scoped to
// that org. Both reads bypass RLS because the subprocess hasn't
// entered a claims-bound tx — we don't know who to claim AS until
// after the lookup tells us conv.CreatorUserID.
func ResolveConversationIdentity(ctx context.Context, stores db.Stores, conversationID string) (ConversationIdentity, error) {
	if conversationID == "" {
		return ConversationIdentity{}, ErrConversationIdentityMissing
	}
	orgID, err := stores.Conversations.LookupOrgForConversationSystem(ctx, conversationID)
	if err != nil {
		return ConversationIdentity{}, fmt.Errorf("lookup org for conversation %s: %w", conversationID, err)
	}
	if orgID == "" {
		return ConversationIdentity{}, fmt.Errorf("%w: %s", ErrConversationIdentityNotFound, conversationID)
	}
	conv, err := stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		return ConversationIdentity{}, fmt.Errorf("lookup conversation %s: %w", conversationID, err)
	}
	if conv == nil {
		return ConversationIdentity{}, fmt.Errorf("%w: %s", ErrConversationIdentityNotFound, conversationID)
	}
	return ConversationIdentity{
		OrgID:            orgID,
		UserID:           conv.CreatorUserID,
		ConversationID:   conversationID,
		TeamID:           conv.TeamID,
		IsEventTriggered: conv.TriggerType == domain.TriggerTypeEvent,
	}, nil
}
