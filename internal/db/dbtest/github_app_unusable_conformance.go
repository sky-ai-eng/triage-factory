package dbtest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubAppUnusableSeeder stages the rows the unusable-App suite needs. An App
// registration that the backfill can mint from spans three stores (the
// registration row, its PEM in the secret store, and the org's GitHub base
// URL), none of which the GitHubAppsStore under test can write alone, so each
// backend implements them against its own schema.
type GitHubAppUnusableSeeder struct {
	// Org inserts an org row and returns its ID.
	Org func(t *testing.T) string

	// App registers an active App with appID for orgID, stores pemPEM as its
	// private key under the registration's pem_ref, and points the org's GitHub
	// base URL at baseURL.
	App func(t *testing.T, orgID, appID, pemPEM, baseURL string)

	// SetUnusableSince overwrites the registration's unusable_since. The
	// suite uses it to put a known instant in the column, since two writes in
	// one test can land in the same clock second and a "the stamp held" check
	// against a value written a moment ago proves nothing.
	SetUnusableSince func(t *testing.T, orgID string, at time.Time)
}

// GitHubAppUnusableFactory is what a per-backend test file hands to
// RunGitHubAppUnusableConformance. Each call returns a fresh, isolated backend.
type GitHubAppUnusableFactory func(t *testing.T) (db.GitHubAppsStore, GitHubAppUnusableSeeder)

// fakeAppGitHub is the two App-JWT endpoints the backfill can reach, each
// answering a status the subtest sets. A 200 listing reports one installation.
type fakeAppGitHub struct {
	listStatus atomic.Int32
	appStatus  atomic.Int32
	appCalls   atomic.Int32
}

func newFakeAppGitHub(t *testing.T) (*fakeAppGitHub, string) {
	t.Helper()
	f := &fakeAppGitHub{}
	f.listStatus.Store(http.StatusOK)
	f.appStatus.Store(http.StatusOK)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/app/installations", func(w http.ResponseWriter, _ *http.Request) {
		status := int(f.listStatus.Load())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`[{"id": 11, "account": {"id": 501, "login": "acme", "type": "Organization"}, "created_at": "2026-01-01T00:00:00Z"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"message":"Integration not found"}`))
	})
	mux.HandleFunc("GET /api/v3/app", func(w http.ResponseWriter, _ *http.Request) {
		f.appCalls.Add(1)
		status := int(f.appStatus.Load())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"id": 777, "slug": "tf-test", "client_id": "Iv1.x", "owner": {"login": "acme", "type": "Organization"}, "permissions": {"members": "read"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"message":"Integration not found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func testAppPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate app key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// RunGitHubAppUnusableConformance is the shared assertion suite for the
// backfill's diagnosis of an App GitHub has stopped accepting — deleted on
// GitHub, or its key deleted there. What it pins, in both dialects:
//
//   - a listing refused with 404 or 401 is diagnosed by GET /app, and the
//     reason GET /app's status names is recorded and returned as a
//     *GitHubAppUnusableError;
//   - GET /app answering anything else records nothing, and a reason already
//     stored survives it — an outage is not evidence the App came back;
//   - the since-stamp holds while the reason repeats and restarts when it
//     changes;
//   - a successful listing clears the reason;
//   - none of the failure arms touches the installation mirror.
func RunGitHubAppUnusableConformance(t *testing.T, mk GitHubAppUnusableFactory) {
	t.Helper()
	ctx := context.Background()

	setup := func(t *testing.T) (db.GitHubAppsStore, GitHubAppUnusableSeeder, *fakeAppGitHub, string) {
		t.Helper()
		store, seed := mk(t)
		gh, base := newFakeAppGitHub(t)
		orgID := seed.Org(t)
		seed.App(t, orgID, "777", testAppPEM(t), base)
		return store, seed, gh, orgID
	}

	read := func(t *testing.T, store db.GitHubAppsStore, orgID string) domain.OrgGitHubApp {
		t.Helper()
		app, err := store.GetForOrgSystem(ctx, orgID)
		if err != nil || app == nil {
			t.Fatalf("GetForOrgSystem = %v, %v; want the registration", app, err)
		}
		return *app
	}

	activeInstallations := func(t *testing.T, store db.GitHubAppsStore, orgID string) int {
		t.Helper()
		insts, err := store.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		return len(insts)
	}

	for _, tc := range []struct {
		name       string
		listStatus int
		appStatus  int
		want       domain.GitHubAppUnusableReason
	}{
		{"deleted app", http.StatusNotFound, http.StatusNotFound, domain.GitHubAppMissing},
		{"rejected key", http.StatusUnauthorized, http.StatusUnauthorized, domain.GitHubAppKeyRejected},
		{"get app decides the reason", http.StatusNotFound, http.StatusUnauthorized, domain.GitHubAppKeyRejected},
	} {
		t.Run("records "+tc.name, func(t *testing.T) {
			store, _, gh, orgID := setup(t)
			if err := store.BackfillInstallationsFromAPI(ctx, orgID); err != nil {
				t.Fatalf("healthy backfill: %v", err)
			}

			gh.listStatus.Store(int32(tc.listStatus))
			gh.appStatus.Store(int32(tc.appStatus))
			err := store.BackfillInstallationsFromAPI(ctx, orgID)
			var unusable *db.GitHubAppUnusableError
			if !errors.As(err, &unusable) || unusable.Reason != tc.want {
				t.Fatalf("backfill error = %v; want a GitHubAppUnusableError with reason %q", err, tc.want)
			}

			app := read(t, store, orgID)
			if app.UnusableReason != tc.want || app.UnusableSince.IsZero() {
				t.Errorf("registration = reason %q since %v; want %q with a since-stamp", app.UnusableReason, app.UnusableSince, tc.want)
			}
			if !app.Unusable() {
				t.Error("Unusable() = false for a registration with a recorded reason")
			}
			if n := activeInstallations(t, store, orgID); n != 1 {
				t.Errorf("installation mirror holds %d active rows after a diagnosed failure; want the 1 it had", n)
			}
		})
	}

	t.Run("an answer that says nothing about the app records nothing", func(t *testing.T) {
		for _, appStatus := range []int{http.StatusOK, http.StatusInternalServerError, http.StatusForbidden} {
			store, _, gh, orgID := setup(t)
			gh.listStatus.Store(http.StatusNotFound)
			gh.appStatus.Store(int32(appStatus))

			err := store.BackfillInstallationsFromAPI(ctx, orgID)
			var unusable *db.GitHubAppUnusableError
			if err == nil || errors.As(err, &unusable) {
				t.Fatalf("GET /app %d: backfill error = %v; want the listing's own failure, undiagnosed", appStatus, err)
			}
			if app := read(t, store, orgID); app.Unusable() {
				t.Errorf("GET /app %d: recorded reason %q from an answer that is not about the app", appStatus, app.UnusableReason)
			}
		}
	})

	t.Run("a listing failure that is not a refusal never asks", func(t *testing.T) {
		store, _, gh, orgID := setup(t)
		gh.listStatus.Store(http.StatusInternalServerError)
		if err := store.BackfillInstallationsFromAPI(ctx, orgID); err == nil {
			t.Fatal("backfill against a 500 listing = nil; want its error")
		}
		if n := gh.appCalls.Load(); n != 0 {
			t.Errorf("GET /app asked %d times after a 500 listing; want 0", n)
		}
	})

	t.Run("a stored reason survives an undiagnosed failure", func(t *testing.T) {
		store, _, gh, orgID := setup(t)
		gh.listStatus.Store(http.StatusNotFound)
		gh.appStatus.Store(http.StatusNotFound)
		_ = store.BackfillInstallationsFromAPI(ctx, orgID)

		gh.appStatus.Store(http.StatusBadGateway)
		_ = store.BackfillInstallationsFromAPI(ctx, orgID)
		if app := read(t, store, orgID); app.UnusableReason != domain.GitHubAppMissing {
			t.Errorf("reason after an outage = %q; want %q kept", app.UnusableReason, domain.GitHubAppMissing)
		}
	})

	t.Run("the since-stamp holds for a repeated reason and restarts for a new one", func(t *testing.T) {
		store, seed, gh, orgID := setup(t)
		gh.listStatus.Store(http.StatusNotFound)
		gh.appStatus.Store(http.StatusNotFound)
		_ = store.BackfillInstallationsFromAPI(ctx, orgID)

		earlier := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		seed.SetUnusableSince(t, orgID, earlier)
		_ = store.BackfillInstallationsFromAPI(ctx, orgID)
		if got := read(t, store, orgID).UnusableSince; !got.Equal(earlier) {
			t.Errorf("since after a repeated reason = %v; want %v held", got, earlier)
		}

		gh.listStatus.Store(http.StatusUnauthorized)
		gh.appStatus.Store(http.StatusUnauthorized)
		_ = store.BackfillInstallationsFromAPI(ctx, orgID)
		app := read(t, store, orgID)
		if app.UnusableReason != domain.GitHubAppKeyRejected || !app.UnusableSince.After(earlier) {
			t.Errorf("after a new reason = %q since %v; want %q since after %v", app.UnusableReason, app.UnusableSince, domain.GitHubAppKeyRejected, earlier)
		}
	})

	t.Run("a successful listing clears the reason", func(t *testing.T) {
		store, _, gh, orgID := setup(t)
		gh.listStatus.Store(http.StatusNotFound)
		gh.appStatus.Store(http.StatusNotFound)
		_ = store.BackfillInstallationsFromAPI(ctx, orgID)

		gh.listStatus.Store(http.StatusOK)
		if err := store.BackfillInstallationsFromAPI(ctx, orgID); err != nil {
			t.Fatalf("recovered backfill: %v", err)
		}
		app := read(t, store, orgID)
		if app.Unusable() || !app.UnusableSince.IsZero() {
			t.Errorf("after a successful listing = reason %q since %v; want both cleared", app.UnusableReason, app.UnusableSince)
		}
		if n := activeInstallations(t, store, orgID); n != 1 {
			t.Errorf("installation mirror holds %d active rows after recovery; want 1", n)
		}
	})
}
