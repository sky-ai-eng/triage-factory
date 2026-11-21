package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// gitHubAccountJSON is a GitHub account as the account reads answer it:
// GitHub's id for it, which the GitHub predicates' identity filters
// (author_id_in and its siblings) take, and the login it is called now.
type gitHubAccountJSON struct {
	GitHubUserID string `json:"github_user_id" doc:"GitHub's id for the account, which the identity filters of a GitHub event handler's predicate take."`
	Login        string `json:"login" doc:"The account's current login."`
}

var opGitHubAccountByName = httpx.NewJSONOp[httpx.NoBody, gitHubAccountJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/github/users/by-name/{login}",
	ID:          "get_github_user_by_name",
	Summary:     "Look up a GitHub account by login",
	Description: "Asks the organization's GitHub host, through the organization's GitHub credential, for the account a login names, and returns its id and login.",
	PathParams: map[string]string{
		"org_id": "The organization's id.",
		"login":  "The account's login on the organization's GitHub host, with a [bot] suffix for a bot.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

var opGitHubAccountByID = httpx.NewJSONOp[httpx.NoBody, gitHubAccountJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/github/users/{github_user_id}",
	ID:          "get_github_user",
	Summary:     "Get a GitHub account",
	Description: "Asks the organization's GitHub host, through the organization's GitHub credential, for the account an id names, and returns its id and the login it has now.",
	PathParams: map[string]string{
		"org_id":         "The organization's id.",
		"github_user_id": "GitHub's id for the account.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// gitHubLoginPattern is the shape of a GitHub login as a path value: the
// characters a user or Enterprise login is made of, and a bot's "[bot]"
// suffix. It refuses what no account can be called (a slash, a space, an
// empty value) before a request reaches GitHub.
var gitHubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}(\[bot\])?$`)

// handleGitHubAccountByName resolves a login to the GitHub account it names on
// the org's GitHub, through the org's GitHub credential. It is how a caller
// turns a login into the account id a GitHub predicate's identity filters
// take; a login no account holds is a 404.
//
// GET /api/orgs/{org_id}/github/users/by-name/{login}
func (s *Server) handleGitHubAccountByName(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	login := r.PathValue("login")
	if !gitHubLoginPattern.MatchString(login) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "login is not a GitHub login", Field: "login",
		})
		return
	}
	s.answerGitHubAccount(w, r, opGitHubAccountByName, orgID, fmt.Sprintf("no GitHub account is called %q", login),
		func(ctx context.Context, c *ghclient.Client) (ghclient.Account, error) {
			return c.GetAccountByLogin(ctx, login)
		})
}

// handleGitHubAccountByID reads the GitHub account an id names, under the
// login it holds now: how a reader names an id a predicate stores for an
// account outside TF. An id no account holds is a 404.
//
// GET /api/orgs/{org_id}/github/users/{github_user_id}
func (s *Server) handleGitHubAccountByID(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	id := r.PathValue("github_user_id")
	if !domain.IsGitHubID(id) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "github_user_id must be a GitHub account id", Field: "github_user_id",
		})
		return
	}
	s.answerGitHubAccount(w, r, opGitHubAccountByID, orgID, fmt.Sprintf("no GitHub account has id %s", id),
		func(ctx context.Context, c *ghclient.Client) (ghclient.Account, error) {
			return c.GetAccountByID(ctx, id)
		})
}

// answerGitHubAccount runs read with the org's GitHub client and answers its
// account through op, a 404 naming notFoundMessage when GitHub has none, 409
// NOT_CONFIGURED when the org has no GitHub credential, and 502 when GitHub
// fails otherwise.
func (s *Server) answerGitHubAccount(w http.ResponseWriter, r *http.Request, op *httpx.JSONOp[httpx.NoBody, gitHubAccountJSON], orgID, notFoundMessage string,
	read func(context.Context, *ghclient.Client) (ghclient.Account, error)) {
	client, err := s.gitHubAccountClient(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			writeNotConfigured(w, "GitHub is not connected for this organization")
			return
		}
		internalError(w, "github-accounts", err)
		return
	}
	account, err := read(r.Context(), client)
	if err != nil {
		if ghclient.IsHTTP404(err) {
			httpx.WriteErrors(w, http.StatusNotFound, httpx.ErrorItem{Reason: httpx.ReasonNotFound, Message: notFoundMessage})
			return
		}
		httpx.WriteUpstreamUnavailable(w, "could not read the account from GitHub", err)
		return
	}
	op.Write(w, gitHubAccountJSON{GitHubUserID: account.ID, Login: account.Login})
}

// gitHubAccountClient resolves a client on the org's GitHub credential for
// reading accounts (ghclient.AccountClient).
func (s *Server) gitHubAccountClient(ctx context.Context, orgID string) (*ghclient.Client, error) {
	return ghclient.AccountClient(ctx, s.ghResolver, s.orgs, s.allStores.GitHubApps, orgID)
}
