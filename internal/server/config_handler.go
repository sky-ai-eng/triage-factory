package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
	DeploymentMode    string `json:"deployment_mode" doc:"How this deployment runs: local for one person on their own machine, or multi for a shared deployment with sign-in."`
	DefaultGitHubHost string `json:"default_github_host" doc:"The base URL of the GitHub that Sign in with GitHub uses, such as https://github.com."`
}

var opConfig = httpx.NewJSONOp[httpx.NoBody, configResponse](httpx.OpSpec{
	Pattern:     "GET /api/config",
	ID:          "get_config",
	Summary:     "Get the deployment's configuration",
	Description: "Returns the deployment facts a client needs before anyone signs in: whether it runs in local or multi mode, and which GitHub its sign-in uses. It needs no credential and says nothing about any person or organization.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	opConfig.Write(w, configResponse{
		DeploymentMode:    string(runmode.Current()),
		DefaultGitHubHost: ghbase.DefaultBaseURL(),
	})
}

// healthResponse is the body of GET /api/health.
type healthResponse struct {
	Status string `json:"status" doc:"Always ok."`
}

var opHealth = httpx.NewJSONOp[httpx.NoBody, healthResponse](httpx.OpSpec{
	Pattern:     "GET /api/health",
	ID:          "get_health",
	Summary:     "Check that the server is up",
	Description: "Reports status ok once the server accepts requests. It needs no credential and checks no database or integration, so it says only that the process is serving.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleHealth is the liveness probe target. Returns 200 once the
// server is accepting requests; platform restart logic uses this to
// decide when a Machine/container has come up. Deliberately does NOT
// reach into the DB or integrations — a flapping Postgres shouldn't
// recycle the whole TF process via the platform's auto-restart.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	opHealth.Write(w, healthResponse{Status: "ok"})
}
