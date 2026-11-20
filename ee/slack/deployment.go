package slack

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The deployment's Slack app is operator config, kept in the environment for
// the same reason as the deployment GitHub App: a secret store is keyed by
// org, and a deployment secret has no org. The App ID is configured rather
// than derived because TF has to recognize the app before any org has
// installed it, and Slack reports an app's id only from an install or a token.
//
// The transport is not configured: Socket Mode is a setting on the Slack app
// itself, so whichever of the signing secret or the app-level token the
// operator holds says which mode the app is in. The three secret-bearing names
// are registered in secretenv.Secrets.
const (
	envSlackAppID         = "TF_SLACK_APP_ID"
	envSlackClientID      = "TF_SLACK_CLIENT_ID"
	envSlackClientSecret  = "TF_SLACK_CLIENT_SECRET"
	envSlackSigningSecret = "TF_SLACK_SIGNING_SECRET"
	envSlackAppToken      = "TF_SLACK_APP_TOKEN"
)

// deploymentApp is the deployment's own Slack app. The zero value is no
// deployment app, which is what local mode and a deployment whose orgs all
// bring their own app have.
type deploymentApp struct {
	AppID         string
	ClientID      string
	ClientSecret  string
	SigningSecret string
	AppToken      string
	Transport     string // transportEventsAPI or transportSocket
}

// Configured reports whether the deployment has a Slack app.
// deploymentAppFromEnv returns either a complete app or the zero one, so the
// App ID alone answers it.
func (d deploymentApp) Configured() bool { return d.AppID != "" }

// String renders the app with each secret reduced to whether it is set, so a
// stray %v never prints the client secret, the signing secret, or the app
// token.
func (d deploymentApp) String() string {
	return fmt.Sprintf("slack.deploymentApp{app_id:%s, client_id:%s, client_secret:%s, signing_secret:%s, app_token:%s, transport:%s}",
		d.AppID,
		d.ClientID,
		secretPresence(d.ClientSecret),
		secretPresence(d.SigningSecret),
		secretPresence(d.AppToken),
		d.Transport,
	)
}

// LogValue gives slog the same rendering. A JSON handler marshals a struct
// field by field and never consults String, so String alone would not cover
// it.
func (d deploymentApp) LogValue() slog.Value {
	return slog.StringValue(d.String())
}

func secretPresence(v string) string {
	if v == "" {
		return "unset"
	}
	return "set"
}

// deploymentAppFromEnv reads the deployment's Slack app from the environment,
// in multi mode only: a distributed local binary can ship no shared app.
//
// None of the five variables set is the zero app and no error. Anything else
// short of a complete, consistent set is an error, because each such state is
// one in which an operator believes they have configured the app and has not.
func deploymentAppFromEnv() (deploymentApp, error) {
	if runmode.Current() != runmode.ModeMulti {
		return deploymentApp{}, nil
	}

	d := deploymentApp{
		AppID:         strings.TrimSpace(os.Getenv(envSlackAppID)),
		ClientID:      strings.TrimSpace(os.Getenv(envSlackClientID)),
		ClientSecret:  strings.TrimSpace(secretenv.Get(envSlackClientSecret)),
		SigningSecret: strings.TrimSpace(secretenv.Get(envSlackSigningSecret)),
		AppToken:      strings.TrimSpace(secretenv.Get(envSlackAppToken)),
	}
	if d == (deploymentApp{}) {
		return deploymentApp{}, nil
	}

	var problems []string
	var missing []string
	for _, v := range []struct{ name, value string }{
		{envSlackAppID, d.AppID},
		{envSlackClientID, d.ClientID},
		{envSlackClientSecret, d.ClientSecret},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, "not set: "+strings.Join(missing, ", "))
	}
	switch {
	case d.SigningSecret != "" && d.AppToken != "":
		problems = append(problems, fmt.Sprintf("set %s for the Events API or %s for Socket Mode, not both",
			envSlackSigningSecret, envSlackAppToken))
	case d.SigningSecret == "" && d.AppToken == "":
		problems = append(problems, fmt.Sprintf("set %s for the Events API or %s for Socket Mode",
			envSlackSigningSecret, envSlackAppToken))
	}
	if d.AppID != "" && !strings.HasPrefix(d.AppID, "A") {
		problems = append(problems, fmt.Sprintf("%s must start with A: set it to the App ID from Basic Information, not the client ID (got %q)",
			envSlackAppID, d.AppID))
	}
	if d.AppToken != "" && !isAppLevelToken(d.AppToken) {
		// The value is a credential, so the message never echoes it.
		problems = append(problems, fmt.Sprintf("%s must start with xapp-: set it to an app-level token from Basic Information", envSlackAppToken))
	}
	if len(problems) > 0 {
		return deploymentApp{}, errors.New("slack: the deployment Slack app is misconfigured: " + strings.Join(problems, "; "))
	}

	d.Transport = transportSocket
	if d.SigningSecret != "" {
		d.Transport = transportEventsAPI
	}
	return d, nil
}

// installablePublicURL reports whether the deployment app can be installed
// behind publicURL: Slack refuses a redirect URL that is not HTTPS.
func installablePublicURL(publicURL string) bool {
	return strings.HasPrefix(publicURL, "https://")
}

// deploymentAppHandler serves GET /api/slack/deployment-app.
type deploymentAppHandler struct {
	app       deploymentApp
	publicURL func() string
	// insecureURLLogged makes the non-HTTPS public URL an Error once per
	// process rather than once per request.
	insecureURLLogged sync.Once
}

type deploymentAppView struct {
	AppID     string `json:"app_id"`
	Transport string `json:"transport"`
}

// GET /api/slack/deployment-app
//
// No org and no entitlement gate: the answer is a fact about the deployment,
// the same for every caller. A configured app is reported only when the
// deployment's public URL is HTTPS, because Slack refuses a redirect URL that
// is not, so an app behind one cannot be installed.
func (h *deploymentAppHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal || !h.app.Configured() {
		httpx.NotFound(w, "deployment Slack app")
		return
	}
	if !installablePublicURL(h.publicURL()) {
		h.insecureURLLogged.Do(func() {
			slackLog.Error("the deployment Slack app is configured but the deployment's public URL is not HTTPS, so no org can install it; set TF_PUBLIC_URL to an https:// URL",
				"app_id", h.app.AppID)
		})
		httpx.NotFound(w, "deployment Slack app")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, deploymentAppView{AppID: h.app.AppID, Transport: h.app.Transport})
}
