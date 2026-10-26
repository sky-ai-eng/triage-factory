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
	GitHubUserID string `json:"github_user_id"`
	Login        string `json:"login"`
}

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
	s.answerGitHubAccount(w, r, orgID, fmt.Sprintf("no GitHub account is called %q", login),
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
	s.answerGitHubAccount(w, r, orgID, fmt.Sprintf("no GitHub account has id %s", id),
		func(ctx context.Context, c *ghclient.Client) (ghclient.Account, error) {
			return c.GetAccountByID(ctx, id)
		})
}

// answerGitHubAccount runs read with the org's GitHub client and answers its
// account, a 404 naming notFoundMessage when GitHub has none, 409
// NOT_CONFIGURED when the org has no GitHub credential, and 502 when GitHub
// fails otherwise.
func (s *Server) answerGitHubAccount(w http.ResponseWriter, r *http.Request, orgID, notFoundMessage string,
	read func(context.Context, *ghclient.Client) (ghclient.Account, error)) {
	client, err := s.gitHubAccountClient(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			writeNotConfigured(w, "GitHub is not connected for this workspace")
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
		writeUpstreamGitHub(w, "could not read the account from GitHub", err)
		return
	}
	writeJSON(w, http.StatusOK, gitHubAccountJSON{GitHubUserID: account.ID, Login: account.Login})
}

// gitHubAccountClient resolves a client on the org's GitHub credential for
// reading accounts (ghclient.AccountClient).
func (s *Server) gitHubAccountClient(ctx context.Context, orgID string) (*ghclient.Client, error) {
	return ghclient.AccountClient(ctx, s.ghResolver, s.orgs, s.allStores.GitHubApps, orgID)
}
