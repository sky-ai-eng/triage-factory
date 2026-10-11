package integrations

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// AdoptSavedJiraSite moves the Jira site a local install kept beside its
// service credential (legacyJiraURL) into org_settings.jira_base_url, the only
// place an org's Jira site is read from, and deletes the key. Boot runs it in
// local mode; once the key is gone it reads one secret and returns.
//
// With a service credential stored, the saved site becomes the org's site even
// where org_settings names another: the credential's client talked to the
// saved site, so that is the site it was issued for and the site the org's
// issues came from. With none stored the key is deleted and the setting left
// as it is.
//
// userID's own Jira token is keyed by site (jira.UserTokenKey) under the
// spelling the site had when the token was bound, which is the saved value
// with surrounding space and trailing slashes trimmed. Where that differs from
// the canonical site, which lowercases the scheme and host, the token moves to
// the canonical key, so a user who bound under a capitalised site stays
// connected.
func AdoptSavedJiraSite(ctx context.Context, secrets db.SecretStore, orgs db.OrgsStore, orgID, userID string) error {
	saved, err := secrets.GetSystem(ctx, orgID, legacyJiraURL)
	if err != nil {
		return fmt.Errorf("read the saved jira site: %w", err)
	}
	if saved == "" {
		return nil
	}
	if site, ok := jira.CanonicalHost(saved); ok {
		creds, err := LoadSystem(ctx, secrets, orgID)
		if err != nil {
			return fmt.Errorf("load the jira credential: %w", err)
		}
		if _, stored := JiraSystemConfig(creds, site); stored {
			orgSet, err := orgs.GetSettingsSystem(ctx, orgID)
			if err != nil {
				return fmt.Errorf("load org settings: %w", err)
			}
			if orgSet.JiraBaseURL != site {
				if _, err := orgs.SetSourceBaseURL(ctx, orgID, "jira", site); err != nil {
					return fmt.Errorf("store the jira site: %w", err)
				}
			}
		}
		if bound := strings.TrimRight(strings.TrimSpace(saved), "/"); bound != site {
			if err := moveUserJiraToken(ctx, secrets, orgID, userID, jira.UserTokenKey(bound), jira.UserTokenKey(site)); err != nil {
				return err
			}
		}
	}
	if _, err := secrets.DeleteSystemIfValue(ctx, orgID, legacyJiraURL, saved); err != nil {
		return fmt.Errorf("delete the saved jira site: %w", err)
	}
	return nil
}

// moveUserJiraToken moves userID's token from one key to another. A token
// already stored under to is newer than the one under from, so it is kept
// and the one under from is only deleted.
func moveUserJiraToken(ctx context.Context, secrets db.SecretStore, orgID, userID, from, to string) error {
	token, err := secrets.GetUserSystem(ctx, orgID, userID, from)
	if err != nil {
		return fmt.Errorf("read the user's jira token: %w", err)
	}
	if token == "" {
		return nil
	}
	cur, err := secrets.GetUserSystem(ctx, orgID, userID, to)
	if err != nil {
		return fmt.Errorf("read the user's jira token: %w", err)
	}
	if cur == "" {
		if err := secrets.PutUserSystem(ctx, orgID, userID, to, token, ""); err != nil {
			return fmt.Errorf("store the user's jira token: %w", err)
		}
	}
	if _, err := secrets.DeleteUserSystemIfValue(ctx, orgID, userID, from, token); err != nil {
		return fmt.Errorf("delete the user's jira token: %w", err)
	}
	return nil
}

// AdoptSavedGitHubHost gives a local org with no GitHub URL the one it was
// using, and deletes the key a local install kept its GitHub host under
// beside the PAT (legacyGitHubURL). An org's GitHub host is
// org_settings.github_base_url alone and nothing derives one, so an org that
// was on GitHub with the setting empty would otherwise lose it. Boot runs it
// in local mode; once the setting is set and the key is gone it changes
// nothing.
//
// With the setting empty, the saved URL becomes it when it is a valid GitHub
// URL. Without one, an org already using GitHub — a PAT, an App registration
// or installation, or repositories on the deployment's GitHub — was on the
// deployment's GitHub (ghbase.DefaultBaseURL), and that becomes it. An org
// using none of them is left with no URL, to set when it connects. A set
// setting is never changed: it is what every client already talked to.
func AdoptSavedGitHubHost(ctx context.Context, stores db.Stores, orgID string) error {
	saved, err := stores.Secrets.GetSystem(ctx, orgID, legacyGitHubURL)
	if err != nil {
		return fmt.Errorf("read the saved github url: %w", err)
	}
	orgSet, err := stores.Orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("load org settings: %w", err)
	}
	if orgSet.GitHubBaseURL == "" {
		host, ok := ghbase.NormalizeBaseURL(saved)
		if !ok {
			inUse, err := usingDefaultGitHub(ctx, stores, orgID)
			if err != nil {
				return err
			}
			if inUse {
				host = ghbase.DefaultBaseURL()
			}
		}
		if host != "" {
			if _, err := stores.Orgs.SetSourceBaseURL(ctx, orgID, "github", host); err != nil {
				return fmt.Errorf("store the github url: %w", err)
			}
		}
	}
	if saved != "" {
		if _, err := stores.Secrets.DeleteSystemIfValue(ctx, orgID, legacyGitHubURL, saved); err != nil {
			return fmt.Errorf("delete the saved github url: %w", err)
		}
	}
	return nil
}

// usingDefaultGitHub reports whether an org with no GitHub URL holds anything
// GitHub: a PAT, an App registration, an App installation, or repositories on
// the deployment's GitHub, which is where every one of them was used.
func usingDefaultGitHub(ctx context.Context, stores db.Stores, orgID string) (bool, error) {
	pat, err := stores.Secrets.GetSystem(ctx, orgID, KeyGitHubPAT)
	if err != nil {
		return false, fmt.Errorf("read the github pat: %w", err)
	}
	if pat != "" {
		return true, nil
	}
	app, err := stores.GitHubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("read the github app registration: %w", err)
	}
	if app != nil {
		return true, nil
	}
	insts, err := stores.GitHubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("read github app installations: %w", err)
	}
	if len(insts) > 0 {
		return true, nil
	}
	repos, err := stores.Repos.ListSystem(ctx, orgID, ghbase.DefaultBaseURL())
	if err != nil {
		return false, fmt.Errorf("read repositories: %w", err)
	}
	return len(repos) > 0, nil
}
