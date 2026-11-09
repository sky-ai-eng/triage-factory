package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubRefreshMarkStore owns github_refresh_marks: the pending refresh marks
// the GitHub webhook receivers write, one row per target in a tracked
// repository, and the poll reads to learn what to refresh.
//
// A mark is written in the transaction that records its delivery
// (TxRunner.SystemWithTx, on SystemTxStores.GitHubRefreshMarks), because the
// lease holder that acts on it is usually not the pod that received the
// delivery: a delivery recorded without its mark would be a change the poll
// never hears about.
//
// Every method is a system method on the admin pool in Postgres, org_id bound
// by argument: the receiver is pre-auth and the poll runs with no claims.
// SQLite collapses to the one connection.
type GitHubRefreshMarkStore interface {
	// MarkSystem records that target, in the repository whose registry row is
	// repositoryID, changed. A target with no pending mark gets a new row at
	// revision 1; one that has a pending mark lands on that row, which moves
	// MarkedAt to now and bumps Revision, and keeps FirstMarkedAt. Returns the
	// row as stored. The target is refused unless it passes Validate, and the
	// repository must be a registry row of orgID.
	MarkSystem(ctx context.Context, orgID, repositoryID string, target domain.GitHubRefreshTarget) (domain.GitHubRefreshMark, error)

	// GetSystem reads one mark by id, or (nil, nil) when the org has no mark
	// with that id.
	GetSystem(ctx context.Context, orgID, id string) (*domain.GitHubRefreshMark, error)

	// ListSystem returns up to limit of the org's pending marks, ordered by
	// (FirstMarkedAt, ID), so the target that has waited longest is read
	// first. limit must be positive.
	ListSystem(ctx context.Context, orgID string, limit int) ([]domain.GitHubRefreshMark, error)

	// ListMarkedOrgsSystem returns the id of every org with at least one
	// pending mark, in id order. It is the one read not bound to an org: the
	// poll's sweep makes it to find the orgs whose marks a lost nudge left
	// waiting, without reading each active org's marks in turn.
	ListMarkedOrgsSystem(ctx context.Context) ([]string, error)

	// ClearSystem deletes each of read that is still at the revision it was
	// read at, and returns how many it deleted. A mark a delivery landed on
	// after it was read is at a later revision and is kept, because the pass
	// that read it may have refreshed before that delivery's change happened.
	// A mark of another org, or one already cleared, is skipped.
	//
	// Exempt from the returned-row rule: it deletes a set of rows.
	ClearSystem(ctx context.Context, orgID string, read []domain.GitHubRefreshMark) (int, error)
}

// ValidateGitHubRefreshMarkLimit refuses a ListSystem limit that is not
// positive, which both dialects would otherwise read as no limit or none.
func ValidateGitHubRefreshMarkLimit(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("github refresh marks: limit must be positive, got %d", limit)
	}
	return nil
}

// GitHubRefreshMarkReceiptsJSON encodes the (id, revision) pairs ClearSystem
// deletes at, as one JSON array both dialects bind as a single parameter.
func GitHubRefreshMarkReceiptsJSON(read []domain.GitHubRefreshMark) (string, error) {
	type receipt struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	out := make([]receipt, len(read))
	for i, m := range read {
		out[i] = receipt{ID: m.ID, Revision: m.Revision}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
