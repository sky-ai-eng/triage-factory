package githubapp

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
)

// The deployment App — one App key serving many workspaces — keeps its identity
// and key in the operator's environment rather than in the schema. It is
// operator config in the same class as the GoTrue signing material: a secret
// store is keyed (orgID, key) by construction and a deployment secret has no
// org to hang on, and the environment buys a property a table cannot — no API
// path, and no bug in an admin handler, can read or rewrite it. The Atlassian
// deployment app (jira.DeploymentOAuthAppFromEnv) is the precedent this
// mirrors, down to the multi-only gate.
//
// Four vars, not six. GET /app, authenticated with a JWT signed by the App's
// own key, already returns the App's slug, client id, owner and granted
// permissions — so the App tells us who it is, and the operator supplies only
// what GitHub will not hand back:
//
//	TF_GITHUB_APP_ID              signs the JWT that authenticates that call
//	TF_GITHUB_APP_PRIVATE_KEY     the signing key itself             (secret)
//	TF_GITHUB_APP_WEBHOOK_SECRET  GitHub never discloses it          (secret)
//	TF_GITHUB_APP_CLIENT_SECRET   GitHub never discloses it          (secret)
//
// The slug and the client id are derived, never configured — see
// PreflightDeploymentApp, which is where the App learns its own identity and
// where the members-permission assertion rides along on a call we were making
// regardless.
//
// The three secret-bearing names are registered in secretenv.Secrets, so each
// also accepts the NAME_FILE convention. That matters most for the private key:
// a PEM is multi-line, which a .env file carries badly and a mounted file
// carries natively.
const (
	envDeploymentAppID            = "TF_GITHUB_APP_ID"
	envDeploymentAppPrivateKey    = "TF_GITHUB_APP_PRIVATE_KEY"
	envDeploymentAppWebhookSecret = "TF_GITHUB_APP_WEBHOOK_SECRET"
	envDeploymentAppClientSecret  = "TF_GITHUB_APP_CLIENT_SECRET"
)

// deploymentAppEnvNames is the all-or-none set, in the order an operator reads
// them: the missing-variable error and the "set all of these" hint are built
// from this one list so they cannot drift apart.
var deploymentAppEnvNames = []string{
	envDeploymentAppID,
	envDeploymentAppPrivateKey,
	envDeploymentAppWebhookSecret,
	envDeploymentAppClientSecret,
}

// ErrNoDeploymentApp reports that this deployment has no deployment App
// configured. It is the ordinary state of a deployment whose orgs all bring
// their own App, and of every local-mode process — not a fault.
var ErrNoDeploymentApp = errors.New("githubapp: no deployment App configured")

// DeploymentApp is the deployment's own GitHub App credential: the identity it
// signs App JWTs under, plus the two secrets GitHub will not disclose.
//
// The private key is held parsed rather than as PEM text. ParsePrivateKey runs
// when the config is built, so a malformed key is a configuration error the
// operator sees immediately rather than a mint failure surfacing hours later
// inside a delegated run.
type DeploymentApp struct {
	// AppID is the App's numeric id — the "iss" claim on every App JWT, and
	// the reason the id cannot itself be derived from GET /app.
	AppID int64

	// PrivateKey is the App's RSA signing key, already parsed from its PEM.
	PrivateKey *rsa.PrivateKey

	// WebhookSecret verifies the signature on deliveries to the deployment's
	// receiver.
	WebhookSecret string

	// ClientSecret authenticates the OAuth code exchange that identifies the
	// person completing an installation.
	ClientSecret string
}

// Configured reports whether all four values are present. Construction is
// all-or-nothing — DeploymentAppFromEnv refuses a strict subset — so this
// answers "did the operator configure a deployment App at all", and never
// describes a half-configured one.
func (a DeploymentApp) Configured() bool {
	return a.AppID > 0 && a.PrivateKey != nil && a.WebhookSecret != "" && a.ClientSecret != ""
}

// Minter returns a Minter that signs App JWTs with this App's key, so the
// (app id, key) pair is spelled in one place rather than at every call site
// that needs to reach GitHub as the deployment App.
//
// apiBase is the REST endpoint root ("" for api.github.com, a GHES root
// otherwise). It is the caller's rather than a fifth env var because which
// GitHub this deployment talks to is already resolved elsewhere, and the App
// reports nothing about where it lives.
func (a DeploymentApp) Minter(apiBase string) (*Minter, error) {
	if !a.Configured() {
		return nil, ErrNoDeploymentApp
	}
	return NewMinter(Config{PrivateKey: a.PrivateKey, AppID: a.AppID, APIBase: apiBase})
}

// String renders the App without its secrets. Three of the four fields are
// credentials GitHub will not re-issue, so what a stray %v produces must never
// be the default struct formatting — which would print the client secret, the
// webhook secret, and the private key's primes.
func (a DeploymentApp) String() string {
	return fmt.Sprintf("githubapp.DeploymentApp{app_id:%d, private_key:%s, webhook_secret:%s, client_secret:%s}",
		a.AppID,
		fieldPresence(a.PrivateKey != nil),
		fieldPresence(a.WebhookSecret != ""),
		fieldPresence(a.ClientSecret != ""),
	)
}

// LogValue is the slog half of the same guarantee. A JSON handler marshals a
// struct field by field and would never consult String, so the Stringer alone
// would still leak there.
func (a DeploymentApp) LogValue() slog.Value {
	return slog.StringValue(a.String())
}

// fieldPresence renders a credential field as its presence, which is the whole
// of what a log line may say about one.
func fieldPresence(set bool) string {
	if set {
		return "set"
	}
	return "unset"
}

// DeploymentAppFromEnv reads the deployment GitHub App from the environment —
// but ONLY in multi mode. A distributed local binary cannot ship a shared App
// key, so local mode returns the zero App and its single org brings its own,
// exactly as jira.DeploymentOAuthAppFromEnv does. Whitespace is trimmed on
// every field so a stray newline in .env cannot poison a credential.
//
// None of the four set is the zero App and no error: a deployment whose orgs
// all bring their own App is an ordinary deployment. Three things are errors
// instead, because each is a state in which an operator believes they have set
// this up and has not — a strict subset of the four, an App ID that is not a
// positive number, and a private key that will not parse.
func DeploymentAppFromEnv() (DeploymentApp, error) {
	if runmode.Current() != runmode.ModeMulti {
		return DeploymentApp{}, nil
	}

	raw := map[string]string{
		envDeploymentAppID:            strings.TrimSpace(os.Getenv(envDeploymentAppID)),
		envDeploymentAppPrivateKey:    strings.TrimSpace(secretenv.Get(envDeploymentAppPrivateKey)),
		envDeploymentAppWebhookSecret: strings.TrimSpace(secretenv.Get(envDeploymentAppWebhookSecret)),
		envDeploymentAppClientSecret:  strings.TrimSpace(secretenv.Get(envDeploymentAppClientSecret)),
	}

	var missing []string
	for _, name := range deploymentAppEnvNames {
		if raw[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == len(deploymentAppEnvNames) {
		return DeploymentApp{}, nil
	}
	if len(missing) > 0 {
		return DeploymentApp{}, fmt.Errorf(
			"githubapp: the deployment App is partially configured; not set: %s. Set all of %s, or none of them",
			strings.Join(missing, ", "),
			strings.Join(deploymentAppEnvNames, ", "),
		)
	}

	// Parsed here rather than at first use, for the same reason as the PEM: an
	// App ID that is not a number is a typo in a .env file, and the operator
	// should read about it while looking at that file.
	appID, err := strconv.ParseInt(raw[envDeploymentAppID], 10, 64)
	if err != nil || appID <= 0 {
		return DeploymentApp{}, fmt.Errorf(
			"githubapp: %s must be the App's positive numeric id (the App ID, not the client id), got %q",
			envDeploymentAppID, raw[envDeploymentAppID])
	}

	// The wrapped error names the PEM problem without echoing any of the key.
	key, err := ParsePrivateKey([]byte(raw[envDeploymentAppPrivateKey]))
	if err != nil {
		return DeploymentApp{}, fmt.Errorf(
			"githubapp: %s is not a PEM private key (paste the whole .pem, BEGIN/END lines included): %w",
			envDeploymentAppPrivateKey, err)
	}

	return DeploymentApp{
		AppID:         appID,
		PrivateKey:    key,
		WebhookSecret: raw[envDeploymentAppWebhookSecret],
		ClientSecret:  raw[envDeploymentAppClientSecret],
	}, nil
}
