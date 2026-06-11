package server

import (
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// githubAppStatusResponse is the read-only shape the Workspace Settings
// "GitHub access" panel polls to render its state machine. `app` is null
// when the org has no registered App (the panel offers registration);
// `using_hosted_default` is true only when a deployment-default App
// covers the org — always false in local + self-host.
type githubAppStatusResponse struct {
	App                *githubAppInfo          `json:"app"`
	Installations      []githubAppInstallation `json:"installations"`
	UsingHostedDefault bool                    `json:"using_hosted_default"`
}

type githubAppInfo struct {
	AppID                   string `json:"app_id"`
	Slug                    string `json:"slug"`
	OwnerType               string `json:"owner_type"`
	RegisteredAt            string `json:"registered_at"`
	RegisteredByDisplayName string `json:"registered_by_display_name"`
}

type githubAppInstallation struct {
	InstallationID string `json:"installation_id"`
	AccountType    string `json:"account_type"`
	AccountLogin   string `json:"account_login"`
	InstalledAt    string `json:"installed_at"`
}

// handleGitHubAppStatus returns the org's GitHub App registration +
// installation state. Read-only; any org member (or local-mode user).
//
// GET /api/orgs/{org_id}/github-app
func (s *Server) handleGitHubAppStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var app *domain.OrgGitHubApp
	var insts []domain.OrgGitHubAppInstallation
	var registeredByName string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		insts, lerr = tx.GitHubApps.ListInstallationsForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		if app != nil && app.RegisteredByUserID != "" {
			// Best-effort: a missing/failed display-name lookup leaves the
			// field empty but shouldn't fail the whole status read.
			name, derr := tx.Users.GetDisplayName(r.Context(), app.RegisteredByUserID)
			if derr != nil {
				log.Printf("[github-app] display name for %s: %v", app.RegisteredByUserID, derr)
			}
			registeredByName = name
		}
		return nil
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}

	resp := githubAppStatusResponse{
		Installations:      make([]githubAppInstallation, 0, len(insts)),
		UsingHostedDefault: false,
	}
	if app != nil {
		resp.App = &githubAppInfo{
			AppID:                   app.AppID,
			Slug:                    app.Slug,
			OwnerType:               app.NormalizedOwnerType(),
			RegisteredAt:            app.RegisteredAt.UTC().Format(time.RFC3339),
			RegisteredByDisplayName: registeredByName,
		}
	}
	for _, inst := range insts {
		resp.Installations = append(resp.Installations, githubAppInstallation{
			InstallationID: inst.InstallationID,
			AccountType:    inst.AccountType,
			AccountLogin:   inst.AccountLogin,
			InstalledAt:    inst.InstalledAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGitHubAppInstallURL returns the GitHub deep-link the panel's
// "Install on another GitHub account" button opens. 404 when the org
// has no registered App (nothing to install). Read-only; any org member.
//
// GET /api/orgs/{org_id}/github-app/install-url
func (s *Server) handleGitHubAppInstallURL(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var app *domain.OrgGitHubApp
	var orgSettings domain.OrgSettings
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		orgSettings, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}

	ghBase := ghclient.ResolveBaseURL(orgSettings.GitHubBaseURL)
	writeJSON(w, http.StatusOK, map[string]string{
		"url": ghBase + "/apps/" + app.Slug + "/installations/new",
	})
}
