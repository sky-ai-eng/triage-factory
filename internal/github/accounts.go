package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Account is a GitHub user or bot account: GitHub's id for it, which a rename
// leaves alone, and the login it is currently called.
type Account struct {
	ID    string
	Login string
	// Type is GitHub's kind of account: AccountTypeUser, "Bot" or
	// "Organization".
	Type string
	// Name is the account's profile name, "" when the profile has none.
	Name string
}

// AccountTypeUser is Account.Type for a person's account, the only kind that
// can hold a signing key.
const AccountTypeUser = "User"

// GetAccountByLogin returns the account GitHub currently calls login
// (GET /users/{login}). A bot is addressed as "<slug>[bot]", which the path
// escapes. A login no account holds is a 404 *HTTPError (IsHTTP404).
func (c *Client) GetAccountByLogin(ctx context.Context, login string) (Account, error) {
	return c.getAccount(ctx, "/users/"+url.PathEscape(login))
}

// GetAccountByID returns the account GitHub's id names (GET /user/{id}), under
// whatever login it holds now. An id no account holds is a 404 *HTTPError.
func (c *Client) GetAccountByID(ctx context.Context, id string) (Account, error) {
	return c.getAccount(ctx, "/user/"+url.PathEscape(id))
}

func (c *Client) getAccount(ctx context.Context, path string) (Account, error) {
	data, err := c.Get(ctx, path)
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return Account{}, fmt.Errorf("parse account: %w", err)
	}
	if u.ID == 0 || u.Login == "" {
		return Account{}, fmt.Errorf("account %s: response carried no id or login", path)
	}
	return Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Type: u.Type, Name: u.Name}, nil
}

// AccountClient resolves a client on orgID's GitHub credential for reading
// accounts, which any of the org's credentials can: its PAT, or an
// installation token of its App. An App installed on several accounts has no
// single installation to name, so the first one on the org's host that mints a
// token answers.
func AccountClient(ctx context.Context, resolver Resolver, orgs db.OrgsStore, apps db.GitHubAppsStore, orgID string) (*Client, error) {
	client, err := resolver.ClientFor(ctx, orgID, "")
	if !errors.Is(err, ErrAmbiguousInstallation) {
		return client, err
	}
	set, serr := orgs.GetSettingsSystem(ctx, orgID)
	if serr != nil {
		return nil, fmt.Errorf("read github host: %w", serr)
	}
	installations, lerr := apps.ListInstallationsOnHostSystem(ctx, orgID, db.EffectiveGitHubHost(set.GitHubBaseURL))
	if lerr != nil {
		return nil, fmt.Errorf("list github app installations: %w", lerr)
	}
	for _, inst := range installations {
		var c *Client
		if c, err = resolver.ClientForInstallation(ctx, orgID, inst); err == nil {
			return c, nil
		}
	}
	return nil, err
}
