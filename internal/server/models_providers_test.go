package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// teamModelsPath / teamProvidersPath address one team's catalog and the
// restriction that shapes it.
func teamModelsPath(teamID string) string    { return "/api/teams/" + teamID + "/models" }
func teamProvidersPath(teamID string) string { return teamModelsPath(teamID) + "/providers" }

// modelKeyOn returns an offered model served by provider, read from the catalog
// so these tests exercise the real join rather than a spelling of their own.
func modelKeyOn(t *testing.T, provider string) string {
	t.Helper()
	for _, e := range modelcatalog.Entries() {
		if e.Provider == provider {
			return e.Key
		}
	}
	t.Fatalf("catalog offers no model on %s", provider)
	return ""
}

// connectOrgProviders marks the local org as holding the named credentials, by
// the settings refs that are the record of what is bound.
func connectOrgProviders(t *testing.T, s *Server, anthropic, bedrock bool) {
	t.Helper()
	ctx := context.Background()
	set, err := s.allStores.Orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("get org settings: %v", err)
	}
	if anthropic {
		set.AnthropicAPIKeyRef = secretKeyAnthropicAPIKey
	}
	if bedrock {
		set.BedrockCredentialsRef = "aws_bearer_token_bedrock"
	}
	if _, err := s.allStores.Orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("connect providers: %v", err)
	}
}

// providersOf collects the distinct providers a catalog read returned.
func providersOf(items []modelCatalogRow) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[it.Provider] = true
	}
	return out
}

// An unrestricted team sees the whole catalog; restricting it to one provider
// removes the other's entries from the picker's options entirely.
func TestTeamModelsList_OmitsRestrictedProviders(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, teamModelsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	all := decodeModels(t, rec.Body.Bytes()).Items
	if got, want := len(all), len(modelcatalog.Entries()); got != want {
		t.Fatalf("unrestricted team read returned %d models, want the whole catalog (%d)", got, want)
	}
	if !providersOf(all)[modelcatalog.ProviderBedrock] {
		t.Fatal("catalog read carries no Bedrock entry; the restriction case would prove nothing")
	}

	put := doJSON(t, s, http.MethodPut, teamProvidersPath("default"),
		map[string]any{"allowed_providers": []string{modelcatalog.ProviderAnthropic}})
	if put.Code != http.StatusOK {
		t.Fatalf("restrict: %d %s", put.Code, put.Body.String())
	}

	rec = doJSON(t, s, http.MethodGet, teamModelsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status after restriction = %d, want 200", rec.Code)
	}
	restricted := decodeModels(t, rec.Body.Bytes()).Items
	if len(restricted) == 0 {
		t.Fatal("restricted read returned nothing; the team can run no model at all")
	}
	seen := providersOf(restricted)
	if seen[modelcatalog.ProviderBedrock] {
		t.Errorf("Bedrock entries survived a restriction to Anthropic: %+v", restricted)
	}
	if !seen[modelcatalog.ProviderAnthropic] {
		t.Error("the allowed provider's entries are missing")
	}

	// Clearing the restriction restores the whole catalog — empty is the
	// unrestricted state, not "nothing allowed".
	clear := doJSON(t, s, http.MethodPut, teamProvidersPath("default"), map[string]any{"allowed_providers": []string{}})
	if clear.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", clear.Code, clear.Body.String())
	}
	rec = doJSON(t, s, http.MethodGet, teamModelsPath("default"), nil)
	if got, want := len(decodeModels(t, rec.Body.Bytes()).Items), len(modelcatalog.Entries()); got != want {
		t.Errorf("after clearing the restriction the team sees %d models, want %d", got, want)
	}
}

// The write echoes what it stored, and refuses a provider this build cannot
// drive rather than storing a restriction nothing would ever match.
func TestTeamProvidersPut_ValidatesAndEchoes(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	ok := doJSON(t, s, http.MethodPut, teamProvidersPath("default"),
		map[string]any{"allowed_providers": []string{modelcatalog.ProviderBedrock}})
	if ok.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", ok.Code, ok.Body.String())
	}
	var echo teamProvidersUpdate
	if err := json.Unmarshal(ok.Body.Bytes(), &echo); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if len(echo.AllowedProviders) != 1 || echo.AllowedProviders[0] != modelcatalog.ProviderBedrock {
		t.Errorf("echo = %v, want [%s]", echo.AllowedProviders, modelcatalog.ProviderBedrock)
	}

	for name, body := range map[string]any{
		"unknown provider": map[string]any{"allowed_providers": []string{"vertex"}},
		"empty entry":      map[string]any{"allowed_providers": []string{""}},
		"duplicate":        map[string]any{"allowed_providers": []string{modelcatalog.ProviderAnthropic, modelcatalog.ProviderAnthropic}},
		"unknown field":    map[string]any{"allowed_providers": []string{}, "nope": 1},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPut, teamProvidersPath("default"), body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}

	// The refusals changed nothing.
	rec := doJSON(t, s, http.MethodGet, teamModelsPath("default"), nil)
	if seen := providersOf(decodeModels(t, rec.Body.Bytes()).Items); seen[modelcatalog.ProviderAnthropic] {
		t.Error("a rejected write widened the stored restriction")
	}
}

// Save-time enforcement: a default whose provider this team is restricted from,
// or whose provider the org never connected, is refused with the field named.
// The stored default is untouched by the refusal.
func TestTeamSettingsPatch_ModelProviderMustBeAvailable(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()

	bedrockModel := modelKeyOn(t, modelcatalog.ProviderBedrock)

	t.Run("provider not connected", func(t *testing.T) {
		s := newTestServer(t)
		connectOrgProviders(t, s, true, false)

		rec := doJSON(t, s, http.MethodPatch, "/api/teams/default/settings", map[string]any{"ai_model": bedrockModel})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
		assertFieldError(t, rec, "ai_model")

		set, err := s.allStores.Teams.GetSettingsSystem(context.Background(), runmode.LocalDefaultTeamID)
		if err != nil {
			t.Fatalf("read team settings: %v", err)
		}
		if set.DefaultModel == bedrockModel {
			t.Error("the refused model was stored anyway")
		}
	})

	t.Run("provider restricted", func(t *testing.T) {
		s := newTestServer(t)
		connectOrgProviders(t, s, true, true)
		if rec := doJSON(t, s, http.MethodPut, teamProvidersPath("default"),
			map[string]any{"allowed_providers": []string{modelcatalog.ProviderAnthropic}}); rec.Code != http.StatusOK {
			t.Fatalf("restrict: %d %s", rec.Code, rec.Body.String())
		}

		rec := doJSON(t, s, http.MethodPatch, "/api/teams/default/settings", map[string]any{"ai_model": bedrockModel})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
		assertFieldError(t, rec, "ai_model")
	})

	t.Run("both connected and unrestricted saves", func(t *testing.T) {
		s := newTestServer(t)
		connectOrgProviders(t, s, true, true)

		rec := doJSON(t, s, http.MethodPatch, "/api/teams/default/settings", map[string]any{"ai_model": bedrockModel})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		set, err := s.allStores.Teams.GetSettingsSystem(context.Background(), runmode.LocalDefaultTeamID)
		if err != nil {
			t.Fatalf("read team settings: %v", err)
		}
		if set.DefaultModel != bedrockModel {
			t.Errorf("stored default = %q, want %q", set.DefaultModel, bedrockModel)
		}
	})
}

// assertFieldError checks the response carries one error naming field.
func assertFieldError(t *testing.T, rec *httptest.ResponseRecorder, field string) {
	t.Helper()
	var body struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode errors: %v (body: %s)", err, rec.Body.String())
	}
	for _, e := range body.Errors {
		if e.Field == field {
			return
		}
	}
	t.Errorf("no error names field %q: %s", field, rec.Body.String())
}

// TestTeamProviders_GatesAndScope_Postgres pins the multi-mode gates the local
// short-circuits hide: only an org admin may restrict a team (a team admin
// cannot widen their own), the write reaches a team the admin does not belong
// to, and the read is scoped to the caller's own org. Skips without Docker.
func TestTeamProviders_GatesAndScope_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	mdh := &modelsHandler{az: s.az, tx: s.tx}

	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, h, "providers-founder")
	teamB := pgtest.SeedTeam(t, h, orgID, "teamB")
	orgAdmin := pgtest.SeedUser(t, h, "providers-orgadmin")
	pgtest.AddOrgMember(t, h, orgAdmin, orgID, teamB, "admin", "member") // org admin, teamB only
	teamAdmin := pgtest.SeedUser(t, h, "providers-teamadmin")
	pgtest.AddOrgMember(t, h, teamAdmin, orgID, teamA, "member", "admin") // teamA admin, plain org member

	req := func(method, teamID string, body any) *http.Request {
		var r *http.Request
		if body == nil {
			r = httptest.NewRequest(method, "/api/models", nil)
		} else {
			enc, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			r = httptest.NewRequest(method, "/api/models", bytes.NewReader(enc))
			r.Header.Set("Content-Type", "application/json")
		}
		r.SetPathValue("team_id", teamID)
		return r
	}
	withCaller := func(r *http.Request, caller string) *http.Request {
		ctx := httpx.WithOrgID(r.Context(), orgID)
		ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
		return r.WithContext(ctx)
	}
	restrictBody := map[string]any{"allowed_providers": []string{modelcatalog.ProviderAnthropic}}

	t.Run("team_admin_cannot_restrict_their_own_team", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mdh.handleTeamProvidersPut(rec, withCaller(req(http.MethodPut, teamA, restrictBody), teamAdmin))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("team admin PUT = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_admin_restricts_a_team_they_are_not_in", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mdh.handleTeamProvidersPut(rec, withCaller(req(http.MethodPut, teamA, restrictBody), orgAdmin))
		if rec.Code != http.StatusOK {
			t.Fatalf("org admin PUT = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		// The restriction landed on teamA and shaped its read, on the Postgres
		// dialect as it does on SQLite.
		read := httptest.NewRecorder()
		mdh.handleTeamModelsList(read, withCaller(req(http.MethodGet, teamA, nil), teamAdmin))
		if read.Code != http.StatusOK {
			t.Fatalf("teamA read = %d, want 200 (body: %s)", read.Code, read.Body.String())
		}
		if providersOf(decodeModels(t, read.Body.Bytes()).Items)[modelcatalog.ProviderBedrock] {
			t.Error("Bedrock entries survived the restriction on Postgres")
		}
		// teamB is untouched: a restriction is per team, not per org.
		other := httptest.NewRecorder()
		mdh.handleTeamModelsList(other, withCaller(req(http.MethodGet, teamB, nil), orgAdmin))
		if other.Code != http.StatusOK {
			t.Fatalf("teamB read = %d, want 200 (body: %s)", other.Code, other.Body.String())
		}
		if !providersOf(decodeModels(t, other.Body.Bytes()).Items)[modelcatalog.ProviderBedrock] {
			t.Error("restricting teamA also narrowed teamB")
		}
	})

	t.Run("team_in_another_org_is_404", func(t *testing.T) {
		_, _, otherTeam := pgtest.SeedOrgWithUser(t, h, "providers-other-org")
		for name, call := range map[string]func(http.ResponseWriter, *http.Request){
			"read":  mdh.handleTeamModelsList,
			"write": mdh.handleTeamProvidersPut,
		} {
			rec := httptest.NewRecorder()
			r := req(http.MethodPut, otherTeam, restrictBody)
			if name == "read" {
				r = req(http.MethodGet, otherTeam, nil)
			}
			call(rec, withCaller(r, owner))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s of another org's team = %d, want 404 (body: %s)", name, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("archived_team_refuses_the_write", func(t *testing.T) {
		archived := pgtest.SeedTeam(t, h, orgID, "archived-team")
		pgtest.MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, archived)
		rec := httptest.NewRecorder()
		mdh.handleTeamProvidersPut(rec, withCaller(req(http.MethodPut, archived, restrictBody), orgAdmin))
		if rec.Code == http.StatusOK {
			t.Errorf("PUT on an archived team = 200, want a refusal (body: %s)", rec.Body.String())
		}
	})
}
