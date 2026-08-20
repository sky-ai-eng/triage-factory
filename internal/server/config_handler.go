package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// configResponse is the FE-facing shape exposed by GET /api/config.
//
// Single consumer + single purpose: AuthGate (D8) reads
// deployment_mode at boot to choose between the local keychain-capture
// flow and the multi-mode cookie auth flow. The call is unauthenticated
// — it has to work before login, hence the pre-auth allowlist mount in
// routes(). Per-user identity (github_username, jira_*) used to live
// here for the predicate editor; that data moved to /api/me, which now
// returns the same fields in both modes.
//
// Don't conflate with /api/settings (user-mutable preferences),
// /api/me (the caller's identity + org list), or
// POST /api/teams/{team_id}/members/list (the team roster).
type configResponse struct {
	DeploymentMode string `json:"deployment_mode"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configResponse{
		DeploymentMode: string(runmode.Current()),
	})
}

// handleHealth is the liveness probe target. Returns 200 once the
// server is accepting requests; platform restart logic uses this to
// decide when a Machine/container has come up. Deliberately does NOT
// reach into the DB or integrations — a flapping Postgres shouldn't
// recycle the whole TF process via the platform's auto-restart.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
