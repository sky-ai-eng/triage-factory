package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// DependabotAlertsAccess asks GitHub for one open Dependabot alert of
// owner/repo and reports which of the three answers it gave:
//
//   - 200: readable.
//   - 403 saying alerts are turned off on the repository (or unavailable on
//     an archived one): disabled.
//   - any other 403 from GitHub, or a 404: no permission. GitHub sends these
//     to an App without the Dependabot alerts permission, a classic token
//     without the security_events scope (which repo includes), a fine-grained
//     token without the Dependabot alerts permission, and a token whose
//     account may not see the repository's alerts. A GitHub Enterprise Server
//     without the endpoint answers 404 too.
//
// Anything else is an error and no answer: a rate limit comes back as
// *ErrRateLimited, and a 401, a 5xx, a transport failure or a 403 that did
// not come from GitHub (its body is not GitHub's JSON, so something in front
// of GitHub sent it) says nothing about this repository.
//
// GitHub's 403 is counted as an answered request rather than a refused
// credential (withForbiddenAsAnswer): the credential may lack this one
// permission and still do everything else TF asks of it.
func (c *Client) DependabotAlertsAccess(ctx context.Context, owner, repo string) (domain.DependabotAlertsAccess, error) {
	_, err := c.Get(withForbiddenAsAnswer(ctx), fmt.Sprintf("/repos/%s/%s/dependabot/alerts?state=open&per_page=1", owner, repo))
	if err == nil {
		return domain.DependabotAlertsReadable, nil
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		return "", err
	}
	switch {
	case he.StatusCode == http.StatusForbidden && he.Class == upstream.Auth:
		if dependabotAlertsTurnedOff(he.Body) {
			return domain.DependabotAlertsDisabled, nil
		}
		return domain.DependabotAlertsNoPermission, nil
	case he.StatusCode == http.StatusNotFound:
		return domain.DependabotAlertsNoPermission, nil
	}
	return "", err
}

type forbiddenIsAnswerKey struct{}

// withForbiddenAsAnswer marks the requests made under ctx as questions that
// GitHub answers with a 403. Its JSON 403 to one of them is counted as
// rejected, the class of every other answered 4xx, instead of auth, which the
// poll cycle's connection state and the fleet alert on failing orgs read as
// GitHub refusing the credential.
func withForbiddenAsAnswer(ctx context.Context) context.Context {
	return context.WithValue(ctx, forbiddenIsAnswerKey{}, true)
}

func forbiddenIsAnswer(ctx context.Context) bool {
	answer, _ := ctx.Value(forbiddenIsAnswerKey{}).(bool)
	return answer
}

// dependabotAlertsTurnedOff reports whether a 403 body is GitHub saying the
// repository has no alerts to read ("Dependabot alerts are disabled for this
// repository.", "Dependabot alerts are not available for archived
// repositories.") rather than refusing the credential. GitHub answers both
// with 403 and only the message tells them apart.
func dependabotAlertsTurnedOff(body string) bool {
	var e struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return false
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "dependabot alerts are disabled") ||
		strings.Contains(m, "not available for archived repositories")
}
