package commitsigner

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
)

// The deployment signer is operator config on the control pods, in the same
// class as the deployment App whose workspaces it signs for: an environment
// variable has no API path, so no handler can read or rewrite it, and a key
// with no org has no row in the org-keyed secret store to live in. The
// docker-compose.yml control service forwards all three and the executor
// service none, so no executor holds the key.
//
//	TF_GITHUB_COMMIT_SIGNER_LOGIN  the account's login; the checks follow it by id from the first read
//	TF_GITHUB_COMMIT_SIGNER_KEY    its unencrypted ed25519 private key, OpenSSH format (secret, honours _FILE)
//	TF_GITHUB_COMMIT_SIGNER_EMAIL  optional committer address; unset is the derived noreply address,
//	                               which exists only on github.com
const (
	EnvDeploymentSignerLogin = "TF_GITHUB_COMMIT_SIGNER_LOGIN"
	EnvDeploymentSignerKey   = "TF_GITHUB_COMMIT_SIGNER_KEY"
	EnvDeploymentSignerEmail = "TF_GITHUB_COMMIT_SIGNER_EMAIL"
)

// DeploymentSignerEnvNames are the three settings, in the order an operator
// reads them.
var DeploymentSignerEnvNames = []string{EnvDeploymentSignerLogin, EnvDeploymentSignerKey, EnvDeploymentSignerEmail}

// DeploymentSigner is the deployment's commit signer: the GitHub user account
// every workspace on the deployment App signs its agents' commits as unless it
// registers a signer of its own, and the private key that signs for it. A
// control pod builds it from its own config and keeps the signer's health in
// deployment_commit_signer, on the row for its key on its host.
//
// The private key is held parsed, so a malformed key refuses the boot rather
// than the first signature, and it is unexported: only this package signs with
// it, and String and LogValue never print it.
type DeploymentSigner struct {
	// Host is the GitHub the account is on: the deployment's default GitHub,
	// where the deployment App is registered.
	Host string
	// Login is the login the operator configured. The first check reads the
	// account it names, and every later read follows that account by its id.
	Login string
	// EmailOverride is the committer address the operator configured, ""
	// for the derived noreply address.
	EmailOverride string
	// PublicKey is the key's public half, "ssh-ed25519 AAAA…": the row the
	// signer's health is kept on, and the key the account must list.
	PublicKey string

	key ed25519.PrivateKey
}

// Configured reports whether d names a signer. A nil signer is none.
func (d *DeploymentSigner) Configured() bool {
	return d != nil && d.PublicKey != "" && len(d.key) == ed25519.PrivateKeySize
}

// CommitEmail is the committer address the signer's commits carry, for the
// account row names: the configured address, else on github.com the id form
// of the account's noreply address. "" off github.com without an address, and
// for a row whose account no check has read.
func (d *DeploymentSigner) CommitEmail(row domain.DeploymentCommitSigner) string {
	if d.EmailOverride != "" {
		return d.EmailOverride
	}
	return domain.NoreplyCommitEmail(d.Host, row.GitHubUserID, row.Login)
}

// String renders the signer without its key. A value receiver, so a stray %v
// of the struct itself renders this rather than the key's bytes.
func (d DeploymentSigner) String() string {
	return fmt.Sprintf("commitsigner.DeploymentSigner{host:%s, login:%s, email:%q, public_key:%s, private_key:%s}",
		d.Host, d.Login, d.EmailOverride, d.PublicKey, fieldPresence(len(d.key) > 0))
}

// LogValue is String for slog, whose JSON handler would otherwise marshal the
// struct field by field.
func (d DeploymentSigner) LogValue() slog.Value {
	return slog.StringValue(d.String())
}

func fieldPresence(set bool) string {
	if set {
		return "set"
	}
	return "unset"
}

// DeploymentSignerFromEnv reads the deployment signer from the environment,
// in multi mode only: local mode has no deployment App, so it has no signer
// either, and (nil, nil) is returned there whatever is set. The key is read
// through secretenv, so it honours TF_GITHUB_COMMIT_SIGNER_KEY_FILE. Call it
// after secretenv.Resolve and ghbase.InitDefaultBaseURLFromEnv.
func DeploymentSignerFromEnv() (*DeploymentSigner, error) {
	if runmode.Current() != runmode.ModeMulti {
		return nil, nil
	}
	return ParseDeploymentSigner(os.Getenv, secretenv.Get, ghbase.DefaultBaseURL())
}

// ParseDeploymentSigner reads the deployment signer through getenv, for the
// login and the email, and getSecret, for the key. host is the deployment's
// default GitHub. None of the three set is no signer and no error, an ordinary
// deployment. Whitespace around each value is trimmed.
//
// Every other incomplete or malformed configuration is an error, because each
// is one in which an operator believes managed workspaces sign and they would
// not: a login without a key or a key without a login, an email with neither,
// a login GitHub would not issue, a key that is not an unencrypted ed25519
// private key, an email git would not write unchanged, and no email for a
// host other than github.com, which has no noreply address to derive.
func ParseDeploymentSigner(getenv, getSecret func(string) string, host string) (*DeploymentSigner, error) {
	login := strings.TrimSpace(getenv(EnvDeploymentSignerLogin))
	rawKey := strings.TrimSpace(getSecret(EnvDeploymentSignerKey))
	email := strings.TrimSpace(getenv(EnvDeploymentSignerEmail))
	if login == "" && rawKey == "" && email == "" {
		return nil, nil
	}
	if login == "" || rawKey == "" {
		var missing []string
		if login == "" {
			missing = append(missing, EnvDeploymentSignerLogin)
		}
		if rawKey == "" {
			missing = append(missing, EnvDeploymentSignerKey)
		}
		return nil, fmt.Errorf("commitsigner: the deployment commit signer is partially configured; not set: %s. Set %s and %s (and optionally %s), or none of them",
			strings.Join(missing, ", "), EnvDeploymentSignerLogin, EnvDeploymentSignerKey, EnvDeploymentSignerEmail)
	}
	if !validLogin(login) {
		return nil, fmt.Errorf("commitsigner: %s=%q is not a GitHub login (letters, digits, hyphens and underscores; no [bot] suffix)",
			EnvDeploymentSignerLogin, login)
	}
	// The wrapped error names the problem without echoing any of the key.
	key, err := commitsign.ParsePrivateKey([]byte(rawKey))
	if err != nil {
		return nil, fmt.Errorf("commitsigner: %s is not an unencrypted ed25519 private key in OpenSSH format (generate one with ssh-keygen -t ed25519 -N ''): %w",
			EnvDeploymentSignerKey, err)
	}
	pub, err := commitsign.PublicKey(key)
	if err != nil {
		return nil, fmt.Errorf("commitsigner: derive the public half of %s: %w", EnvDeploymentSignerKey, err)
	}
	if email != "" {
		if problem := CommitEmailProblem(email); problem != "" {
			return nil, fmt.Errorf("commitsigner: %s=%q is not a committer address: %s", EnvDeploymentSignerEmail, email, problem)
		}
	}
	host = ghbase.CanonicalBaseURL(host)
	if email == "" && !domain.DerivesCommitEmail(host) {
		return nil, fmt.Errorf("commitsigner: %s is required: the deployment's GitHub is %s, which has no noreply address to commit under, so set it to an address verified on %s's account there",
			EnvDeploymentSignerEmail, host, login)
	}
	return &DeploymentSigner{Host: host, Login: login, EmailOverride: email, PublicKey: pub, key: key}, nil
}

// validLogin reports whether login is one GitHub issues to a user account:
// letters, digits, hyphens and, for Enterprise Managed Users, underscores. A
// bot's "<slug>[bot]" is refused, since a bot cannot hold a signing key.
func validLogin(login string) bool {
	if len(login) > 100 {
		return false
	}
	for _, r := range login {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return login != ""
}

// MaxCommitEmailLen bounds a committer address at the longest an address can
// be.
const MaxCommitEmailLen = 254

// CommitEmailProblem is why email cannot be a signer's committer address, ""
// when it can: a bare address, as git writes it between the angle brackets of
// the committer header, with no display name, surrounding space or brackets,
// and one git writes unchanged. A bare address can begin with an apostrophe,
// which git drops, and the commit would then name an address the signer never
// set.
func CommitEmailProblem(email string) string {
	if len(email) > MaxCommitEmailLen {
		return fmt.Sprintf("email must be at most %d characters", MaxCommitEmailLen)
	}
	if addr, err := mail.ParseAddress(email); err != nil || addr.Name != "" || addr.Address != email {
		return "email must be a bare email address, such as agent@example.com"
	}
	if commitsign.IdentName(email) != email {
		return "email must not begin with an apostrophe, which git drops from a committer address"
	}
	return ""
}
