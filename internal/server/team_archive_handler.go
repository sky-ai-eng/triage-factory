package server

import (
	"errors"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Team archive/restore lifecycle (TFAC-448). Archiving a team is a soft-delete
// that immediately force-stops all of the team's in-flight work — agent
// delegations and curator sessions — and blocks all further writes. There is no
// "let it finish" option by design. Org-admin only (matches the org-admin-only
// teams_delete RLS); multi-mode only (local is N=1 and never archives its sole
// team). Restore flips the tombstone back but deliberately does NOT resurrect
// the runs / curator sessions that archive force-stopped.

// teamArchivePreviewJSON is the shared count shape: the preview reports the work
// an archive WOULD stop; the archive response reports what it DID stop. Field
// names differ between the two responses (active_* vs cancelled_*) so the two
// shapes below embed concrete fields rather than this struct.

// teamArchivePreviewJSON backs the confirm modal: how much live work an archive
// will force-stop, plus the team's current archived state so a stale client can
// render "already archived".
type teamArchivePreviewJSON struct {
	TeamID                string `json:"team_id"`
	Name                  string `json:"name"`
	Archived              bool   `json:"archived"`
	ActiveRuns            int    `json:"active_runs"`
	ActiveCuratorSessions int    `json:"active_curator_sessions"`
}

// teamArchiveResultJSON is the POST /archive response: the counts of work the
// cascade actually stopped.
type teamArchiveResultJSON struct {
	CancelledRuns            int `json:"cancelled_runs"`
	CancelledCuratorSessions int `json:"cancelled_curator_sessions"`
}

// resolveTeamForLifecycle runs the shared front gate for every archive/restore
// endpoint: multi-mode only, active org resolved, a syntactically valid team_id
// that belongs to the org, and an org-admin caller. A non-org-admin gets 403,
// not 404: VerifyTeamInOrg has already established that the team is in the
// caller's org, which means GET /api/teams lists it for them, so hiding it
// here would deny the existence of a row they can see. 404 stays where it
// belongs — the team isn't in this org, or the id isn't an id. Returns the
// resolved (orgID, userID, teamID) and ok=false when a response was already
// written.
func (th *teamsHandler) resolveTeamForLifecycle(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		notFound(w, "route")
		return "", "", "", false
	}
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject

	teamID, ok = th.az.TeamIDFromPath(w, r, "teams", orgID, userID)
	if !ok {
		return "", "", "", false
	}
	// Cross-org 404 before the role gate — non-disclosure of teams in other orgs.
	if !th.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return "", "", "", false
	}
	isAdmin, err := th.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "teams", err)
		return "", "", "", false
	}
	if !isAdmin {
		forbidden(w, "org admin role required")
		return "", "", "", false
	}
	return orgID, userID, teamID, true
}

// handleTeamArchivePreview returns the live-work counts the archive confirm
// modal surfaces ("ends N delegations and M curator sessions"). Read-only — no
// state changes. Org-admin only, multi-mode only.
//
// GET /api/teams/{team_id}/archive/preview
func (th *teamsHandler) handleTeamArchivePreview(w http.ResponseWriter, r *http.Request) {
	orgID, _, teamID, ok := th.resolveTeamForLifecycle(w, r)
	if !ok {
		return
	}

	team, err := th.allStores.Teams.GetSystem(r.Context(), orgID, teamID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	if team == nil {
		notFound(w, "team")
		return
	}

	activeRuns, curatorSessions, err := th.teamActiveWork(r, orgID, teamID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	writeJSON(w, http.StatusOK, teamArchivePreviewJSON{
		TeamID:                teamID,
		Name:                  team.Name,
		Archived:              team.DeletedAt != nil,
		ActiveRuns:            activeRuns,
		ActiveCuratorSessions: curatorSessions,
	})
}

// handleTeamArchive soft-deletes the team and force-stops its in-flight work.
// Order: stamp deleted_at FIRST (so no further team-scoped writes can land —
// new runs / curator turns are blocked), THEN enumerate and cancel the active
// runs + curator sessions. Enumerating after the tombstone keeps the cascade
// from missing work that started in the window between the read and the stamp.
// The cascade runs on the admin pool (spawner.Cancel with an empty userID,
// curator.CancelProject) so the now-archived team's write RLS doesn't block the
// teardown. Returns the counts of work stopped.
//
// POST /api/teams/{team_id}/archive
func (th *teamsHandler) handleTeamArchive(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := th.resolveTeamForLifecycle(w, r)
	if !ok {
		return
	}

	team, err := th.allStores.Teams.GetSystem(r.Context(), orgID, teamID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	if team == nil {
		notFound(w, "team")
		return
	}
	if team.DeletedAt != nil {
		conflict(w, "team is already archived")
		return
	}

	// Stamp deleted_at FIRST so no new writes can start, then reap. teams_update
	// RLS gates the write (org-admin via the front gate); a zero-row result means
	// it was archived in a race past GetSystem.
	if err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Teams.Archive(r.Context(), teamID)
	}); err != nil {
		if errors.Is(err, db.ErrTeamNotFound) {
			conflict(w, "team is already archived")
			return
		}
		internalError(w, "teams", err)
		return
	}

	// Enumerate the work to stop now that the tombstone blocks new writes. The
	// curator in-flight count is a point-in-time read taken before CancelProject
	// clears the in-flight marker; the run ids are stable (cancellation only moves
	// them to terminal).
	conversationIDs, err := th.allStores.Conversations.ActiveIDsForTeamSystem(r.Context(), orgID, teamID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	projectIDs, err := th.teamProjectIDs(r, orgID, teamID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	var curatorSessions int
	if cur := th.curatorRuntime(); cur != nil {
		curatorSessions = cur.InFlightProjectCount(projectIDs)
	}

	// Force-stop the runs. StopAndCancelBlueprint("" userID) hard-kills a live
	// process or parks one that has none, and cancels the blueprint behind it —
	// an archived team's work is over, so a frozen 'running' blueprint would
	// hold a worktree nobody can resume. All on the admin pool. A per-run error
	// is a benign race (the run reached terminal on its own) — log and keep going
	// so one stuck run can't strand the rest; count only the ones we stopped.
	cancelledRuns := 0
	if sp := th.spawnerRuntime(); sp != nil {
		for _, conversationID := range conversationIDs {
			if cErr := sp.StopAndCancelBlueprint(orgID, conversationID, "", delegate.StopCauseTeamArchived); cErr != nil {
				teamsLog.Warn("archive: run stop failed", "team", teamID, "conversation", conversationID, "error", cErr)
				continue
			}
			cancelledRuns++
		}
	}

	// Force-stop the curator sessions: cancel the in-flight request + drain queued
	// rows for every project the team owns.
	if cur := th.curatorRuntime(); cur != nil {
		for _, projectID := range projectIDs {
			cur.CancelProject(orgID, projectID, "team archived")
		}
	}

	teamsLog.Info("team archived",
		"org", orgID, "team", teamID,
		"cancelled_runs", cancelledRuns, "cancelled_curator_sessions", curatorSessions)
	writeJSON(w, http.StatusOK, teamArchiveResultJSON{
		CancelledRuns:            cancelledRuns,
		CancelledCuratorSessions: curatorSessions,
	})
}

// handleTeamRestore clears the team's tombstone so it's visible + writable
// again. Killed runs and curator sessions are NOT resurrected — archive's
// teardown is terminal by design. Org-admin only, multi-mode only.
//
// POST /api/teams/{team_id}/restore
func (th *teamsHandler) handleTeamRestore(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := th.resolveTeamForLifecycle(w, r)
	if !ok {
		return
	}

	team, err := th.allStores.Teams.GetSystem(r.Context(), orgID, teamID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	if team == nil {
		notFound(w, "team")
		return
	}
	if team.DeletedAt == nil {
		conflict(w, "team is not archived")
		return
	}

	if err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Teams.Restore(r.Context(), teamID)
	}); err != nil {
		if errors.Is(err, db.ErrTeamNotFound) {
			// Restored in a race past GetSystem — treat as already-restored.
			conflict(w, "team is not archived")
			return
		}
		internalError(w, "teams", err)
		return
	}

	teamsLog.Info("team restored", "org", orgID, "team", teamID)
	// team was read before the restore, so its tombstone is stale by the time
	// we serialize it. Clear it rather than echoing an archived_at the write
	// just removed — a response must not assert a state the request undid.
	team.DeletedAt = nil
	writeJSON(w, http.StatusOK, teamToJSON(*team, ""))
}

// archivedTeamListRequest is the body of POST /api/teams/archived/list. The
// resource is "the org's archived teams", which has no filters of its own.
type archivedTeamListRequest struct {
	httpx.PageRequest
}

// handleTeamArchivedList returns the org's archived teams for the org-admin
// restore surface (a toggle on the Teams management view). Org-admin only,
// multi-mode only.
//
// It is its own route rather than a flag on POST /api/teams/list because the
// two reads are different resources wearing one word: the active list is
// membership-scoped and answers "my teams", while this one is org-scoped and
// admin-gated so a restore surface can reach a team its admin never joined.
// Folding them together would mean one route whose scope, gate, and result set
// all change with a boolean.
//
// Rows carry `role` empty for the same reason: an org admin restoring a team
// they were never on has no membership to report.
//
// POST /api/teams/archived/list
func (th *teamsHandler) handleTeamArchivedList(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		notFound(w, "route")
		return
	}
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	isAdmin, err := th.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	if !isAdmin {
		// The caller is an org member by construction (requireOrg resolved
		// their active org), so the org is visible and the denial names the
		// role rather than hiding the surface.
		forbidden(w, "org admin role required")
		return
	}

	var req archivedTeamListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	teams, total, err := th.allStores.Teams.ListArchivedForOrgSystem(r.Context(), orgID,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset})
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	out := make([]teamJSON, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamToJSON(t, ""))
	}
	httpx.WriteList(w, page, out, total)
}

// teamActiveWork returns the count of active runs + in-flight curator sessions
// for the team — the preview's two numbers. Runs are read on the admin pool
// (ActiveIDsForTeamSystem); curator sessions are the in-flight count over the
// team's projects.
func (th *teamsHandler) teamActiveWork(r *http.Request, orgID, teamID string) (runs, curatorSessions int, err error) {
	conversationIDs, err := th.allStores.Conversations.ActiveIDsForTeamSystem(r.Context(), orgID, teamID)
	if err != nil {
		return 0, 0, err
	}
	projectIDs, err := th.teamProjectIDs(r, orgID, teamID)
	if err != nil {
		return 0, 0, err
	}
	if cur := th.curatorRuntime(); cur != nil {
		curatorSessions = cur.InFlightProjectCount(projectIDs)
	}
	return len(conversationIDs), curatorSessions, nil
}

// spawnerRuntime / curatorRuntime resolve the wired delegation spawner / curator
// runtime, or nil. They guard the getter func ITSELF being nil (not just the
// pointer it returns) so the archive cascade degrades to "nothing to cancel"
// instead of panicking on a teamsHandler constructed without the getters wired
// (early startup, a future refactor, or a test fixture that doesn't need them).
func (th *teamsHandler) spawnerRuntime() *delegate.Spawner {
	if th.spawner == nil {
		return nil
	}
	return th.spawner()
}

func (th *teamsHandler) curatorRuntime() *curator.Curator {
	if th.curator == nil {
		return nil
	}
	return th.curator()
}

// teamProjectIDs returns the ids of every project owned by the team. Reads the
// org's projects on the admin pool (ListSystem) and filters by team_id — the
// org-admin reaper may not be a member of the team, so the app-pool projects_select
// RLS would hide team-visibility projects.
func (th *teamsHandler) teamProjectIDs(r *http.Request, orgID, teamID string) ([]string, error) {
	projects, err := th.allStores.Projects.ListSystem(r.Context(), orgID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range projects {
		if p.TeamID == teamID {
			ids = append(ids, p.ID)
		}
	}
	return ids, nil
}
