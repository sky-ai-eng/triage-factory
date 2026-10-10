package integrations

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
