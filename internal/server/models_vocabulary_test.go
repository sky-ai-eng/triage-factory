package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// vocabularyRig is a multi-mode server addressed through the settings handlers
// directly, with claims injected — the same shape
// TestTeamProviders_GatesAndScope_Postgres uses. The multi-mode local
// short-circuits are not available here, so this is the only way to exercise
// the native universe's own rules: the vocabulary it accepts, and the
// per-provider credential gate that only a native id can trip.
type vocabularyRig struct {
	h      *pgtest.Harness
	s      *Server
	orgID  string
	admin  string
	teamID string
}

func newVocabularyRig(t *testing.T) *vocabularyRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	orgID, admin, teamID := pgtest.SeedOrgWithUser(t, h, "vocabulary-api")
	return &vocabularyRig{h: h, s: New(h.AdminDB, stores), orgID: orgID, admin: admin, teamID: teamID}
}

// connect records which credentials the org holds, by the settings refs that are
// the record of what an admin bound.
func (rig *vocabularyRig) connect(t *testing.T, anthropic, bedrock bool) {
	t.Helper()
	anthropicRef, bedrockRef := "", ""
	if anthropic {
		anthropicRef = "anthropic_api_key"
	}
	if bedrock {
		bedrockRef = "aws_bearer_token_bedrock"
	}
	pgtest.MustExec(t, rig.h.AdminDB, `
		INSERT INTO org_settings (org_id, anthropic_api_key_ref, bedrock_credentials_ref)
		VALUES ($1, $2, $3)
		ON CONFLICT (org_id) DO UPDATE
		   SET anthropic_api_key_ref = $2, bedrock_credentials_ref = $3`,
		rig.orgID, anthropicRef, bedrockRef)
}

func (rig *vocabularyRig) request(t *testing.T, method string, body any) *http.Request {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, "/api/settings", nil)
	} else {
		enc, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, "/api/settings", bytes.NewReader(enc))
		r.Header.Set("Content-Type", "application/json")
	}
	r.SetPathValue("org_id", rig.orgID)
	ctx := httpx.WithOrgID(r.Context(), rig.orgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: rig.admin})
	return r.WithContext(ctx)
}

// orgVersion is the concurrency token the org-settings PATCH requires. Read
// straight from the row rather than through the GET, so a broken read cannot
// make a write test pass.
func (rig *vocabularyRig) orgVersion(t *testing.T) int {
	t.Helper()
	var version int
	if err := rig.h.AdminDB.QueryRow(
		`SELECT COALESCE(max(version), 0) FROM org_settings WHERE org_id = $1`, rig.orgID).Scan(&version); err != nil {
		t.Fatalf("read org settings version: %v", err)
	}
	return version
}

func (rig *vocabularyRig) patchOrg(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	withVersion := map[string]any{"version": rig.orgVersion(t)}
	for k, v := range body {
		withVersion[k] = v
	}
	rec := httptest.NewRecorder()
	rig.s.handleOrgSettingsPatch(rec, rig.request(t, http.MethodPatch, withVersion))
	return rec
}

func (rig *vocabularyRig) patchTeam(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	r := rig.request(t, http.MethodPatch, body)
	r.SetPathValue("team_id", rig.teamID)
	rec := httptest.NewRecorder()
	rig.s.handleTeamSettingsPatch(rec, r)
	return rec
}

func (rig *vocabularyRig) storedBackgroundJobsModel(t *testing.T) string {
	t.Helper()
	var model string
	if err := rig.h.AdminDB.QueryRow(
		`SELECT background_jobs_model FROM org_settings WHERE org_id = $1`, rig.orgID).Scan(&model); err != nil {
		t.Fatalf("read background_jobs_model: %v", err)
	}
	return model
}

// The mirror image of the local vocabulary tests: a multi deployment stores and
// dispatches concrete wire ids, so the harness alias is what it refuses — an
// alias on the bifrost wire goes unresolved and every message it produced
// persists unpriced, which is the bug this whole design started from.
func TestOrgSettingsPatch_BackgroundJobsModel_Postgres_RefusesTheOtherVocabulary(t *testing.T) {
	rig := newVocabularyRig(t)
	rig.connect(t, true, true)

	rec := rig.patchOrg(t, map[string]any{"background_jobs_model": domain.ModelAliasSonnet})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "background_jobs_model") {
		t.Errorf("error does not name the field: %s", rec.Body.String())
	}
	// The message names this deployment's universe, so the caller's next move
	// is picking from it.
	if !strings.Contains(rec.Body.String(), domain.ModelSonnet) {
		t.Errorf("error does not name the native universe: %s", rec.Body.String())
	}

	ok := rig.patchOrg(t, map[string]any{"background_jobs_model": domain.ModelSonnet})
	if ok.Code != http.StatusOK {
		t.Fatalf("native id save = %d, want 200 (body: %s)", ok.Code, ok.Body.String())
	}
	if got := rig.storedBackgroundJobsModel(t); got != domain.ModelSonnet {
		t.Errorf("stored background_jobs_model = %q, want %q", got, domain.ModelSonnet)
	}
}

// A model whose provider the org has not connected is one its next cycle would
// refuse, so it is not worth storing. The refusal names the provider, because
// connecting it is the whole remedy — and nothing quietly picks the provider the
// org does hold.
//
// Native-path only, and Postgres is where that path runs: the gate reads the
// provider off the id, and an SDK alias names none.
func TestOrgSettingsPatch_BackgroundJobsModel_Postgres_RefusesUnconnectedProvider(t *testing.T) {
	rig := newVocabularyRig(t)
	rig.connect(t, true, false)
	before := rig.storedBackgroundJobsModel(t)

	rec := rig.patchOrg(t, map[string]any{
		"background_jobs_model": modelKeyOn(t, modelcatalog.ProviderBedrock),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock)) {
		t.Errorf("refusal does not name the provider to connect: %s", rec.Body.String())
	}
	if got := rig.storedBackgroundJobsModel(t); got != before {
		t.Errorf("a refused save changed the stored value to %q", got)
	}
}

// Save-time enforcement on the team default: a model whose provider the org
// never connected is refused with the field named. The stored default is
// untouched by the refusal.
//
// Native-path only, for the same reason as the org knob above.
func TestTeamSettingsPatch_Postgres_ModelProviderMustBeAvailable(t *testing.T) {
	bedrockModel := modelKeyOn(t, modelcatalog.ProviderBedrock)

	t.Run("provider not connected", func(t *testing.T) {
		rig := newVocabularyRig(t)
		rig.connect(t, true, false)

		rec := rig.patchTeam(t, map[string]any{"ai_model": bedrockModel})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
		assertFieldError(t, rec, "ai_model")

		set, err := rig.s.allStores.Teams.GetSettingsSystem(t.Context(), rig.teamID)
		if err != nil {
			t.Fatalf("read team settings: %v", err)
		}
		if set.DefaultModel == bedrockModel {
			t.Error("the refused model was stored anyway")
		}
	})

	t.Run("connected saves", func(t *testing.T) {
		rig := newVocabularyRig(t)
		rig.connect(t, true, true)

		rec := rig.patchTeam(t, map[string]any{"ai_model": bedrockModel})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		set, err := rig.s.allStores.Teams.GetSettingsSystem(t.Context(), rig.teamID)
		if err != nil {
			t.Fatalf("read team settings: %v", err)
		}
		if set.DefaultModel != bedrockModel {
			t.Errorf("stored default = %q, want %q", set.DefaultModel, bedrockModel)
		}
	})
}
