package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

func linearConfigFor(url string) linear.Config {
	cfg := linear.APIKey("lin_api_test")
	cfg.Endpoint = url
	return cfg
}

func TestValidateLinear_AppUserEmailRule(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		email string
		isApp bool
	}{
		"person":            {"ada@example.com", false},
		"app user":          {"4b1b3139-483d-455e-beba-f02f2d5b49b4@oauthapp.linear.app", true},
		"app user, cased":   {"4B1B3139@OAuthApp.Linear.App", true},
		"lookalike domain":  {"ada@oauthapp.linear.app.example.com", false},
		"subdomain of host": {"ada@x.oauthapp.linear.app", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":{
					"viewer":{"id":"u1","name":"TF","displayName":"tf","email":"` + tc.email + `"},
					"organization":{"id":"org-9","name":"Sky AI","urlKey":"sky"}}}`))
			}))
			t.Cleanup(srv.Close)

			user, org, err := ValidateLinear(context.Background(), linearConfigFor(srv.URL))
			if err != nil {
				t.Fatalf("ValidateLinear: %v", err)
			}
			if user.IsApp != tc.isApp {
				t.Errorf("IsApp = %v for %q, want %v", user.IsApp, tc.email, tc.isApp)
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

func TestValidateLinear_RejectedIsNotUnreachable(t *testing.T) {
	t.Parallel()
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

func TestEnvProvided_Linear(t *testing.T) {
	t.Setenv("TRIAGE_FACTORY_LINEAR_API_KEY", "")
	if slices.Contains(EnvProvided(), "linear") {
		t.Error("EnvProvided reports linear with no key in the environment")
	}
	t.Setenv("TRIAGE_FACTORY_LINEAR_API_KEY", "lin_api_env")
	if !slices.Contains(EnvProvided(), "linear") {
		t.Error("EnvProvided does not report linear with the key in the environment")
	}
	if !EnvProvidesKey("linear_api_key") {
		t.Error("EnvProvidesKey(linear_api_key) = false with the key in the environment")
	}
}
