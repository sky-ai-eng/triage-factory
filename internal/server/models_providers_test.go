package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// teamModelsPath / teamProvidersPath address one team's catalog and the
// restriction that shapes it.
func teamModelsPath(teamID string) string    { return "/api/teams/" + teamID + "/models" }
func teamProvidersPath(teamID string) string { return teamModelsPath(teamID) + "/providers" }

// modelKeyOn returns a NATIVE model served by provider, read from the registry
// so these tests exercise the real join rather than a spelling of their own.
// Native, because the provider is a property of the id only in that vocabulary
// — an SDK alias resolves its access path from the credential.
func modelKeyOn(t *testing.T, provider string) string {
	t.Helper()
	for _, e := range modelcatalog.UniverseFor(true).Models() {
		if e.Provider == provider {
			return e.Key
		}
	}
	t.Fatalf("the native universe offers no model on %s", provider)
	return ""
}

// connectOrgProviders sets which credentials the local org holds, by the
// settings refs that are the record of what is bound. Both directions: a false
// clears the ref, which is what disconnecting a provider does.
func connectOrgProviders(t *testing.T, s *Server, anthropic, bedrock bool) {
	t.Helper()
	ctx := context.Background()
	set, err := s.allStores.Orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("get org settings: %v", err)
	}
	set.AnthropicAPIKeyRef = ""
	if anthropic {
		set.AnthropicAPIKeyRef = secretKeyAnthropicAPIKey
	}
	set.BedrockCredentialsRef = ""
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

// A provider restriction cannot narrow a LOCAL team's picker, and that is the
// decoupling working rather than the restriction failing: the models a local
// install offers are the harness's aliases, and an alias names no access path —
// the SDK resolves one from whichever credential its environment supplies. So
// there is nothing for a restriction over access paths to match on, and
// filtering by one would remove rows nobody asserted were served that way.
//
// The narrowing itself is a native-path behaviour and is pinned against
// Postgres in TestTeamProviders_GatesAndScope_Postgres.
func TestTeamModelsList_LocalAliasesAreNotNarrowedByAProviderRestriction(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, teamModelsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	all := decodeModels(t, rec.Body.Bytes()).Items
	want := len(modelcatalog.UniverseFor(false).Models())
	if len(all) != want {
		t.Fatalf("unrestricted team read returned %d models, want the whole local universe (%d)", len(all), want)
	}
	for _, row := range all {
		if row.Provider != "" {
			t.Errorf("%s: provider = %q, want absent on an alias", row.Key, row.Provider)
		}
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
	if got := len(decodeModels(t, rec.Body.Bytes()).Items); got != want {
		t.Errorf("restricted read returned %d models, want all %d — an alias names no provider to restrict", got, want)
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
		// dialect as it does on SQLite — for a member of the team, and for the
		// org admin who set it, who is not one. The second reader is the point:
		// a membership-gated read would answer them with the defaults and show
		// an unrestricted catalog seconds after they restricted it.
		for _, caller := range []string{teamAdmin, orgAdmin} {
			read := httptest.NewRecorder()
			mdh.handleTeamModelsList(read, withCaller(req(http.MethodGet, teamA, nil), caller))
			if read.Code != http.StatusOK {
				t.Fatalf("teamA read = %d, want 200 (body: %s)", read.Code, read.Body.String())
			}
			if providersOf(decodeModels(t, read.Body.Bytes()).Items)[modelcatalog.ProviderBedrock] {
				t.Errorf("Bedrock entries survived the restriction on Postgres (caller %s)", caller)
			}
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

// A manual delegation refused for its model's provider answers 422 with the
// refusal itself — the provider to connect and where. The 500 arm beside it
// redacts its message in multi mode, so landing there would strip exactly the
// text this refusal exists to deliver; the status is what distinguishes them.
//
// The model here is a step's pin, saved while its provider was connected and
// disconnected afterwards. That is the case no save-time check can catch, and
// the reason the dispatch gate exists.
func TestDelegate_ModelProviderUnavailable_SurfacesTheRemedy(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), domain.ModelSonnet))

	const (
		eventType   = "github:pr:opened"
		promptID    = "p-provider-refusal"
		blueprintID = "bp-provider-refusal"
		taskID      = "00000000-0000-4000-8000-0000000009c1"
	)
	bedrockModel := modelKeyOn(t, modelcatalog.ProviderBedrock)
	if _, err := s.db.Exec(
		`INSERT INTO prompts (id, name, body, model, source, creator_user_id) VALUES (?, 'Refusal', 'do the thing', ?, 'user', ?)`,
		promptID, bedrockModel, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, err := s.blueprints.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: blueprintID, Name: "Refusal", Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id) VALUES (?, 0, ?)`,
		blueprintID, promptID); err != nil {
		t.Fatalf("seed blueprint step: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_prov', 'github', 'sky/repo#prov', 'pr', 'active')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_prov', 'e_prov', ?, '')`,
		eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		 VALUES (?, 'e_prov', ?, 'ev_prov', 'queued')`, taskID, eventType); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// The org holds Anthropic only — the step's pinned provider is gone.
	connectOrgProviders(t, s, true, false)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate", map[string]any{
		"hesitation_ms": 0,
		"blueprint_id":  blueprintID,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delegate = %d, want 422 (the refusal is the request's fault, not a spawn fault); body=%s", rec.Code, rec.Body.String())
	}
	// SPAWN_FAILED, like the other 422 arm: the claim stamped before Delegate
	// ran, and the reason is what tells the client it survived. No field —
	// nothing the caller sent is wrong.
	assertFirstError(t, rec, "SPAWN_FAILED", "")
	body := rec.Body.String()
	for _, want := range []string{modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock), bedrockModel, "Settings"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal does not carry %q — the message is the whole point of it: %s", want, body)
		}
	}

	// Reconnecting the provider makes the same gesture work: the refusal was
	// about the credential, and it left nothing behind.
	connectOrgProviders(t, s, true, true)
	again := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate", map[string]any{
		"hesitation_ms": 0,
		"blueprint_id":  blueprintID,
	})
	if again.Code != http.StatusOK {
		t.Fatalf("delegate after reconnecting = %d, want 200; body=%s", again.Code, again.Body.String())
	}
}
