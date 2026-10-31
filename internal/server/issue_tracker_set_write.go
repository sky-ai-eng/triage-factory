package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// errIssueTrackerSetMoved marks a write to a team's tracked Jira projects or
// Linear teams that was refused because the set changed after the read the
// request was built from. The error carries the route's 409 as well, so a
// route writes it with httpx.WriteFault, and a caller that can rebuild its
// request from the set as it now stands tells it apart with errors.Is.
var errIssueTrackerSetMoved = errors.New("the team's tracked set changed after it was read")

// recheckIssueTrackerSet is the first step of a write to a team's tracked Jira
// projects or Linear teams inside its write transaction. It takes the team's
// settings lock (TeamsStore.LockSettings), held until the transaction ends,
// reads the set again with read, the function prev and prevScope were read
// with, and refuses with a 409 when either moved.
//
// The pre-image is more than a comparison: a rule absent from the body is
// carried from it. Storing over a set another write replaced in the meantime
// would restore rules this caller never sent, and both callers would get a
// 200.
func recheckIssueTrackerSet[R any](
	ctx context.Context, tx db.TxStores, orgID, teamID string,
	read func(context.Context, db.TxStores, string, string) ([]R, string, error),
	prev []R, prevScope string, same func(a, b R) bool, what string,
) error {
	if err := tx.Teams.LockSettings(ctx, teamID); err != nil {
		return err
	}
	now, scope, err := read(ctx, tx, orgID, teamID)
	if err != nil {
		return err
	}
	if scope != prevScope || !slices.EqualFunc(prev, now, same) {
		return fmt.Errorf("%w: %w", errIssueTrackerSetMoved, httpx.NewFault(http.StatusConflict, httpx.ReasonConflict,
			"this team's "+what+" changed while saving; read them again and retry"))
	}
	return nil
}
