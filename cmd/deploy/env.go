package deploy

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/jwkinit"
	"github.com/sky-ai-eng/triage-factory/internal/envfile"
)

// generatedSecrets are the values init generates, each 32 random bytes as
// hex: hex because some are interpolated into URL-form DSNs and one into
// SeaweedFS's JSON identity, where base64's / + = would break the parse.
var generatedSecrets = []string{
	"POSTGRES_PASSWORD",
	"SUPABASE_AUTH_ADMIN_PASSWORD",
	"TF_AUTHENTICATOR_PASSWORD",
	"TF_SYSTEM_PASSWORD",
	"TF_SESSION_ENCRYPTION_KEY",
	"TF_COOKIE_SECRET",
	"TF_SECRET_ENCRYPTION_KEY",
	"TF_BLOB_ACCESS_KEY",
	"TF_BLOB_SECRET_KEY",
}

// fileForms are the secrets the bundled compose also accepts as a mounted
// file through <NAME>_FILE. An operator who moved one to a file has set it;
// writing a plain value beside the file would put back in .env the secret
// they took out.
var fileForms = map[string]bool{
	"TF_SESSION_ENCRYPTION_KEY":    true,
	"TF_COOKIE_SECRET":             true,
	"TF_SECRET_ENCRYPTION_KEY":     true,
	"TF_GOTRUE_SERVICE_ROLE_TOKEN": true,
}

// lossyIfReplaced are the generated values whose replacement on a running
// deployment loses something: Postgres keeps the password it was initialized
// with, the two encryption keys make what they encrypted unreadable, and new
// signing keys end every session.
var lossyIfReplaced = map[string]bool{
	"POSTGRES_PASSWORD":         true,
	"TF_SESSION_ENCRYPTION_KEY": true,
	"TF_SECRET_ENCRYPTION_KEY":  true,
	"GOTRUE_JWT_KEYS":           true,
}

// envValues reads a .env's assignments.
type envValues []byte

func (e envValues) get(key string) string {
	v, _ := envfile.Lookup(e, key)
	return strings.TrimSpace(v)
}

// has reports whether key has a value, in plain or file form.
func (e envValues) has(key string) bool {
	return e.get(key) != "" || (fileForms[key] && e.get(key+"_FILE") != "")
}

// inputFlags are init's flags for the values only the operator can supply.
type inputFlags struct {
	publicURL        string
	clientID         string
	clientSecretFile string
}

// input is one value only the operator can supply: the .env key, the flag
// that supplies it, and how init asks for it on a terminal.
type input struct {
	key      string
	flag     string
	prompt   string
	secret   bool
	validate func(string) error
}

var inputs = []input{
	{key: "TF_PUBLIC_URL", flag: "--public-url", prompt: "Public URL of this deployment (https://…)", validate: validatePublicURL},
	{key: "GH_CLIENT_ID", flag: "--github-client-id", prompt: "GitHub OAuth app client ID", validate: validateToken},
	{key: "GH_CLIENT_SECRET", flag: "--github-client-secret-file", prompt: "GitHub OAuth app client secret", secret: true, validate: validateToken},
}

// resolveInputs settles TF_PUBLIC_URL, GH_CLIENT_ID and GH_CLIENT_SECRET.
// What .env already has stands, and a flag naming a different value is
// refused rather than applied; a flag fills a missing value, and on a
// terminal init asks for the rest. It returns the values to add to .env.
func resolveInputs(h *host, env envValues, f inputFlags) (map[string]string, error) {
	given := map[string]string{"TF_PUBLIC_URL": f.publicURL, "GH_CLIENT_ID": f.clientID}
	if f.clientSecretFile != "" {
		raw, err := os.ReadFile(f.clientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("--github-client-secret-file: %w", err)
		}
		given["GH_CLIENT_SECRET"] = strings.TrimSpace(string(raw))
		if given["GH_CLIENT_SECRET"] == "" {
			return nil, fmt.Errorf("--github-client-secret-file: %s is empty", f.clientSecretFile)
		}
	}

	var (
		problems []string
		missing  []input
		values   = map[string]string{}
	)
	for _, in := range inputs {
		existing, flagValue := env.get(in.key), given[in.key]
		switch {
		case flagValue != "" && existing != "" && flagValue != existing:
			problems = append(problems, fmt.Sprintf("%s: .env already sets %s to a different value; init never changes a value .env has, so edit .env to change it", in.flag, in.key))
		case existing != "":
		case flagValue != "":
			if err := in.validate(flagValue); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", in.flag, err))
				continue
			}
			values[in.key] = flagValue
		default:
			missing = append(missing, in)
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	if len(missing) == 0 {
		return values, nil
	}
	if !h.terminal {
		flags := make([]string, len(missing))
		for i, in := range missing {
			flags[i] = in.flag
		}
		return nil, fmt.Errorf("missing %s (stdin is not a terminal to ask on)", strings.Join(flags, ", "))
	}

	lines := bufio.NewReader(h.stdin)
	for _, in := range missing {
		if in.key == "GH_CLIENT_ID" {
			publicURL := values["TF_PUBLIC_URL"]
			if publicURL == "" {
				publicURL = env.get("TF_PUBLIC_URL")
			}
			printOAuthInstructions(h, publicURL)
		}
		value, err := ask(h, lines, in)
		if err != nil {
			return nil, err
		}
		values[in.key] = value
	}
	return values, nil
}

// ask prompts for one input until it gets a valid value.
func ask(h *host, lines *bufio.Reader, in input) (string, error) {
	for {
		fmt.Fprintf(h.stdout, "%s: ", in.prompt)
		var (
			value string
			err   error
		)
		if in.secret {
			value, err = h.readSecret()
			fmt.Fprintln(h.stdout)
		} else {
			value, err = lines.ReadString('\n')
		}
		value = strings.TrimSpace(value)
		if err != nil && value == "" {
			return "", fmt.Errorf("read %s: %w", in.key, err)
		}
		if verr := in.validate(value); verr != nil {
			fmt.Fprintf(h.stdout, "  %v\n", verr)
			continue
		}
		return value, nil
	}
}

func printOAuthInstructions(h *host, publicURL string) {
	fmt.Fprintf(h.stdout, `
Users sign in through a GitHub OAuth app. Create one now at
https://github.com/settings/developers ("New OAuth App") with:
  Homepage URL:                %s
  Authorization callback URL:  %s/auth/v1/callback

`, publicURL, publicURL)
}

// validatePublicURL accepts an https origin, or http://localhost for a test
// stack: no path, no trailing slash, no query, fragment or credentials.
// GoTrue appends /auth/v1 to it and GitHub's callback must match it exactly,
// so anything else breaks sign-in in a way that is hard to trace back here.
func validatePublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	switch {
	case u.Scheme == "https" && u.Hostname() != "":
	case u.Scheme == "http" && u.Hostname() == "localhost":
	default:
		return fmt.Errorf("%q must start with https:// (or be http://localhost for a test stack)", raw)
	}
	switch {
	case u.User != nil:
		return fmt.Errorf("%q must not carry credentials", raw)
	case u.Path == "/":
		return fmt.Errorf("%q must not end with a slash", raw)
	case u.Path != "" || u.Opaque != "":
		return fmt.Errorf("%q must not have a path; serve the deployment at the root of its host", raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return fmt.Errorf("%q must not have a query or fragment", raw)
	}
	return nil
}

// validateToken accepts a non-empty value with no whitespace, which is what
// GitHub issues as a client ID or secret and what a .env line carries intact.
func validateToken(v string) error {
	if v == "" {
		return errors.New("must not be empty")
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return errors.New("must be a single word with no whitespace")
	}
	return nil
}

// envFill computes what init adds to .env: the operator's inputs, a fresh
// value for every generated secret .env lacks, and GoTrue's signing material
// where .env lacks it. Nothing .env already has is in the result: a new
// TF_SECRET_ENCRYPTION_KEY would make every stored credential unreadable,
// and a new POSTGRES_PASSWORD would lock TF out of its own database.
func envFill(env envValues, values map[string]string) ([]envfile.Var, error) {
	var vars []envfile.Var
	for _, in := range inputs {
		if v, ok := values[in.key]; ok {
			vars = append(vars, envfile.Var{Key: in.key, Value: v})
		}
	}
	for _, key := range generatedSecrets {
		if env.has(key) {
			continue
		}
		v, err := randomHex32()
		if err != nil {
			return nil, err
		}
		vars = append(vars, envfile.Var{Key: key, Value: v})
	}

	publicURL := values["TF_PUBLIC_URL"]
	if publicURL == "" {
		publicURL = env.get("TF_PUBLIC_URL")
	}
	keys := env.get("GOTRUE_JWT_KEYS")
	if keys == "" {
		// A fresh keypair, and the service-role token with it: a token .env
		// still holds was signed by a key that is gone and verifies nowhere.
		// A token kept in a file is the operator's to replace.
		m, err := jwkinit.Generate("", publicURL)
		if err != nil {
			return nil, err
		}
		vars = append(vars, envfile.Var{Key: "GOTRUE_JWT_KEYS", Value: m.Keys})
		if !env.has("GOTRUE_JWT_SECRET") {
			vars = append(vars, envfile.Var{Key: "GOTRUE_JWT_SECRET", Value: m.Secret})
		}
		if env.get("TF_GOTRUE_SERVICE_ROLE_TOKEN_FILE") == "" {
			vars = append(vars, envfile.Var{Key: "TF_GOTRUE_SERVICE_ROLE_TOKEN", Value: m.ServiceRoleToken})
		}
	} else {
		if !env.has("TF_GOTRUE_SERVICE_ROLE_TOKEN") {
			m, err := jwkinit.Generate(keys, publicURL)
			if err != nil {
				return nil, err
			}
			vars = append(vars, m.EnvVars()...)
		}
		if !env.has("GOTRUE_JWT_SECRET") {
			v, err := randomHex32()
			if err != nil {
				return nil, err
			}
			vars = append(vars, envfile.Var{Key: "GOTRUE_JWT_SECRET", Value: v})
		}
	}
	return vars, nil
}

func randomHex32() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}
