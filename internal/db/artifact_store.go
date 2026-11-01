package db

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ArtifactExcludedActionsSQL is domain.ActionsNamingNoObject as a list of SQL
// string literals, for the artifact reads' `action NOT IN (...)`: a row of
// one of these actions is never an artifact, whatever object it carries. The
// values are TF's own action names; each is quoted with any single quote
// doubled, which both dialects read the same way.
func ArtifactExcludedActionsSQL() string {
	return sqlStringLiterals(domain.ActionsNamingNoObject())
}

// ArtifactPerformerSQL is the artifact reads' predicate on who performed a
// row of external_actions aliased ea: one of domain.ArtifactPerformers, and
// for a person, a row that names a conversation. A person's row with none (a
// dashboard draft toggle, a board drag) acts on no run's deliverable; the
// reads scoped to a team or an org would otherwise list it as a run's.
func ArtifactPerformerSQL() string {
	return `ea.performed_by IN (` + sqlStringLiterals(domain.ArtifactPerformers()) + `) AND (ea.performed_by <> ` +
		sqlStringLiterals([]string{domain.PerformedByPerson}) + ` OR ea.conversation_id IS NOT NULL)`
}

// ArtifactRowSQL is the predicate a row of external_actions aliased ea meets
// to be one of an artifact's rows, whatever its object: performed by an
// artifact performer (ArtifactPerformerSQL), of an action that can name an
// object (ArtifactExcludedActionsSQL). The artifact reads add that the row
// names an object; the action list's object filter adds which.
func ArtifactRowSQL() string {
	return ArtifactPerformerSQL() + ` AND ea.action NOT IN (` + ArtifactExcludedActionsSQL() + `)`
}

func sqlStringLiterals(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return strings.Join(quoted, ", ")
}

// RequireArtifactOffsetPage refuses a keyset window: the artifact lists page
// by offset. Both dialects call it before touching the database.
func RequireArtifactOffsetPage(opts ListOpts) error {
	if len(opts.After) > 0 {
		return errors.New("db: the artifact lists page by offset, not by keyset")
	}
	return nil
}

// SplitArtifactTeamIDs reads the team ids an object-activity read joins with
// commas (uuids hold none) as a sorted slice, empty rather than nil for none.
// Both dialects scan through it, so TeamIDs sorts the same bytewise way in
// each whatever the database's collation.
func SplitArtifactTeamIDs(joined string) []string {
	if joined == "" {
		return []string{}
	}
	ids := strings.Split(joined, ",")
	slices.Sort(ids)
	return slices.Compact(ids)
}

// ArtifactStore reads a run's artifacts from the external_actions ledger. It
// owns no table. An artifact group is the rows of one org that name a
// conversation the read asks for, are performed by one of
// domain.ArtifactPerformers (the agent, or a person acting on the run's
// deliverables, whose row names its conversation; ArtifactPerformerSQL), and
// name an object (the dialect's SQL twin of domain.ObjectID is non-NULL),
// grouped by that object id. TF's own rows
// never form an artifact, and neither does a row of an action that names no
// object (a refusal, a failure, a catch-all; ArtifactExcludedActionsSQL),
// whatever object it carries. Rows whose conversation was purged (conversation_id NULL) answer for
// no conversation. See domain.Artifact for the fields and the display rule.
//
// The app-pool methods run under the caller's claims in Postgres, where the
// external_actions policy scopes by org; the handler reads the conversation
// first under the same claims, so a conversation the caller cannot see is a
// 404 before these run. The ...System methods run on the admin pool for
// readers with no claims. SQLite is N=1 and unscoped; both collapse to the one
// connection.
type ArtifactStore interface {
	// ListByConversation returns one page of conversationID's artifacts, by
	// LastAt descending, then ObjectID, and the number of artifacts in all.
	// opts windows the page; Unwindowed reads every artifact. App pool.
	ListByConversation(ctx context.Context, orgID, conversationID string, opts ListOpts) ([]domain.Artifact, int, error)

	// ListByConversationSystem is ListByConversation on the admin pool.
	ListByConversationSystem(ctx context.Context, orgID, conversationID string, opts ListOpts) ([]domain.Artifact, int, error)

	// CountByConversations returns, for each of conversationIDs that has any
	// artifact, its number of artifacts by object kind. A conversation with
	// none has no entry. An empty conversationIDs returns an empty map without
	// touching the database. App pool.
	CountByConversations(ctx context.Context, orgID string, conversationIDs []string) (map[string]map[string]int, error)

	// ExistsForConversationSystem reports whether conversationID has any
	// artifact. Admin pool.
	ExistsForConversationSystem(ctx context.Context, orgID, conversationID string) (bool, error)

	// ListForConversationsSystem returns the artifacts of conversationIDs
	// grouped across all of them: one artifact per object, however many of the
	// conversations changed it, with ActionCount, FirstAt and LastAt spanning
	// every one. By FirstAt ascending, then ObjectID. Unpaged: the caller
	// passes one task's conversations. An empty conversationIDs returns nil
	// without touching the database. Admin pool.
	ListForConversationsSystem(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Artifact, error)

	// ListByTeam returns one page of the objects teamID's runs changed: the
	// artifact rows whose team_id is teamID, with any conversation or none,
	// grouped by object alone, so an object several runs changed is one item.
	// opts filters the rows before they are grouped. By LastAt descending,
	// then ObjectID; the total is the number of objects. App pool.
	ListByTeam(ctx context.Context, orgID, teamID string, opts ArtifactListOpts) ([]domain.ObjectActivity, int, error)

	// ListByOrgSystem is ListByTeam over every row of the org, whatever its
	// team. Admin pool.
	ListByOrgSystem(ctx context.Context, orgID string, opts ArtifactListOpts) ([]domain.ObjectActivity, int, error)
}

// ArtifactListOpts filters and windows ArtifactStore.ListByTeam and
// ListByOrgSystem. Each filter is exact-match and applies only when set.
// They select rows, not objects: an object's ActionCount, ConversationCount,
// TeamIDs and FirstAt count only its rows that pass them. Since and Until bound
// occurred_at to the half-open window [Since, Until). ListOpts windows the
// page of objects and pages by offset (RequireArtifactOffsetPage).
type ArtifactListOpts struct {
	Provider   string
	ObjectKind string
	Since      time.Time
	Until      time.Time

	ListOpts
}
