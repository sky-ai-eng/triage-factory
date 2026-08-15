package githubapp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// TestListInstallations_HappyPath pins the App-JWT auth, the request
// shape, and the parse of GitHub's /app/installations array into the
// trimmed Installation struct.
func TestListInstallations_HappyPath(t *testing.T) {
	key := newTestKey(t)
	const appID int64 = 424242

	var (
		seenMethod string
		seenPath   string
		seenAuth   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")

		bearer := strings.TrimPrefix(seenAuth, "Bearer ")
		if _, err := jwt.Parse(bearer, func(*jwt.Token) (interface{}, error) {
			return &key.PublicKey, nil
		}, jwt.WithValidMethods([]string{"RS256"})); err != nil {
			http.Error(w, fmt.Sprintf("bad jwt: %v", err), http.StatusUnauthorized)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"id": 100, "account": {"login": "acme-eng", "type": "Organization"}, "created_at": "2026-01-02T03:04:05Z"},
			{"id": 200, "account": {"login": "solo-dev", "type": "User"}, "created_at": "2026-02-03T04:05:06Z"}
		]`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	insts, err := m.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}

	if seenMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", seenMethod)
	}
	if seenPath != "/app/installations" {
		t.Errorf("path = %q, want /app/installations", seenPath)
	}
	if !strings.HasPrefix(seenAuth, "Bearer ") {
		t.Errorf("authorization = %q; want Bearer <jwt>", seenAuth)
	}
	if len(insts) != 2 {
		t.Fatalf("got %d installations, want 2", len(insts))
	}
	if insts[0].ID != 100 || insts[0].AccountLogin != "acme-eng" || insts[0].AccountType != "Organization" {
		t.Errorf("inst[0] = %+v, want id=100 acme-eng/Organization", insts[0])
	}
	if insts[0].CreatedAt.IsZero() {
		t.Error("inst[0].CreatedAt is zero; want parsed timestamp")
	}
	if insts[1].ID != 200 || insts[1].AccountLogin != "solo-dev" || insts[1].AccountType != "User" {
		t.Errorf("inst[1] = %+v, want id=200 solo-dev/User", insts[1])
	}
}

// TestListInstallations_FollowsPagination pins that a Link: rel="next"
// header is followed so an App spanning more than one page is returned
// in full.
func TestListInstallations_FollowsPagination(t *testing.T) {
	key := newTestKey(t)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			// First page: point rel="next" at page 2 on this same server.
			w.Header().Set("Link", fmt.Sprintf(`<%s/app/installations?page=2>; rel="next", <%s/app/installations?page=2>; rel="last"`, srv.URL, srv.URL))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id": 1, "account": {"login": "page-one", "type": "User"}}]`))
		case "2":
			// Last page: no Link header.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id": 2, "account": {"login": "page-two", "type": "User"}}]`))
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      1,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	insts, err := m.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("got %d installations across pages, want 2", len(insts))
	}
	if insts[0].AccountLogin != "page-one" || insts[1].AccountLogin != "page-two" {
		t.Errorf("logins = %q, %q; want page-one, page-two", insts[0].AccountLogin, insts[1].AccountLogin)
	}
}

// TestListInstallations_HTTPError pins that a non-200 surfaces an error
// with the status + truncated body rather than a silent empty slice.
func TestListInstallations_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: newTestKey(t),
		AppID:      1,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	if _, err := m.ListInstallations(context.Background()); err == nil {
		t.Fatal("ListInstallations on 403 = nil err; want error")
	}
}

// TestListInstallations_CapturesAccountID pins that the account's numeric id
// survives the parse alongside its login. The two are not interchangeable: the
// login is what the account is currently called and the id is which account it
// is, and only the second is stable across a rename — so dropping the id here
// would leave the mirror with no durable way to identify what it is holding.
// A payload that omits it (older GHES, a trimmed fixture) yields 0, which
// callers map to "unknown" rather than to an account.
func TestListInstallations_CapturesAccountID(t *testing.T) {
	key := newTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"id": 100, "account": {"id": 1234, "login": "acme-eng", "type": "Organization"}, "created_at": "2026-01-02T03:04:05Z"},
			{"id": 200, "account": {"login": "solo-dev", "type": "User"}, "created_at": "2026-02-03T04:05:06Z"}
		]`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      424242,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	insts, err := m.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("got %d installations, want 2", len(insts))
	}
	if insts[0].AccountID != 1234 {
		t.Errorf("inst[0].AccountID = %d, want 1234", insts[0].AccountID)
	}
	if insts[1].AccountID != 0 {
		t.Errorf("inst[1].AccountID = %d for a payload without one, want 0", insts[1].AccountID)
	}
}

// TestListInstallations_CapturesSuspension pins the reconcile's half of
// installation suspension. GitHub does not re-deliver a missed
// installation.suspend, and local mode receives no deliveries at all, so this
// listing is what converges a suspended installation in either case — and both
// fields are nullable on the wire, so the null and the absent form must read
// back as "not suspended" rather than as a parse failure.
func TestListInstallations_CapturesSuspension(t *testing.T) {
	key := newTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"id": 100, "account": {"login": "acme-eng", "type": "Organization"},
			 "suspended_at": "2026-08-15T12:00:00Z", "suspended_by": {"login": "octocat", "id": 583231}},
			{"id": 200, "account": {"login": "solo-dev", "type": "User"}, "suspended_at": null, "suspended_by": null},
			{"id": 300, "account": {"login": "third-co", "type": "Organization"}}
		]`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      424242,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	insts, err := m.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}
	if len(insts) != 3 {
		t.Fatalf("got %d installations, want 3", len(insts))
	}
	want := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	if !insts[0].SuspendedAt.Equal(want) {
		t.Errorf("inst[0].SuspendedAt = %s, want %s", insts[0].SuspendedAt, want)
	}
	if insts[0].SuspendedBy != "octocat" {
		t.Errorf("inst[0].SuspendedBy = %q, want %q", insts[0].SuspendedBy, "octocat")
	}
	for _, i := range []int{1, 2} {
		if !insts[i].SuspendedAt.IsZero() || insts[i].SuspendedBy != "" {
			t.Errorf("inst[%d] = (%s, %q) for an unsuspended installation, want (zero, \"\")",
				i, insts[i].SuspendedAt, insts[i].SuspendedBy)
		}
	}
}
