package db

import (
	"context"
	"errors"
	"strings"

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

// ArtifactPerformersSQL is domain.ArtifactPerformers as a list of SQL string
// literals, for the artifact reads' `performed_by IN (...)`.
func ArtifactPerformersSQL() string {
	return sqlStringLiterals(domain.ArtifactPerformers())
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

// ArtifactStore reads a run's artifacts from the external_actions ledger. It
// owns no table. An artifact group is the rows of one org that name a
// conversation the read asks for, are performed by one of
// domain.ArtifactPerformers (the agent, or a person acting on the run's
// deliverables), and name an object (the dialect's SQL twin of
// domain.ObjectID is non-NULL), grouped by that object id. TF's own rows
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
}
