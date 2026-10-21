package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

func linearConfigFor(url string) linear.Config {
	cfg := linear.APIKey("lin_api_test")
	cfg.Endpoint = url
	return cfg
}

// Not parallel: see TestCaptureGitHubIdentity.
func TestValidateLinear_AppUserIsLinearsFlag(t *testing.T) {
	// The email is no part of the answer: an app user is whoever Linear says
	// is one, whatever address it carries.
	cases := map[string]struct {
		email string
		app   string
		isApp bool
	}{
		"person":                      {"ada@example.com", "false", false},
		"app user":                    {"4b1b3139-483d-455e-beba-f02f2d5b49b4@oauthapp.linear.app", "true", true},
		"app user, any address":       {"bot@example.com", "true", true},
		"person, app-looking address": {"ada@oauthapp.linear.app", "false", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var query string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Query string `json:"query"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				query = body.Query
				_, _ = w.Write([]byte(`{"data":{
					"viewer":{"id":"u1","name":"TF","displayName":"tf","email":"` + tc.email + `","app":` + tc.app + `},
					"organization":{"id":"org-9","name":"Sky AI","urlKey":"sky"}}}`))
			}))
			t.Cleanup(srv.Close)

			user, org, err := ValidateLinear(context.Background(), linearConfigFor(srv.URL))
			if err != nil {
				t.Fatalf("ValidateLinear: %v", err)
			}
			if !strings.Contains(query, " app ") && !strings.Contains(query, " app}") {
				t.Errorf("whoami query %q does not select the viewer's app flag", query)
			}
			if user.IsApp != tc.isApp {
				t.Errorf("IsApp = %v for app=%s, want %v", user.IsApp, tc.app, tc.isApp)
			}
			if user.ID != "u1" || user.Email != tc.email {
				t.Errorf("user = %+v", user)
			}
			if *org != (LinearOrganization{ID: "org-9", Name: "Sky AI", URLKey: "sky"}) {
				t.Errorf("organization = %+v", org)
			}
		})
	}
}

// Not parallel: see TestCaptureGitHubIdentity.
func TestValidateLinear_RejectedIsNotUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required, not authenticated","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`))
	}))
	t.Cleanup(srv.Close)

	_, _, err := ValidateLinear(context.Background(), linearConfigFor(srv.URL))
	if !errors.Is(err, linear.ErrUnauthorized) {
		t.Fatalf("err = %v, want linear.ErrUnauthorized", err)
	}
	if errors.Is(err, ErrLinearUnreachable) {
		t.Errorf("a refused credential must not read as unreachable: %v", err)
	}
}

// Not parallel: it shortens the linear client's process-wide backoff.
func TestValidateLinear_TransportFailureIsUnreachable(t *testing.T) {
	linear.SetRetryBackoffForTest(t, time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, _, err := ValidateLinear(context.Background(), linearConfigFor(url))
	if !errors.Is(err, ErrLinearUnreachable) {
		t.Fatalf("err = %v, want ErrLinearUnreachable", err)
	}
	if errors.Is(err, linear.ErrUnauthorized) {
		t.Errorf("an unreachable host must not read as a refused credential: %v", err)
	}
}
