package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// configResponse is the FE-facing shape exposed by GET /api/config: the
// deployment facts a client needs before anyone has signed in. The call is
// unauthenticated — it has to work before login, hence the pre-auth
// allowlist mount in routes() — so it carries deployment configuration only,
// never anything about a user or an org.
//
// DeploymentMode is what AuthGate reads at boot to choose between the local
// keychain-capture flow and the multi-mode cookie auth flow.
//
// DefaultGitHubHost is the deployment's GitHub (TF_DEFAULT_GITHUB_HOST, or
// github.com): the GitHub that "Sign in with GitHub" signs people in on, which
// the login page names when it is not github.com.
//
// Don't conflate with /api/settings (user-mutable preferences),
// /api/me (the caller's identity + org list), or
// POST /api/teams/{team_id}/members/list (the team roster).
type configResponse struct {
	DeploymentMode    string `json:"deployment_mode"`
	DefaultGitHubHost string `json:"default_github_host"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configResponse{
		DeploymentMode:    string(runmode.Current()),
		DefaultGitHubHost: ghbase.DefaultBaseURL(),
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
