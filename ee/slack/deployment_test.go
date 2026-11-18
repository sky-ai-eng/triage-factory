package slack

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const (
	testDeployAppID         = "A0DEPLOY01"
	testDeployClientID      = "1234.5678"
	testDeployClientSecret  = "client-secret-not-real"
	testDeploySigningSecret = "signing-secret-not-real"
	testDeployAppToken      = "xapp-1-not-real"
)

// setSlackDeploymentEnv sets all five variables through t.Setenv, so each is
// restored when the test ends. An empty value leaves that variable unset.
func setSlackDeploymentEnv(t *testing.T, appID, clientID, clientSecret, signingSecret, appToken string) {
	t.Helper()
	for name, value := range map[string]string{
		envSlackAppID:         appID,
		envSlackClientID:      clientID,
		envSlackClientSecret:  clientSecret,
		envSlackSigningSecret: signingSecret,
		envSlackAppToken:      appToken,
	} {
		t.Setenv(name, value)
	}
}

func TestDeploymentAppFromEnv_NothingSet(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setSlackDeploymentEnv(t, "", "", "", "", "")

	app, err := deploymentAppFromEnv()
	if err != nil {
		t.Fatalf("deploymentAppFromEnv: %v", err)
	}
	if app != (deploymentApp{}) || app.Configured() {
		t.Fatalf("app = %+v; want the zero app", app)
	}
}

func TestDeploymentAppFromEnv_FullSet(t *testing.T) {
	cases := []struct {
		name          string
		signingSecret string
		appToken      string
		wantTransport string
	}{
		{"events api", testDeploySigningSecret, "", transportEventsAPI},
		{"socket mode", "", testDeployAppToken, transportSocket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			setSlackDeploymentEnv(t, testDeployAppID, testDeployClientID, testDeployClientSecret, tc.signingSecret, tc.appToken)

			app, err := deploymentAppFromEnv()
			if err != nil {
				t.Fatalf("deploymentAppFromEnv: %v", err)
			}
			want := deploymentApp{
				AppID:         testDeployAppID,
				ClientID:      testDeployClientID,
				ClientSecret:  testDeployClientSecret,
				SigningSecret: tc.signingSecret,
				AppToken:      tc.appToken,
				Transport:     tc.wantTransport,
			}
			if app != want {
				t.Fatalf("app = %+v; want %+v", app, want)
			}
			if !app.Configured() {
				t.Fatal("Configured() = false for a full set")
			}
		})
	}
}

func TestDeploymentAppFromEnv_RequiredVariableMissing(t *testing.T) {
	cases := []struct {
		missing                       string
		appID, clientID, clientSecret string
	}{
		{envSlackAppID, "", testDeployClientID, testDeployClientSecret},
		{envSlackClientID, testDeployAppID, "", testDeployClientSecret},
		{envSlackClientSecret, testDeployAppID, testDeployClientID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.missing, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			setSlackDeploymentEnv(t, tc.appID, tc.clientID, tc.clientSecret, testDeploySigningSecret, "")

			app, err := deploymentAppFromEnv()
			if err == nil {
				t.Fatal("deploymentAppFromEnv returned no error")
			}
			if app != (deploymentApp{}) {
				t.Errorf("app = %+v; want the zero app with the error", app)
			}
			if !strings.Contains(err.Error(), tc.missing) {
				t.Errorf("error %q does not name %s", err, tc.missing)
			}
		})
	}
}

func TestDeploymentAppFromEnv_Invalid(t *testing.T) {
	cases := []struct {
		name                           string
		appID, signingSecret, appToken string
		wantInError                    string
		// notInError tells the two transport cases apart: each message
		// names both variables.
		notInError string
	}{
		{"both transport secrets", testDeployAppID, testDeploySigningSecret, testDeployAppToken, "not both", ""},
		{"neither transport secret", testDeployAppID, "", "", envSlackSigningSecret, "not both"},
		{"app id without A prefix", "1234.5678", testDeploySigningSecret, "", "not the client ID", ""},
		{"app token without xapp-", testDeployAppID, "", "xoxb-1-not-an-app-token", "xapp-", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			setSlackDeploymentEnv(t, tc.appID, testDeployClientID, testDeployClientSecret, tc.signingSecret, tc.appToken)

			app, err := deploymentAppFromEnv()
			if err == nil {
				t.Fatalf("deploymentAppFromEnv returned no error; app = %+v", app)
			}
			if app != (deploymentApp{}) {
				t.Errorf("app = %+v; want the zero app with the error", app)
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error %q does not contain %q", err, tc.wantInError)
			}
			if tc.notInError != "" && strings.Contains(err.Error(), tc.notInError) {
				t.Errorf("error %q contains %q", err, tc.notInError)
			}
		})
	}
}

// TestDeploymentAppFromEnv_AppTokenNotEchoed: the app token is a credential,
// so the error about its shape must not repeat it.
func TestDeploymentAppFromEnv_AppTokenNotEchoed(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setSlackDeploymentEnv(t, testDeployAppID, testDeployClientID, testDeployClientSecret, "", "xoxb-secret-value")

	_, err := deploymentAppFromEnv()
	if err == nil {
		t.Fatal("deploymentAppFromEnv returned no error")
	}
	if strings.Contains(err.Error(), "xoxb-secret-value") {
		t.Errorf("error %q echoes the token", err)
	}
}

func TestDeploymentAppFromEnv_Trims(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setSlackDeploymentEnv(t,
		"  "+testDeployAppID+"\n",
		"\t"+testDeployClientID+" ",
		"\n"+testDeployClientSecret+"\n",
		"",
		"  "+testDeployAppToken+"\n",
	)

	app, err := deploymentAppFromEnv()
	if err != nil {
		t.Fatalf("deploymentAppFromEnv: %v", err)
	}
	want := deploymentApp{
		AppID:        testDeployAppID,
		ClientID:     testDeployClientID,
		ClientSecret: testDeployClientSecret,
		AppToken:     testDeployAppToken,
		Transport:    transportSocket,
	}
	if app != want {
		t.Fatalf("app = %+v; want %+v", app, want)
	}
}

func TestDeploymentAppFromEnv_LocalModeReadsNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	setSlackDeploymentEnv(t, testDeployAppID, testDeployClientID, testDeployClientSecret, testDeploySigningSecret, "")

	app, err := deploymentAppFromEnv()
	if err != nil {
		t.Fatalf("deploymentAppFromEnv: %v", err)
	}
	if app != (deploymentApp{}) {
		t.Fatalf("app = %+v; want the zero app in local mode", app)
	}
}

// TestDeploymentApp_RedactsSecrets: fmt and both slog handlers render each
// secret as its presence, and keep the values that are not secrets.
func TestDeploymentApp_RedactsSecrets(t *testing.T) {
	app := deploymentApp{
		AppID:         testDeployAppID,
		ClientID:      testDeployClientID,
		ClientSecret:  testDeployClientSecret,
		SigningSecret: testDeploySigningSecret,
		AppToken:      testDeployAppToken,
		Transport:     transportEventsAPI,
	}

	var text, json bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("app", "app", app)
	slog.New(slog.NewJSONHandler(&json, nil)).Info("app", "app", app)
	renderings := map[string]string{
		"%v":        fmt.Sprintf("%v", app),
		"%+v":       fmt.Sprintf("%+v", app),
		"slog text": text.String(),
		"slog json": json.String(),
	}
	for name, out := range renderings {
		for _, secret := range []string{testDeployClientSecret, testDeploySigningSecret, testDeployAppToken} {
			if strings.Contains(out, secret) {
				t.Errorf("%s rendering %q carries %q", name, out, secret)
			}
		}
		for _, kept := range []string{testDeployAppID, testDeployClientID, transportEventsAPI} {
			if !strings.Contains(out, kept) {
				t.Errorf("%s rendering %q lacks %q", name, out, kept)
			}
		}
	}
}

// ---------- GET /api/slack/deployment-app ----------

func getDeploymentApp(t *testing.T, h *deploymentAppHandler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleGet(rec, httptest.NewRequest(http.MethodGet, "/api/slack/deployment-app", nil))
	return rec
}

func TestHandleDeploymentApp_Configured(t *testing.T) {
	cases := []struct {
		name      string
		transport string
	}{
		{"events api", transportEventsAPI},
		{"socket mode", transportSocket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			h := &deploymentAppHandler{
				app: deploymentApp{
					AppID:         testDeployAppID,
					ClientID:      testDeployClientID,
					ClientSecret:  testDeployClientSecret,
					SigningSecret: testDeploySigningSecret,
					AppToken:      testDeployAppToken,
					Transport:     tc.transport,
				},
				publicURL: func() string { return "https://tf.example" },
			}

			rec := getDeploymentApp(t, h)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			got := decodeBody[map[string]any](t, rec)
			want := map[string]any{"app_id": testDeployAppID, "transport": tc.transport}
			if len(got) != len(want) || got["app_id"] != want["app_id"] || got["transport"] != want["transport"] {
				t.Fatalf("body = %v; want exactly %v", got, want)
			}
			for _, secret := range []string{testDeployClientID, testDeployClientSecret, testDeploySigningSecret, testDeployAppToken} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("body %s carries %q", rec.Body.String(), secret)
				}
			}
		})
	}
}

func TestHandleDeploymentApp_NotFound(t *testing.T) {
	configured := deploymentApp{AppID: testDeployAppID, Transport: transportEventsAPI}
	cases := []struct {
		name      string
		mode      runmode.Mode
		app       deploymentApp
		publicURL string
	}{
		{"unconfigured", runmode.ModeMulti, deploymentApp{}, "https://tf.example"},
		{"empty public url", runmode.ModeMulti, configured, ""},
		{"http public url", runmode.ModeMulti, configured, "http://tf.example"},
		{"local mode", runmode.ModeLocal, configured, "https://tf.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			h := &deploymentAppHandler{app: tc.app, publicURL: func() string { return tc.publicURL }}

			rec := getDeploymentApp(t, h)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"NOT_FOUND"`) {
				t.Errorf("body = %s; want reason NOT_FOUND", rec.Body.String())
			}
		})
	}
}
