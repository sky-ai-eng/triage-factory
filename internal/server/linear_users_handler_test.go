package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

var (
	linearUsersListPath = linearOrgPath(runmode.LocalDefaultOrgID) + "/users/list"

	linearUserAda   = fixtureUUID("linear-user-ada")
	linearUserAdam  = fixtureUUID("linear-user-adam")
	linearUserGrace = fixtureUUID("linear-user-grace")
	linearUserBot   = fixtureUUID("linear-user-adabot")
	linearUserGhost = fixtureUUID("linear-user-ghost")
)

func linearUserPath(id string) string {
	return linearOrgPath(runmode.LocalDefaultOrgID) + "/users/" + id
}

// newServerWithLinearUsers is a connected Linear catalog whose workspace has
// Ada and Adam, Grace, who is deactivated, and an integration's app user.
func newServerWithLinearUsers(t *testing.T) (*Server, *linearCatalogFake) {
	t.Helper()
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	fake.SetUsers(
		linear.User{ID: linearUserAda, Name: "Ada Lovelace", DisplayName: "ada", Email: "ada@example.com", Active: true},
		linear.User{ID: linearUserAdam, Name: "Adam Smith", DisplayName: "asmith", Email: "adam@example.com", Active: true},
		linear.User{ID: linearUserGrace, Name: "Grace Hopper", DisplayName: "grace", Email: "grace@example.com"},
		linear.User{ID: linearUserBot, Name: "Adabot", DisplayName: "adabot", Active: true, App: true},
	)
	return s, fake
}

func linearUserIDsOf(items []linearUserJSON) string {
	ids := make([]string, 0, len(items))
	for _, u := range items {
		ids = append(ids, u.ID)
	}
	return strings.Join(ids, ",")
}

// The list matches the typed text against the name, the display name and the
// email, offers only active users, app users among them and marked so, pages
// through Linear's cursor, and never returns an email.
func TestLinearUsersList_FindsActiveUsersByNameDisplayNameOrEmail(t *testing.T) {
	s, _ := newServerWithLinearUsers(t)

	for q, want := range map[string]string{
		"ADA":              linearUserAda + "," + linearUserAdam + "," + linearUserBot,
		"asmith":           linearUserAdam,
		"ada@example":      linearUserAda,
		"grace":            "",
		"nobody-like-that": "",
		"":                 linearUserAda + "," + linearUserAdam + "," + linearUserBot,
		"  adam@example  ": linearUserAdam,
	} {
		page := httpxtest.DecodeList[linearUserJSON](t, doJSON(t, s, http.MethodPost, linearUsersListPath, map[string]any{"q": q}))
		if got := linearUserIDsOf(page.Items); got != want {
			t.Errorf("q %q = %s, want %s", q, got, want)
		}
	}

	rec := doJSON(t, s, http.MethodPost, linearUsersListPath, map[string]any{"q": "ada", "page_size": 1})
	if strings.Contains(rec.Body.String(), "example.com") {
		t.Errorf("the list returned an email: %s", rec.Body.String())
	}
	first := httpxtest.DecodeList[linearUserJSON](t, rec)
	if want := (linearUserJSON{ID: linearUserAda, Name: "Ada Lovelace", DisplayName: "ada", Active: true}); len(first.Items) != 1 || first.Items[0] != want {
		t.Fatalf("page 1 = %+v, want [%+v]", first.Items, want)
	}
	if first.TotalCount != nil || first.NextPageToken == "" {
		t.Fatalf("page 1 total_count = %v, next_page_token = %q; want null and a token", first.TotalCount, first.NextPageToken)
	}
	second := httpxtest.DecodeList[linearUserJSON](t, doJSON(t, s, http.MethodPost, linearUsersListPath,
		map[string]any{"q": "ada", "page_size": 1, "page_token": first.NextPageToken}))
	if got := linearUserIDsOf(second.Items); got != linearUserAdam || second.NextPageToken == "" {
		t.Fatalf("page 2 = %s (next %q), want Adam and a token", got, second.NextPageToken)
	}
	third := httpxtest.DecodeList[linearUserJSON](t, doJSON(t, s, http.MethodPost, linearUsersListPath,
		map[string]any{"q": "ada", "page_size": 1, "page_token": second.NextPageToken}))
	if want := (linearUserJSON{ID: linearUserBot, Name: "Adabot", DisplayName: "adabot", Active: true, App: true}); len(third.Items) != 1 || third.Items[0] != want || third.NextPageToken != "" {
		t.Errorf("page 3 = %+v (next %q), want [%+v] and no token", third.Items, third.NextPageToken, want)
	}

	assertOneFault(t, doJSON(t, s, http.MethodPost, linearUsersListPath,
		map[string]any{"q": "adam", "page_size": 1, "page_token": first.NextPageToken}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearUsersListPath, map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearUsersListPath, map[string]any{"email": "ada@example.com"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "email")
}

// A user is read by id, deactivated or not; an id Linear has no user for, and
// a path value that is not a Linear id, are both a 404, the second without
// asking Linear.
func TestLinearUserGet(t *testing.T) {
	s, fake := newServerWithLinearUsers(t)

	for id, want := range map[string]linearUserJSON{
		linearUserAda:   {ID: linearUserAda, Name: "Ada Lovelace", DisplayName: "ada", Active: true},
		linearUserGrace: {ID: linearUserGrace, Name: "Grace Hopper", DisplayName: "grace"},
	} {
		rec := doJSON(t, s, http.MethodGet, linearUserPath(id), nil)
		var got linearUserJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got != want {
			t.Errorf("GET %s = %d %+v, want 200 %+v; body=%s", id, rec.Code, got, want, rec.Body.String())
		}
	}

	assertOneFault(t, doJSON(t, s, http.MethodGet, linearUserPath(linearUserGhost), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	before := fake.Calls()
	for _, id := range []string{"ada", strings.ToUpper(linearUserAda)} {
		assertOneFault(t, doJSON(t, s, http.MethodGet, linearUserPath(id), nil),
			http.StatusNotFound, "NOT_FOUND", "")
	}
	if fake.Calls() != before {
		t.Error("a path id that is not a Linear id reached Linear")
	}
}
