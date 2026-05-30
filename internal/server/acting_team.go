package server

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// resolveActingTeam returns the team a write should be attributed to for
// the current request — "the acting team." It centralizes the
// default/first-team assumption that used to be hardcoded at every write
// site so the multi-team selector work can layer the real selection on
// top in one place instead of N.
//
// Resolution order — note that today only step 3 executes; steps 1 and 2
// are the design the multi-team selector (SKY-294) will fill in, listed
// here so the intended contract is on the page before PR2 implements it.
// The body below is just the GetDefaultForOrg call (step 3):
//
//  1. The requesting user's sticky default team, if set. The backing
//     column doesn't exist yet, so this step is inert today — there is
//     nothing to read.
//
//  2. Else the user's sole team. There is no "teams for this user" store
//     method yet, and every org currently has exactly one team, so the
//     default-team lookup below already returns that sole team.
//
//  3. Else (the user is on two or more teams, has no sticky default, and
//     the write-time picker hasn't shipped) fall back to the org's
//     deterministic default/first team (oldest by created_at).
//
//     TODO(SKY-294): the multi-team selector resolves this branch
//     properly (sticky default → most-recent → explicit pick). Until it
//     ships, a caller on two or more teams silently lands on the default
//     team — by decision (2026-05-30) we fall back rather than 400,
//     because multi-mode isn't live and no real multi-team orgs exist, so
//     a guessed team can't mislead anyone.
//
// Solo behavior — the only behavior today — is byte-identical to the
// previous default-team call sites: the sole team is returned everywhere
// a default was hardcoded. Call this inside the request's WithTx so the
// teams_select RLS policy gates the lookup under the caller's claims.
func resolveActingTeam(ctx context.Context, teams db.TeamsStore, orgID string) (string, error) {
	teamID, err := teams.GetDefaultForOrg(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("acting team lookup: %w", err)
	}
	if teamID == "" {
		// Every install gets a default team at provisioning time, so an
		// empty result is a bootstrap bug, not a normal state.
		return "", fmt.Errorf("org %s has no team", orgID)
	}
	return teamID, nil
}
