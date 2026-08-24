package server

import (
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

// teamModelsPath addresses one team's catalog: the org's enable-set narrowed to
// that team's own.
func teamModelsPath(teamID string) string { return "/api/teams/" + teamID + "/models" }

// keysOf collects the model keys a catalog read returned, in order.
func keysOf(items []modelCatalogRow) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Key)
	}
	return out
}

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

// A team that has narrowed nothing sees its org's whole enable-set; narrowing
// the team removes the rest from the picker's options entirely, and clearing it
// puts them back. The org's own narrowing shows through the team read too, which
// is the intersection doing its job.
//
// Local mode, so the sets are written in the vocabulary a local deployment
// dispatches — the harness's aliases, not native wire ids. That is not a detail
// of the fixture: a set names models the deployment will be asked to run, so the
// save refuses the other vocabulary outright (pinned both ways below).
func TestTeamModelsList_NarrowsToTheEnabledSets(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	universe := modelcatalog.UniverseFor(false)
	keys := universe.Keys()
	if len(keys) < 3 {
		t.Fatalf("the local universe offers %d models, need 3: %v", len(keys), keys)
	}
	// Named by position rather than by constant: what this exercises is set
	// algebra over whatever this deployment offers, and pinning the alias list
	// itself is internal/modelcatalog's job.
	wide, narrow, other := keys[0], keys[1], keys[2]

	read := func() []string {
		t.Helper()
		rec := doJSON(t, s, http.MethodGet, teamModelsPath("default"), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("team models read = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		return keysOf(decodeModels(t, rec.Body.Bytes()).Items)
	}

	if got, want := len(read()), len(keys); got != want {
		t.Fatalf("a team that has narrowed nothing sees %d models, want the whole universe (%d)", got, want)
	}

	// The org narrows to three; the team read follows without the team saying
	// anything, because an absent team set inherits the org's whole answer.
	orgSet := []string{wide, narrow, other}
	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": orgSet})
	if got := read(); len(got) != len(orgSet) {
		t.Errorf("after the org narrowed to %v the team sees %v", orgSet, got)
	}

	// The team narrows further, moving its default in the same call — a team
	// whose default fell outside its own new set is refused, so the two travel
	// together. What it sees afterwards is its own set, not the org's.
	if rec := doJSON(t, s, http.MethodPatch, "/api/teams/default/settings", map[string]any{
		"enabled_models": []string{narrow},
		"ai_model":       narrow,
	}); rec.Code != http.StatusOK {
		t.Fatalf("team narrow: %d %s", rec.Code, rec.Body.String())
	}
	if got := read(); len(got) != 1 || got[0] != narrow {
		t.Errorf("after the team narrowed to %s it sees %v", narrow, got)
	}

	// The org narrowing BELOW what the team stored wins at the read: the team's
	// set is frozen at what it named, and the intersection is what keeps a
	// stale team row from outliving its org's decision. Nothing rewrites the
	// team row to say so.
	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": []string{wide}})
	if got := read(); len(got) != 0 {
		t.Errorf("a team set disjoint from its org's still shows %v", got)
	}

	// Clearing the team's set inherits the org's again. The default moves with
	// it for the same reason as above: the org's narrowing left the team's
	// stored default outside every set it could resolve to, which is exactly
	// the re-pick the org save's warning asks for.
	if rec := doJSON(t, s, http.MethodPatch, "/api/teams/default/settings", map[string]any{
		"enabled_models": nil,
		"ai_model":       wide,
	}); rec.Code != http.StatusOK {
		t.Fatalf("team clear: %d %s", rec.Code, rec.Body.String())
	}
	if got := read(); len(got) != 1 || got[0] != wide {
		t.Errorf("after clearing the team's set it sees %v, want the org's set", got)
	}

	// Clearing the org's set restores the deployment's whole universe for both.
	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": nil})
	if got, want := len(read()), len(keys); got != want {
		t.Errorf("after clearing both sets the team sees %d models, want %d", got, want)
	}
}

// An enable-set names models the deployment will be asked to dispatch, so it is
// written in that deployment's own vocabulary and the other one is refused —
// on both settings scopes, and whichever way round the mismatch runs.
//
// This is the gate that keeps the two registries from mixing in stored config.
// A local org that stored a native wire id would hand the Claude Code harness a
// word it cannot resolve; a multi org that stored an alias would put it on the
// bifrost wire unresolved, and every message it produced would persist unpriced.
// Neither is a spelling the set resolution could recover from later, because
// nothing translates a stored value.
func TestSettingsPatch_EnabledModels_RefusesTheOtherVocabulary(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	native := modelcatalog.UniverseFor(true).Keys()[0]
	alias := modelcatalog.UniverseFor(false).Keys()[0]

	body := map[string]any{"enabled_models": []string{native}}
	for name, patch := range map[string]func() *httptest.ResponseRecorder{
		"org":  func() *httptest.ResponseRecorder { return patchOrgSettings(t, s, body) },
		"team": func() *httptest.ResponseRecorder { return patchTeamSettings(t, s, "default", body) },
	} {
		t.Run(name, func(t *testing.T) {
			rec := patch()
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("a native wire id on a local %s save = %d, want 400 (body: %s)", name, rec.Code, rec.Body.String())
			}
			assertFieldError(t, rec, "enabled_models")
			// The refusal names the offered vocabulary, which is the whole fix.
			if !strings.Contains(rec.Body.String(), alias) {
				t.Errorf("the refusal does not name what this deployment does offer: %s", rec.Body.String())
			}
		})
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

// TestTeamModelsRead_ScopeAndSets_Postgres pins the multi-mode read the local
// short-circuits hide: the team's stored set shapes its catalog on the Postgres
// dialect too, it shapes it for the ORG ADMIN who is not a member of that team
// (the reason the read crosses the admin pool), one team's set does not narrow
// another's, and a team in another org is invisible. Skips without Docker.
func TestTeamModelsRead_ScopeAndSets_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	mdh := &modelsHandler{az: s.az, tx: s.tx}

	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, h, "enableset-founder")
	teamB := pgtest.SeedTeam(t, h, orgID, "teamB")
	orgAdmin := pgtest.SeedUser(t, h, "enableset-orgadmin")
	pgtest.AddOrgMember(t, h, orgAdmin, orgID, teamB, "admin", "member") // org admin, teamB only
	teamAdmin := pgtest.SeedUser(t, h, "enableset-teamadmin")
	pgtest.AddOrgMember(t, h, teamAdmin, orgID, teamA, "member", "admin") // teamA admin, plain org member

	// teamA is narrowed to one model. Written straight to the row rather than
	// through the settings route: this test is about what the READ does with a
	// stored set, and going through the write would only re-prove the write.
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO team_settings (team_id, enabled_models, default_model)
		VALUES ($1, $2, $3)
		ON CONFLICT (team_id) DO UPDATE SET
			enabled_models = EXCLUDED.enabled_models,
			default_model = EXCLUDED.default_model`,
		teamA, `["`+domain.ModelSonnet+`"]`, domain.ModelSonnet)

	req := func(teamID string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/models", nil)
		r.SetPathValue("team_id", teamID)
		return r
	}
	withCaller := func(r *http.Request, caller string) *http.Request {
		ctx := httpx.WithOrgID(r.Context(), orgID)
		ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
		return r.WithContext(ctx)
	}

	t.Run("the_set_shapes_the_read_for_members_and_for_the_org_admin", func(t *testing.T) {
		// The second reader is the point: a membership-gated read would answer
		// the org admin with the defaults and show them a catalog nobody
		// narrowed, seconds after the team narrowed it.
		for _, caller := range []string{teamAdmin, orgAdmin} {
			rec := httptest.NewRecorder()
			mdh.handleTeamModelsList(rec, withCaller(req(teamA), caller))
			if rec.Code != http.StatusOK {
				t.Fatalf("teamA read = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			got := keysOf(decodeModels(t, rec.Body.Bytes()).Items)
			if len(got) != 1 || got[0] != domain.ModelSonnet {
				t.Errorf("teamA read for %s = %v, want just %s", caller, got, domain.ModelSonnet)
			}
		}
	})

	t.Run("another_team_is_untouched", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mdh.handleTeamModelsList(rec, withCaller(req(teamB), orgAdmin))
		if rec.Code != http.StatusOK {
			t.Fatalf("teamB read = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if got, want := len(decodeModels(t, rec.Body.Bytes()).Items), len(modelcatalog.UniverseFor(true).Models()); got != want {
			t.Errorf("narrowing teamA also narrowed teamB: %d models, want %d", got, want)
		}
	})

	t.Run("team_in_another_org_is_404", func(t *testing.T) {
		_, _, otherTeam := pgtest.SeedOrgWithUser(t, h, "enableset-other-org")
		rec := httptest.NewRecorder()
		mdh.handleTeamModelsList(rec, withCaller(req(otherTeam), owner))
		if rec.Code != http.StatusNotFound {
			t.Errorf("read of another org's team = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
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
