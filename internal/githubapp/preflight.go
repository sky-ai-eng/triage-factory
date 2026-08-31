package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// The deployment App learns its own identity rather than being told it.
//
// GET /app, authenticated with a JWT the App signs for itself, returns the
// slug, the client id and the owner — which is why the configuration is four
// vars and not six. So the preflight is not overhead bolted onto config
// loading: it is the only way those three values are known, and the permission
// assertion below rides along on a call that had to happen regardless.
//
// # Why the members permission is a hard gate
//
// `members` is the only ORGANIZATION permission the App requests, and two
// independent things hang off that one fact. GitHub restricts App installation
// on an org to owners only if the App requests organization permissions, so
// `members` is what stops a repo admin installing the deployment App. And it is
// the permission the bind ceremony's authority gate reads, when it asks GitHub
// whether the person completing an installation actually administers the
// account they installed on. Lose it in a scope-minimization pass and both go
// at once — silently, since neither failure announces itself. Hence a refusal
// here rather than a warning: without it the deployment App must not serve
// anyone.
//
// # Not at boot
//
// This is a network call, and a GitHub blip must never refuse to start the
// deployment. It belongs where the managed credential class is enabled or first
// used, with its answer cached beside the App metadata; an unknown answer
// refuses that operation, never the process.

var (
	// ErrDeploymentAppKeyRejected reports that the configured App ID and
	// private key are not a usable pair: GitHub answered 401, which is exactly
	// what it answers a JWT whose iss does not match the signing key.
	//
	// The other way a JWT fails to mint — an ID that is not a number, a key
	// that is not a key — never reaches here: DeploymentAppFromEnv refuses both
	// at construction, so a Minter that exists at all is built from a parsed
	// key and a positive id, and what is left for GitHub to reject is the
	// pairing.
	ErrDeploymentAppKeyRejected = errors.New("githubapp: GitHub rejected the deployment App's ID and private key")

	// ErrDeploymentAppUnreachable reports that GitHub did not answer the
	// identity read — a transport failure, or any non-2xx that is not the 401
	// above. It is a statement about GitHub, not about the configuration, and
	// callers should treat it as "no answer yet" rather than as a verdict.
	ErrDeploymentAppUnreachable = errors.New("githubapp: could not read the deployment App's identity from GitHub")

	// ErrDeploymentAppMembersPermission reports an App whose granted
	// permissions do not include organization `members`. See above for why
	// this refuses rather than warns.
	ErrDeploymentAppMembersPermission = errors.New("githubapp: the deployment App is not granted the organization members permission")
)

// membersPermission is the organization permission the authority gate needs and
// the installer restriction depends on.
const membersPermission = "members"

// DeploymentAppIdentity is what the deployment App turns out to be: the
// identity GET /app reports, plus the granted level on the permission the
// preflight gates on.
//
// AppID is GitHub's own id for the App, which is worth carrying even though the
// operator supplied one: it is the id GitHub answers under, so a caller that
// wants to check the configured id against it can.
type DeploymentAppIdentity struct {
	AppID             int64
	Slug              string
	ClientID          string
	OwnerLogin        string
	OwnerType         string
	MembersPermission string
}

// PreflightDeploymentApp resolves the deployment App's identity and returns it
// with a verdict: nil error means this App may serve the managed credential
// class, and a non-nil one names which of the three failures happened, since
// they want different things from the operator.
//
//	ErrDeploymentAppKeyRejected        fix TF_GITHUB_APP_ID / _PRIVATE_KEY
//	ErrDeploymentAppUnreachable        wait, or check the API base / network
//	ErrDeploymentAppMembersPermission  grant Members on the App, then re-accept
//
// m is a Minter built from the config (DeploymentApp.Minter). Nothing here
// writes, mints an installation token, or reads a database.
func PreflightDeploymentApp(ctx context.Context, m *Minter) (DeploymentAppIdentity, error) {
	if m == nil {
		return DeploymentAppIdentity{}, errors.New("githubapp: preflight needs a minter built from the deployment App config")
	}

	app, err := m.GetApp(ctx)
	if err != nil {
		var status *APIStatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusUnauthorized {
			return DeploymentAppIdentity{}, fmt.Errorf(
				"%w: check that %s names the App the key in %s belongs to: %w",
				ErrDeploymentAppKeyRejected, envDeploymentAppID, envDeploymentAppPrivateKey, err)
		}
		return DeploymentAppIdentity{}, fmt.Errorf("%w: %w", ErrDeploymentAppUnreachable, err)
	}

	granted := app.Permissions[membersPermission]
	if !membersPermissionSatisfied(granted) {
		return DeploymentAppIdentity{}, fmt.Errorf(
			"%w: GET /app reports members=%q; grant the App the organization \"Members\" permission at read (or write), and have each installing account accept the updated permissions",
			ErrDeploymentAppMembersPermission, granted)
	}

	return DeploymentAppIdentity{
		AppID:             app.ID,
		Slug:              app.Slug,
		ClientID:          app.ClientID,
		OwnerLogin:        app.OwnerLogin,
		OwnerType:         app.OwnerType,
		MembersPermission: granted,
	}, nil
}

// membersPermissionSatisfied reports whether a granted level on the `members`
// permission is enough for the authority gate, which only ever reads.
//
// read and write are the levels GitHub documents for it; admin is accepted as
// the superset it would be. Everything else — absent, empty, or a value this
// build does not recognise — fails, because a permission string we cannot rank
// is not evidence that the gate will work, and a wrong refusal costs a retry
// where a wrong acceptance is unrecoverable.
func membersPermissionSatisfied(granted string) bool {
	switch granted {
	case "read", "write", "admin":
		return true
	default:
		return false
	}
}
