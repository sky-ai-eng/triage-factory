package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetAccount reads an account by login and by id, escapes a bot's
// brackets, and reports an unknown login or id as a 404.
func TestGetAccount(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v3/users/Octocat", "/api/v3/user/583231":
			_, _ = w.Write([]byte(`{"id":583231,"login":"octocat","type":"User"}`))
		case "/api/v3/users/dependabot%5Bbot%5D":
			_, _ = w.Write([]byte(`{"id":49699333,"login":"dependabot[bot]","type":"Bot"}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	c := NewClient(ts.URL, "pat")
	ctx := context.Background()

	if got, err := c.GetAccountByLogin(ctx, "Octocat"); err != nil || got != (Account{ID: "583231", Login: "octocat"}) {
		t.Errorf("GetAccountByLogin(Octocat) = %+v, %v; want octocat 583231", got, err)
	}
	if got, err := c.GetAccountByLogin(ctx, "dependabot[bot]"); err != nil || got != (Account{ID: "49699333", Login: "dependabot[bot]"}) {
		t.Errorf("GetAccountByLogin(dependabot[bot]) = %+v, %v; want the bot's id", got, err)
	}
	if got, err := c.GetAccountByID(ctx, "583231"); err != nil || got != (Account{ID: "583231", Login: "octocat"}) {
		t.Errorf("GetAccountByID(583231) = %+v, %v; want octocat", got, err)
	}
	if _, err := c.GetAccountByLogin(ctx, "nobody"); !IsHTTP404(err) {
		t.Errorf("GetAccountByLogin(nobody) error = %v, want a 404", err)
	}
	if _, err := c.GetAccountByID(ctx, "1"); !IsHTTP404(err) {
		t.Errorf("GetAccountByID(1) error = %v, want a 404", err)
	}
}
