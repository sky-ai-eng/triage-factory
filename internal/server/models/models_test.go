package models

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// localRig is a local-mode store over the schema the binary boots with, which
// seeds the local org, team and user.
type localRig struct {
	db     *sql.DB
	stores db.Stores
	az     *authz.Checker
}

func newLocalRig(t *testing.T) *localRig {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(conn)
	return &localRig{db: conn, stores: stores, az: authz.New(conn, stores.Tx)}
}

// handler is the models handler over the local store, probing through p.
func (l *localRig) handler(p Prober) *Handler {
	return New(l.az, l.stores.Tx, func() Prober { return p })
}

// list is the local org's catalog read.
func (l *localRig) list(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	l.handler(nil).handleModelsList(rec, localRequest())
	return rec
}

// localRequest is a request addressed at the local org as its own user — the
// identity the session middleware seeds in local mode.
func localRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/models", nil)
	r.SetPathValue("org_id", runmode.LocalDefaultOrgID)
	ctx := httpx.WithOrgID(r.Context(), runmode.LocalDefaultOrgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: runmode.LocalDefaultUserID})
	return r.WithContext(ctx)
}

// decodeModels reads the response body as the catalog contract.
func decodeModels(t *testing.T, body []byte) modelCatalogResponse {
	t.Helper()
	var resp modelCatalogResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode catalog: %v (body: %s)", err, body)
	}
	return resp
}

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

// The read a member of a MULTI org gets: every native registry entry, enabled,
// with prices and capabilities joined in from the datasheet.
func TestModelsList_Postgres_ServesTheJoinedCatalog(t *testing.T) {
	rig := newAvailabilityRig(t)
	items := rig.catalogRead(t, rig.member)

	catalog := modelcatalog.Entries()
	if len(items) != len(catalog) {
		t.Fatalf("returned %d models, want %d (one per registry entry)", len(items), len(catalog))
	}
	for i, want := range catalog {
		got := items[i]
		if got.Key != want.Key {
			t.Errorf("item %d: key = %q, want %q (file order is display order)", i, got.Key, want.Key)
		}
		if got.DisplayName != want.DisplayName {
			t.Errorf("%s: display_name = %q, want %q", got.Key, got.DisplayName, want.DisplayName)
		}
		if got.Provider != want.Provider {
			t.Errorf("%s: provider = %q, want %q", got.Key, got.Provider, want.Provider)
		}
		if got.PricesPerMTok == nil {
			t.Fatalf("%s: no prices on a native row", got.Key)
		}
		if got.PricesPerMTok.Output != want.Prices.Output {
			t.Errorf("%s: output price = %v, want %v", got.Key, got.PricesPerMTok.Output, want.Prices.Output)
		}
		if got.ContextWindow != want.ContextWindow {
			t.Errorf("%s: context_window = %d, want %d", got.Key, got.ContextWindow, want.ContextWindow)
		}
		if got.SupportsPromptCaching == nil {
			t.Errorf("%s: no supports_prompt_caching on a native row", got.Key)
		} else if *got.SupportsPromptCaching != want.SupportsPromptCaching {
			t.Errorf("%s: supports_prompt_caching = %v, want %v", got.Key, *got.SupportsPromptCaching, want.SupportsPromptCaching)
		}
		if got.ProviderDisplayName != modelcatalog.ProviderDisplayName(want.Provider) {
			t.Errorf("%s: provider_display_name = %q, want %q",
				got.Key, got.ProviderDisplayName, modelcatalog.ProviderDisplayName(want.Provider))
		}
		if got.DisplayOrder != i {
			t.Errorf("%s: display_order = %d, want %d", got.Key, got.DisplayOrder, i)
		}
		// No org has expressed an enable-set yet, so every entry is enabled.
		if !got.Enabled {
			t.Errorf("%s: enabled = false, want true with no stored enable-set", got.Key)
		}
	}
}

// The read a LOCAL install gets: the Claude Code SDK's alias list, and only it.
// Every datasheet field and the availability triple are omitted from the wire,
// not zero-valued: a rendered $0.00 is a price claim and `"provider": ""` an
// access-path claim, and an alias names no provider and joins no price table.
func TestModelsList_LocalServesTheSDKAliasList(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	rec := newLocalRig(t).list(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items

	want := modelcatalog.SDKModels(modelcatalog.SDKClaudeCode)
	if len(items) != len(want) {
		t.Fatalf("returned %d models, want %d (one per SDK alias): %+v", len(items), len(want), items)
	}
	for i, w := range want {
		got := items[i]
		if got.Key != w.Key {
			t.Errorf("item %d: key = %q, want %q", i, got.Key, w.Key)
		}
		if got.DisplayName != w.DisplayName {
			t.Errorf("%s: display_name = %q, want %q", got.Key, got.DisplayName, w.DisplayName)
		}
		if got.DisplayOrder != i {
			t.Errorf("%s: display_order = %d, want %d", got.Key, got.DisplayOrder, i)
		}
		if !got.Enabled {
			t.Errorf("%s: enabled = false, want true with no stored enable-set", got.Key)
		}
		if got.Provider != "" {
			t.Errorf("%s: provider = %q, want absent — an alias names no access path", got.Key, got.Provider)
		}
		if got.PricesPerMTok != nil {
			t.Errorf("%s: prices_per_mtok = %+v, want absent — cost is harness-settled", got.Key, *got.PricesPerMTok)
		}
		if got.ContextWindow != 0 || got.SupportsPromptCaching != nil {
			t.Errorf("%s: carries datasheet facts (%d, %v), want absent", got.Key, got.ContextWindow, got.SupportsPromptCaching)
		}
		if got.ProviderDisplayName != "" {
			t.Errorf("%s: provider_display_name = %q, want absent — an alias names no access path", got.Key, got.ProviderDisplayName)
		}
		if got.Availability != "" || got.AvailabilityDetail != "" || got.AvailabilityCheckedAt != nil {
			t.Errorf("%s: carries an availability triple under system credentials, want absent", got.Key)
		}
	}

	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, item := range raw.Items {
		for _, absent := range []string{
			"provider", "prices_per_mtok", "context_window", "supports_prompt_caching",
			"availability", "availability_detail", "availability_checked_at",
		} {
			if _, present := item[absent]; present {
				t.Errorf("%s: field %q is on the wire, want it omitted", item["key"], absent)
			}
		}
		for _, required := range []string{"key", "display_name", "enabled", "display_order"} {
			if _, present := item[required]; !present {
				t.Errorf("%v: field %q is missing", item["key"], required)
			}
		}
	}
}

// A path id that cannot name an org names nothing the caller may learn about.
func TestModelsList_MalformedOrgIs404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	h := newLocalRig(t).handler(nil)
	r := localRequest()
	r.SetPathValue("org_id", "not-a-uuid")
	rec := httptest.NewRecorder()
	h.handleModelsList(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The org in the path is the subject, and membership of THAT org is the gate: a
// member reads it, and a non-member is told the org does not exist rather than
// that they may not see it.
func TestModelsList_Postgres_OrgScoping(t *testing.T) {
	rig := newAvailabilityRig(t)
	if got, want := len(rig.catalogRead(t, rig.member)), len(modelcatalog.Entries()); got != want {
		t.Errorf("member read returned %d models, want %d", got, want)
	}

	otherOrg, _, _ := pgtest.SeedOrgWithUser(t, rig.h, "models-other-org")
	r := rig.req(t, rig.member, rig.orgID, nil)
	r.SetPathValue("org_id", otherOrg)
	rec := httptest.NewRecorder()
	rig.mdh.handleModelsList(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-member GET = %d, want 404 (not 403 — don't leak existence)", rec.Code)
	}
}

// TestTeamModelsRead_ScopeAndSets_Postgres: on Postgres the team's set shapes
// its catalog, including for an org admin not on the team; one team's set does
// not narrow another's, and a team in another org is invisible.
func TestTeamModelsRead_ScopeAndSets_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	mdh := New(authz.New(h.AdminDB, stores.Tx), stores.Tx, nil)

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
