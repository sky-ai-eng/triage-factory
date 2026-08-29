package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The pull-request list has two arms over one population, and they differ in
// WHOSE pull requests they answer for, never in how a caller narrows them:
//
//   - the personal arm, DashboardStore.PRs — org-wide, authored by the
//     caller's GitHub login OR commissioned by the caller;
//   - the team arm, TeamPRStore.TeamPRs — inside the team's tracked repos,
//     authored by a member OR structurally owned by the team.
//
// So the filter type and the state vocabulary are shared, and the row is the
// same domain.PRSummaryRow projection. The Overview's OPEN PRS figure is the
// team arm's count-only read, which is what makes the figure and the list
// agree by construction rather than by two queries staying in step.

// PRListFilter is the narrowing both arms take. Every field is a *narrowing*:
// a zero filter is the whole population, and the identity legs are never in
// here — they are what each arm is FOR, not something a caller may aim.
type PRListFilter struct {
	// States narrows to a subset of domain's open|merged|closed vocabulary.
	// Empty means every state, like every other narrowing filter. A value
	// outside the vocabulary is ErrUnknownPRState — never dropped, and never
	// widened to "all", which is what a silent fallback would do.
	//
	// "open" is the ENTITY's own state ('active'), not the snapshot's: it is
	// the population the factory belt renders, so the figure on the Overview
	// and the objects on the belt cannot disagree. "merged" and "closed" come
	// off the snapshot, closed meaning closed-and-not-merged.
	States []string
}

// ErrUnknownPRState is what both arms answer when States names something
// outside domain.PRListStates. The route validates first and reports the field
// — this is the store refusing to widen a result set behind a bad filter, the
// fault the list contract cares about most.
var ErrUnknownPRState = errors.New("db: unknown pull-request state filter")

// PRViewer is the personal arm's identity pair. Two ids because "mine" is a
// union of two different things: pull requests my GitHub login authored, and
// pull requests a run I commissioned opened under the bot's login. A struct
// rather than two string parameters because they are same-typed neighbours a
// call site could silently swap, and swapping them yields an empty list rather
// than an error.
//
// Either may be empty and the arm still answers: an unbound GitHub identity
// leaves Login empty (the personal list is then only what I commissioned), and
// the leg it names is simply absent from the predicate.
type PRViewer struct {
	// Login is the caller's GitHub login on the org's host — the value
	// snapshot_json ->> 'author' carries.
	Login string
	// UserID is the caller's TF user id — the value
	// entities.commissioned_by_user_id carries.
	UserID string
}

//go:generate go run github.com/vektra/mockery/v2 --name=TeamPRStore --output=./mocks --case=underscore --with-expecter

// TeamPRStore is the team arm: the pull requests a team is responsible for.
//
// Its own interface rather than a third method on DashboardStore, because the
// dashboard store is a viewer-relative projection (every method there takes
// the caller's GitHub username and answers about that one person) and this
// read is addressed at a team. It is the sibling of TeamActivityStore, and it
// scopes the same way: read it through TxStores under the caller's claims —
// on Postgres the tracked-set semi-join leans on team_github_repos RLS, so a
// caller who is not on the team sees an empty list rather than a team's work.
type TeamPRStore interface {
	// TeamPRs returns one page of the team's pull requests, newest polled
	// first, plus the filtered total.
	//
	// The population is: github entities in the repos this team tracks, that
	// a member of this team authored OR that carry this team as their
	// structural owner (entities.owning_team_id — TF opened them on the
	// team's behalf). The tracked set is the outer filter; the two legs are
	// the inner OR.
	//
	// githubBaseURL is the org's configured host, raw from org_settings — the
	// impl resolves it with EffectiveGitHubHost, since member identities are
	// keyed under the effective host and an unset setting must not look up
	// host="". A member who has bound no identity on that host contributes
	// nothing until they bind; /api/me is where identity state is surfaced.
	TeamPRs(ctx context.Context, orgID, teamID, githubBaseURL string, f PRListFilter, opts ListOpts) ([]domain.PRSummaryRow, int, error)
}
