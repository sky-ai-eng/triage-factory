package githubapp_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const (
	testWebhookSecret = "whsec-not-a-real-secret"
	testClientSecret  = "cs-not-a-real-secret"
)

// testPEM returns a freshly generated PKCS#1 private key as PEM text — the
// shape GitHub hands an operator when they generate an App key.
func testPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// setDeploymentAppEnv sets every deployment-App var through t.Setenv, so the
// process environment is restored when the test ends and nothing leaks into
// the next one. An empty value means "leave this one unset", which is how the
// partial-configuration cases are expressed.
func setDeploymentAppEnv(t *testing.T, appID, pemText, webhookSecret, clientSecret string) {
	t.Helper()
	for name, value := range map[string]string{
		"TF_GITHUB_APP_ID":             appID,
		"TF_GITHUB_APP_PRIVATE_KEY":    pemText,
		"TF_GITHUB_APP_WEBHOOK_SECRET": webhookSecret,
		"TF_GITHUB_APP_CLIENT_SECRET":  clientSecret,
	} {
		t.Setenv(name, value)
	}
}

// TestDeploymentAppFromEnv_LocalModeReadsNothing: a distributed local binary
// cannot ship a shared App key, so local mode returns the zero App even with
// every var set — the same posture as jira.DeploymentOAuthAppFromEnv.
func TestDeploymentAppFromEnv_LocalModeReadsNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	setDeploymentAppEnv(t, "424242", testPEM(t), testWebhookSecret, testClientSecret)

	app, err := githubapp.DeploymentAppFromEnv()
	if err != nil {
		t.Fatalf("DeploymentAppFromEnv: %v", err)
	}
	if app.Configured() {
		t.Fatalf("local mode returned a configured App: %s", app)
	}
	if app != (githubapp.DeploymentApp{}) {
		t.Fatalf("local mode returned %s, want the zero App", app)
	}
}

// TestDeploymentAppFromEnv_MultiPopulatesAndTrims: multi mode reads all four,
// and a stray newline or indent in a .env file does not poison the credential.
func TestDeploymentAppFromEnv_MultiPopulatesAndTrims(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	pemText := testPEM(t)
	setDeploymentAppEnv(t,
		"  424242\n",
		"\n"+pemText+"\n  ",
		"  "+testWebhookSecret+"\n",
		"\t"+testClientSecret+"\n",
	)

	app, err := githubapp.DeploymentAppFromEnv()
	if err != nil {
		t.Fatalf("DeploymentAppFromEnv: %v", err)
	}
	if !app.Configured() {
		t.Fatalf("Configured() = false for %s", app)
	}
	if app.AppID != 424242 {
		t.Errorf("AppID = %d, want 424242", app.AppID)
	}
	if app.PrivateKey == nil {
		t.Error("PrivateKey = nil, want the parsed key")
	}
	if app.WebhookSecret != testWebhookSecret {
		t.Errorf("WebhookSecret = %q, want the value with whitespace trimmed", app.WebhookSecret)
	}
	if app.ClientSecret != testClientSecret {
		t.Errorf("ClientSecret = %q, want the value with whitespace trimmed", app.ClientSecret)
	}
}

// TestDeploymentAppFromEnv_MultiUnconfigured: none of the four set is an
// ordinary deployment (every org brings its own App), not an error.
func TestDeploymentAppFromEnv_MultiUnconfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setDeploymentAppEnv(t, "", "", "", "")

	app, err := githubapp.DeploymentAppFromEnv()
	if err != nil {
		t.Fatalf("DeploymentAppFromEnv: %v", err)
	}
	if app.Configured() {
		t.Fatalf("Configured() = true with nothing set: %s", app)
	}
}

// TestDeploymentAppFromEnv_PartialIsAHardError: every strict subset of the four
// fails, and the message names precisely the vars that are missing — the state
// where an operator believes they have configured this and has not is the one
// state that must never boot.
func TestDeploymentAppFromEnv_PartialIsAHardError(t *testing.T) {
	pemText := testPEM(t)
	all := []string{"424242", pemText, testWebhookSecret, testClientSecret}
	names := []string{
		"TF_GITHUB_APP_ID",
		"TF_GITHUB_APP_PRIVATE_KEY",
		"TF_GITHUB_APP_WEBHOOK_SECRET",
		"TF_GITHUB_APP_CLIENT_SECRET",
	}

	// Every proper non-empty subset of the four: bitmask 1..14 (0 is nothing
	// set, 15 is all four, and both of those are valid configurations).
	for mask := 1; mask < 15; mask++ {
		var wantMissing []string
		values := make([]string, 4)
		for i := range names {
			if mask&(1<<i) != 0 {
				values[i] = all[i]
			} else {
				wantMissing = append(wantMissing, names[i])
			}
		}
		t.Run(fmt.Sprintf("missing_%s", strings.Join(wantMissing, "_")), func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			setDeploymentAppEnv(t, values[0], values[1], values[2], values[3])

			app, err := githubapp.DeploymentAppFromEnv()
			if err == nil {
				t.Fatalf("DeploymentAppFromEnv succeeded on a partial set: %s", app)
			}
			if app.Configured() {
				t.Errorf("returned a configured App alongside the error: %s", app)
			}
			for _, name := range wantMissing {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error does not name the missing %s: %v", name, err)
				}
			}
			// The PEM is one of the values under test; an error message is a
			// place a secret must never reach.
			if strings.Contains(err.Error(), "PRIVATE KEY-----") {
				t.Error("error echoed the private key")
			}
		})
	}
}

// TestDeploymentAppFromEnv_UnparseablePEM: the key is parsed when the config is
// built, so a malformed .env value is a configuration error the operator reads
// at once — not a mint failure surfacing hours later inside a delegated run.
func TestDeploymentAppFromEnv_UnparseablePEM(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setDeploymentAppEnv(t, "424242", "-----BEGIN RSA PRIVATE KEY-----\nnot base64 at all\n-----END RSA PRIVATE KEY-----",
		testWebhookSecret, testClientSecret)

	_, err := githubapp.DeploymentAppFromEnv()
	if err == nil {
		t.Fatal("DeploymentAppFromEnv accepted an unparseable PEM")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TF_GITHUB_APP_PRIVATE_KEY") {
		t.Errorf("error does not name the offending var: %v", err)
	}
	if !strings.Contains(msg, "PEM") {
		t.Errorf("error does not say the key would not parse: %v", err)
	}
}

// TestDeploymentAppFromEnv_AppIDMustBeAPositiveNumber: pasting the client id
// (or a negative number) into TF_GITHUB_APP_ID is the same class of typo as a
// broken PEM, and fails the same way.
func TestDeploymentAppFromEnv_AppIDMustBeAPositiveNumber(t *testing.T) {
	for _, appID := range []string{"Iv23liABCDEF", "0", "-7", "424242x"} {
		t.Run(appID, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			setDeploymentAppEnv(t, appID, testPEM(t), testWebhookSecret, testClientSecret)

			_, err := githubapp.DeploymentAppFromEnv()
			if err == nil {
				t.Fatalf("DeploymentAppFromEnv accepted App ID %q", appID)
			}
			if !strings.Contains(err.Error(), "TF_GITHUB_APP_ID") {
				t.Errorf("error does not name the offending var: %v", err)
			}
		})
	}
}

// TestDeploymentApp_Configured pins the all-four rule on the type itself, so a
// hand-built value (a test fixture, a future caller) cannot read as configured
// while missing a credential.
func TestDeploymentApp_Configured(t *testing.T) {
	key, err := githubapp.ParsePrivateKey([]byte(testPEM(t)))
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	full := githubapp.DeploymentApp{
		AppID:         424242,
		PrivateKey:    key,
		WebhookSecret: testWebhookSecret,
		ClientSecret:  testClientSecret,
	}
	if !full.Configured() {
		t.Fatal("Configured() = false for a complete App")
	}

	for name, mutate := range map[string]func(a *githubapp.DeploymentApp){
		"no app id":         func(a *githubapp.DeploymentApp) { a.AppID = 0 },
		"no key":            func(a *githubapp.DeploymentApp) { a.PrivateKey = nil },
		"no webhook secret": func(a *githubapp.DeploymentApp) { a.WebhookSecret = "" },
		"no client secret":  func(a *githubapp.DeploymentApp) { a.ClientSecret = "" },
	} {
		t.Run(name, func(t *testing.T) {
			partial := full
			mutate(&partial)
			if partial.Configured() {
				t.Errorf("Configured() = true with %s", name)
			}
			if _, err := partial.Minter(""); !errors.Is(err, githubapp.ErrNoDeploymentApp) {
				t.Errorf("Minter err = %v, want ErrNoDeploymentApp", err)
			}
		})
	}
}

// TestDeploymentApp_FormattingRedactsSecrets: the values must never reach a log
// line, so neither the default formatting verbs nor slog may render them. Two
// of the three secrets are strings a %v would print verbatim; the third is a
// key whose primes a struct dump would spill.
func TestDeploymentApp_FormattingRedactsSecrets(t *testing.T) {
	key, err := githubapp.ParsePrivateKey([]byte(testPEM(t)))
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	app := githubapp.DeploymentApp{
		AppID:         424242,
		PrivateKey:    key,
		WebhookSecret: testWebhookSecret,
		ClientSecret:  testClientSecret,
	}

	// %v and %+v are the verbs a careless log line reaches for, and both route
	// through String because the type is a Stringer.
	rendered := []string{
		fmt.Sprintf("%v", app),
		fmt.Sprintf("%+v", app),
		app.String(),
		app.LogValue().String(),
	}
	for _, got := range rendered {
		for _, secret := range []string{testWebhookSecret, testClientSecret, key.D.String()} {
			if strings.Contains(got, secret) {
				t.Errorf("rendering leaked a secret: %q", got)
			}
		}
		if !strings.Contains(got, "424242") {
			t.Errorf("rendering dropped the App ID, which is not secret and is what makes a log line useful: %q", got)
		}
	}
	// slog.Value must be the redacted string, not the struct.
	if _, ok := any(app).(slog.LogValuer); !ok {
		t.Error("DeploymentApp does not implement slog.LogValuer; a JSON handler would marshal it field by field")
	}
}

// TestDeploymentApp_Minter builds the minter the preflight takes, proving the
// config's (app id, key) pair is all a caller needs to reach GitHub as the
// deployment App.
func TestDeploymentApp_Minter(t *testing.T) {
	key, err := githubapp.ParsePrivateKey([]byte(testPEM(t)))
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	app := githubapp.DeploymentApp{
		AppID:         424242,
		PrivateKey:    key,
		WebhookSecret: testWebhookSecret,
		ClientSecret:  testClientSecret,
	}
	m, err := app.Minter("")
	if err != nil {
		t.Fatalf("Minter: %v", err)
	}
	if _, err := m.AppJWT(); err != nil {
		t.Fatalf("AppJWT off the deployment App's key: %v", err)
	}
	if _, err := app.Minter("http://example.com"); err == nil {
		t.Error("Minter accepted a cleartext non-loopback API base")
	}
}
