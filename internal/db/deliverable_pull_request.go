package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrDeliverableNamesNoPullRequest means a deliverable the caller expected to be
// about a GitHub pull request names none: its entity is not a pull request
// (no repository, no number in its key), or, for a deliverable recorded unlinked,
// its target is not owner/repo#N.
var ErrDeliverableNamesNoPullRequest = errors.New("deliverable names no pull request")

// DeliverablePullRequest is the GitHub pull request a deliverable is about, as
// GitHub addresses it now.
type DeliverablePullRequest struct {
	// RepositoryID is the registry row of the pull request's repository, ""
	// for a deliverable recorded unlinked.
	RepositoryID string
	// Owner and Repo are the name GitHub serves the repository under now —
	// the registry row's — for request paths and credential resolution.
	Owner, Repo string
	// Number is the pull request's number, which no rename changes.
	Number int
	// Retired reports a retired registry row: GitHub serves its name as
	// another repository now, so nothing may be addressed under it.
	Retired bool
}

// Slug is the repository's "owner/repo".
func (p DeliverablePullRequest) Slug() string { return p.Owner + "/" + p.Repo }

// ResolveDeliverablePullRequest returns the pull request a GitHub pull request,
// review or comment deliverable is about: the deliverable's entity
// (deliverables.entity_id), the registry row that entity references
// (entities.repository_id), and the number the entity's key carries. Both are
// read by id, so the answer is the repository's current name whatever the
// deliverable's target or the entity's key still spells, and a retired row is
// reported as retired rather than read as whatever GitHub now serves under its
// name. entity and repository are the point reads of the caller's pool —
// EntityStore.Get or GetSystem, RepositoryStore.Get or GetSystem.
//
// A deliverable recorded unlinked (its entity did not resolve when it was
// written), or whose entity has since been deleted, carries no reference but
// its target, and is read from that: the one place a stored target is read as
// a repository. ErrDeliverableNamesNoPullRequest when there is nothing to read; a
// failed read is returned as it came.
func ResolveDeliverablePullRequest(
	ctx context.Context,
	orgID string,
	art domain.Deliverable,
	entity func(ctx context.Context, orgID, id string) (*domain.Entity, error),
	repository func(ctx context.Context, orgID, id string) (*domain.Repository, error),
) (DeliverablePullRequest, error) {
	if art.EntityID != "" {
		e, err := entity(ctx, orgID, art.EntityID)
		if err != nil {
			return DeliverablePullRequest{}, fmt.Errorf("read deliverable %s's entity: %w", art.ID, err)
		}
		if e != nil {
			number := domain.GitHubPRNumber(e.SourceID)
			if e.RepositoryID == "" || number <= 0 {
				return DeliverablePullRequest{}, fmt.Errorf("%w: deliverable %s's entity %s", ErrDeliverableNamesNoPullRequest, art.ID, e.ID)
			}
			row, err := repository(ctx, orgID, e.RepositoryID)
			if err != nil {
				return DeliverablePullRequest{}, fmt.Errorf("read deliverable %s's repository: %w", art.ID, err)
			}
			if row == nil {
				return DeliverablePullRequest{}, fmt.Errorf("%w: %s", ErrNoSuchRepository, e.RepositoryID)
			}
			return DeliverablePullRequest{
				RepositoryID: row.ID,
				Owner:        row.Owner,
				Repo:         row.Repo,
				Number:       number,
				Retired:      row.RetiredAt != nil,
			}, nil
		}
	}
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return DeliverablePullRequest{}, fmt.Errorf("%w: deliverable %s, target %q", ErrDeliverableNamesNoPullRequest, art.ID, art.Target)
	}
	return DeliverablePullRequest{Owner: owner, Repo: repo, Number: number}, nil
}
