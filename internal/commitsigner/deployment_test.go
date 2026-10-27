package commitsigner

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// testDeploymentKey returns a fresh signing key as the PEM an operator
// configures, and its public half.
func testDeploymentKey(t *testing.T) (pem, pub string) {
	t.Helper()
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := commitsign.MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if pub, err = commitsign.PublicKey(key); err != nil {
		t.Fatal(err)
	}
	return string(raw), pub
}

func envOf(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

func parseDeployment(env map[string]string, host string) (*DeploymentSigner, error) {
	return ParseDeploymentSigner(envOf(env), envOf(env), host)
}

func TestParseDeploymentSigner_NoneIsNoSigner(t *testing.T) {
	got, err := parseDeployment(map[string]string{}, "https://github.com")
	if err != nil || got != nil {
		t.Fatalf("ParseDeploymentSigner with nothing set = %v, %v; want no signer and no error", got, err)
	}
	if got.Configured() {
		t.Error("a nil signer reports itself configured")
	}
}

func TestParseDeploymentSigner_Configured(t *testing.T) {
	pem, pub := testDeploymentKey(t)
	got, err := parseDeployment(map[string]string{
		EnvDeploymentSignerLogin: "  TriageFactory\n",
		EnvDeploymentSignerKey:   "\n" + pem + "\n",
	}, "https://GitHub.com/")
	if err != nil {
		t.Fatalf("ParseDeploymentSigner: %v", err)
	}
	if !got.Configured() || got.Login != "TriageFactory" || got.PublicKey != pub || got.Host != "https://github.com" || got.EmailOverride != "" {
		t.Errorf("signer = %v, want TriageFactory on github.com with the key's public half and the derived address", got)
	}
	row := domain.DeploymentCommitSigner{GitHubUserID: 90001, Login: "TriageFactory"}
	if email := got.CommitEmail(row); email != "90001+TriageFactory@users.noreply.github.com" {
		t.Errorf("CommitEmail = %q, want the id form of the noreply address", email)
	}
	if email := got.CommitEmail(domain.DeploymentCommitSigner{Login: "TriageFactory"}); email != "" {
		t.Errorf("CommitEmail before any check read the account = %q, want none", email)
	}

	withEmail, err := parseDeployment(map[string]string{
		EnvDeploymentSignerLogin: "TriageFactory",
		EnvDeploymentSignerKey:   pem,
		EnvDeploymentSignerEmail: "agent@triage.example",
	}, "https://github.com")
	if err != nil {
		t.Fatalf("ParseDeploymentSigner with an email: %v", err)
	}
	if email := withEmail.CommitEmail(row); email != "agent@triage.example" {
		t.Errorf("CommitEmail = %q, want the configured address", email)
	}
}

// TestParseDeploymentSigner_RefusesHalfAConfiguration walks every
// configuration an operator could believe signs and that would not.
func TestParseDeploymentSigner_RefusesHalfAConfiguration(t *testing.T) {
	pem, _ := testDeploymentKey(t)
	rsa := "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----"
	for _, tc := range []struct {
		name string
		env  map[string]string
		host string
		want string
	}{
		{"a login without a key", map[string]string{EnvDeploymentSignerLogin: "TriageFactory"}, "", "not set: " + EnvDeploymentSignerKey},
		{"a key without a login", map[string]string{EnvDeploymentSignerKey: pem}, "", "not set: " + EnvDeploymentSignerLogin},
		{"an email alone", map[string]string{EnvDeploymentSignerEmail: "agent@triage.example"}, "",
			"not set: " + EnvDeploymentSignerLogin + ", " + EnvDeploymentSignerKey},
		{"a bot's login", map[string]string{EnvDeploymentSignerLogin: "triage-factory[bot]", EnvDeploymentSignerKey: pem}, "", "is not a GitHub login"},
		{"a login with a space", map[string]string{EnvDeploymentSignerLogin: "Triage Factory", EnvDeploymentSignerKey: pem}, "", "is not a GitHub login"},
		{"a key that is no key", map[string]string{EnvDeploymentSignerLogin: "TriageFactory", EnvDeploymentSignerKey: "hunter2"}, "", "is not an unencrypted ed25519 private key"},
		{"an RSA key", map[string]string{EnvDeploymentSignerLogin: "TriageFactory", EnvDeploymentSignerKey: rsa}, "", "is not an unencrypted ed25519 private key"},
		{"an email with a name", map[string]string{
			EnvDeploymentSignerLogin: "TriageFactory", EnvDeploymentSignerKey: pem, EnvDeploymentSignerEmail: "Agent <agent@triage.example>",
		}, "", "is not a committer address"},
		{"GHES without an email", map[string]string{EnvDeploymentSignerLogin: "TriageFactory", EnvDeploymentSignerKey: pem},
			"https://ghe.acme.example", EnvDeploymentSignerEmail + " is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host
			if host == "" {
				host = "https://github.com"
			}
			got, err := parseDeployment(tc.env, host)
			if err == nil {
				t.Fatalf("ParseDeploymentSigner = %v, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to say %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "MIIB") {
				t.Errorf("error = %q echoes the key", err)
			}
		})
	}

	ghes, err := parseDeployment(map[string]string{
		EnvDeploymentSignerLogin: "TriageFactory", EnvDeploymentSignerKey: pem, EnvDeploymentSignerEmail: "agent@acme.example",
	}, "https://ghe.acme.example")
	if err != nil || ghes.Host != "https://ghe.acme.example" {
		t.Fatalf("GHES with an email = %v, %v; want a signer there", ghes, err)
	}
}

// TestDeploymentSigner_NeverPrintsTheKey pins that the rendering a stray %v or
// a log line produces names the key's presence and nothing of it.
func TestDeploymentSigner_NeverPrintsTheKey(t *testing.T) {
	pem, _ := testDeploymentKey(t)
	signer, err := parseDeployment(map[string]string{EnvDeploymentSignerLogin: "TriageFactory", EnvDeploymentSignerKey: pem}, "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("signer", "signer", signer, "value", *signer)
	seedHex := fmt.Sprintf("%x", signer.key.Seed())
	// The seed's first bytes as a default %v of a []byte prints them.
	var dec []string
	for _, b := range signer.key.Seed()[:6] {
		dec = append(dec, strconv.Itoa(int(b)))
	}
	seedBytes := strings.Join(dec, " ")
	for _, out := range []string{fmt.Sprintf("%v", signer), fmt.Sprintf("%+v", *signer), fmt.Sprint(*signer), logged.String()} {
		if !strings.Contains(out, "private_key:set") {
			t.Errorf("rendering %q does not say the key is set", out)
		}
		if strings.Contains(out, "OPENSSH") || strings.Contains(out, seedHex) || strings.Contains(out, seedBytes) {
			t.Errorf("rendering %q carries the private key", out)
		}
	}
}

func TestDeploymentSignerFromEnv_LocalModeHasNone(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	pem, _ := testDeploymentKey(t)
	t.Setenv(EnvDeploymentSignerLogin, "TriageFactory")
	t.Setenv(EnvDeploymentSignerKey, pem)
	got, err := DeploymentSignerFromEnv()
	if err != nil || got != nil {
		t.Fatalf("DeploymentSignerFromEnv in local mode = %v, %v; want none: local mode has no deployment App", got, err)
	}
}
