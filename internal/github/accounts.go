package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// Account is a GitHub user or bot account: GitHub's id for it, which a rename
// leaves alone, and the login it is currently called.
type Account struct {
	ID    string
	Login string
}

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
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return Account{}, fmt.Errorf("parse account: %w", err)
	}
	if u.ID == 0 || u.Login == "" {
		return Account{}, fmt.Errorf("account %s: response carried no id or login", path)
	}
	return Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Login}, nil
}
