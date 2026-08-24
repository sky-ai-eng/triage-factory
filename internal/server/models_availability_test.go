package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/modelprobe"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// probeSecrets is an agentproc.SecretsReader over a map keyed "org/key" — the
// org's Anthropic key and, crucially, the base URL that points the probe at
// the mock provider instead of the real one.
type probeSecrets map[string]string

func (s probeSecrets) Get(_ context.Context, orgID, key string) (string, error) {
	return s[orgID+"/"+key], nil
}

// mockProvider answers the probe's request: a streamed one-token completion,
// or whatever status the test set. Its status is swapped between cases, so one
// server serves the whole table.
type mockProvider struct {
	mu       sync.Mutex
	status   int
	errBody  string
	requests int
}

func (m *mockProvider) answerWith(status int, errBody string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.errBody = status, errBody
}

func (m *mockProvider) Requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

func (m *mockProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests++
	status, errBody := m.status, m.errBody
	m.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if errBody == "" {
			errBody = `{"type":"error","error":{"type":"api_error","message":"stub failure"}}`
		}
		_, _ = w.Write([]byte(errBody))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, frame := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_probe\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":8,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"p\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		fmt.Fprint(w, frame)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

// availabilityRig is a multi-mode server whose probes reach a mock provider
// instead of Anthropic: real handler, real prober, real classification, real
// store — only the far end of the socket is a fake.
type availabilityRig struct {
	h        *pgtest.Harness
	mdh      *modelsHandler
	provider *mockProvider
	orgID    string
	admin    string
	member   string
	teamID   string
}

func newAvailabilityRig(t *testing.T) *availabilityRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, admin, teamID := pgtest.SeedOrgWithUser(t, h, "availability-api")
	member := pgtest.SeedUser(t, h, "availability-api-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	provider := &mockProvider{}
	srv := httptest.NewServer(provider)
	t.Cleanup(srv.Close)

	// The org holds an Anthropic credential: the settings ref is what the
	// handler's connected-provider gate reads, and the secret map is what the
	// prober resolves the actual request from.
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_settings (org_id, anthropic_api_key_ref) VALUES ($1, 'anthropic_api_key')
		 ON CONFLICT (org_id) DO UPDATE SET anthropic_api_key_ref = 'anthropic_api_key'`, orgID)

	secrets := probeSecrets{
		orgID + "/anthropic_api_key":  "sk-ant-probe",
		orgID + "/anthropic_base_url": srv.URL,
	}
	prober := modelprobe.New(secrets, nil, systemllm.NewRecorder(stores.SystemLLMRuns))

	return &availabilityRig{
		h:        h,
		mdh:      &modelsHandler{az: s.az, tx: s.tx, prober: func() modelProber { return prober }},
		provider: provider,
		orgID:    orgID,
		admin:    admin,
		member:   member,
		teamID:   teamID,
	}
}

// req builds a request addressed at orgID with caller's claims. body nil sends
// none.
func (rig *availabilityRig) req(t *testing.T, caller, orgID string, body any) *http.Request {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(http.MethodPost, "/api/models", nil)
	} else {
		enc, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(http.MethodPost, "/api/models", bytes.NewReader(enc))
		r.Header.Set("Content-Type", "application/json")
	}
	r.SetPathValue("org_id", orgID)
	ctx := httpx.WithOrgID(r.Context(), orgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
	return r.WithContext(ctx)
}

// test calls the single-model route for modelKey.
func (rig *availabilityRig) test(t *testing.T, caller, modelKey string) *httptest.ResponseRecorder {
	t.Helper()
	r := rig.req(t, caller, rig.orgID, nil)
	r.SetPathValue("model_key", modelKey)
	rec := httptest.NewRecorder()
	rig.mdh.handleModelTest(rec, r)
	return rec
}

// sweep calls the provider-wide route.
func (rig *availabilityRig) sweep(t *testing.T, caller, provider string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	rig.mdh.handleModelTestSweep(rec, rig.req(t, caller, rig.orgID, map[string]any{"provider": provider}))
	return rec
}

// catalogRead is the org models list as caller sees it — where the badge this
// whole feature exists for is published.
func (rig *availabilityRig) catalogRead(t *testing.T, caller string) []modelCatalogRow {
	t.Helper()
	rec := httptest.NewRecorder()
	rig.mdh.handleModelsList(rec, rig.req(t, caller, rig.orgID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models list = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	return decodeModels(t, rec.Body.Bytes()).Items
}

func (rig *availabilityRig) rowFor(t *testing.T, items []modelCatalogRow, key string) modelCatalogRow {
	t.Helper()
	for _, it := range items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("catalog read has no row for %s", key)
	return modelCatalogRow{}
}

func decodeTestResult(t *testing.T, body []byte) modelTestResult {
	t.Helper()
	var out modelTestResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode test result: %v (body: %s)", err, body)
	}
	return out
}

// availabilityRows counts what the org has stored, straight from the table —
// the assertion "nothing was recorded" needs a reader the handler does not
// share a code path with.
func (rig *availabilityRig) availabilityRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := rig.h.AdminDB.QueryRow(
		`SELECT count(*) FROM model_availability WHERE org_id = $1`, rig.orgID).Scan(&n); err != nil {
		t.Fatalf("count model_availability: %v", err)
	}
	return n
}

// The lifecycle, end to end through the real endpoint: a provider that answers
// records verified, one that refuses records red with its own message, and one
// that fails records NOTHING — which is the property the whole design turns
// on, because a save gate that a 500 could turn red would be a save gate one
// bad minute could jam shut.
func TestModelTest_Postgres_RecordsWhatTheProviderAnswered(t *testing.T) {
	rig := newAvailabilityRig(t)
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)

	t.Run("a_stream_records_verified", func(t *testing.T) {
		rig.provider.answerWith(http.StatusOK, "")
		rec := rig.test(t, rig.admin, key)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		res := decodeTestResult(t, rec.Body.Bytes())
		if res.Outcome != modelTestVerified {
			t.Fatalf("outcome = %q, want %q", res.Outcome, modelTestVerified)
		}
		if res.CheckedAt == nil {
			t.Error("a recorded result carried no checked_at")
		}
		row := rig.rowFor(t, rig.catalogRead(t, rig.admin), key)
		if row.Availability != modelAvailabilityVerified {
			t.Errorf("catalog availability = %q, want %q", row.Availability, modelAvailabilityVerified)
		}
		if row.AvailabilityDetail != "" {
			t.Errorf("verified row carries detail %q, want none", row.AvailabilityDetail)
		}
	})

	t.Run("a_refusal_records_red_with_the_detail", func(t *testing.T) {
		rig.provider.answerWith(http.StatusForbidden,
			`{"type":"error","error":{"type":"permission_error","message":"AccessDeniedException"}}`)
		rec := rig.test(t, rig.admin, key)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — a refusal is a successful test with a negative answer (body: %s)", rec.Code, rec.Body.String())
		}
		res := decodeTestResult(t, rec.Body.Bytes())
		if res.Outcome != modelTestRed {
			t.Fatalf("outcome = %q, want %q", res.Outcome, modelTestRed)
		}
		if !strings.Contains(res.Detail, "AccessDeniedException") {
			t.Errorf("detail = %q, want the provider's own message", res.Detail)
		}
		// This also pins that a manual re-test overwrites a green row: the
		// subtest above left this exact model verified.
		row := rig.rowFor(t, rig.catalogRead(t, rig.admin), key)
		if row.Availability != modelAvailabilityRed {
			t.Errorf("catalog availability = %q, want %q after a refused re-test", row.Availability, modelAvailabilityRed)
		}
		if !strings.Contains(row.AvailabilityDetail, "AccessDeniedException") {
			t.Errorf("catalog detail = %q, want the refusal", row.AvailabilityDetail)
		}
	})

	t.Run("a_failure_records_nothing", func(t *testing.T) {
		before := rig.availabilityRows(t)
		rig.provider.answerWith(http.StatusInternalServerError, "")
		// A model with no row yet, so "nothing recorded" is visible as an
		// absence rather than as an unchanged value.
		fresh := ""
		for _, e := range modelcatalog.UniverseFor(true).Models() {
			if e.Provider == modelcatalog.ProviderAnthropic && e.Key != key {
				fresh = e.Key
				break
			}
		}
		if fresh == "" {
			t.Skip("catalog offers only one Anthropic model")
		}
		rec := rig.test(t, rig.admin, fresh)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if res := decodeTestResult(t, rec.Body.Bytes()); res.Outcome != modelTestInconclusive {
			t.Fatalf("outcome = %q, want %q", res.Outcome, modelTestInconclusive)
		}
		if after := rig.availabilityRows(t); after != before {
			t.Errorf("stored rows went %d → %d; a 500 must write nothing", before, after)
		}
		if row := rig.rowFor(t, rig.catalogRead(t, rig.admin), fresh); row.Availability != modelAvailabilityUnverified {
			t.Errorf("availability = %q, want %q — an inconclusive probe leaves the row absent", row.Availability, modelAvailabilityUnverified)
		}
	})
}

// Every probe is a real charge, so every probe lands in the ledger under
// job=probe — where the spend view rolls it up as system_overhead alongside
// every other call TF makes for itself.
func TestModelTest_Postgres_RecordsSpend(t *testing.T) {
	rig := newAvailabilityRig(t)
	rig.provider.answerWith(http.StatusOK, "")
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)
	if rec := rig.test(t, rig.admin, key); rec.Code != http.StatusOK {
		t.Fatalf("test = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var (
		model string
		cost  float64
	)
	if err := rig.h.AdminDB.QueryRow(
		`SELECT model, total_cost_usd FROM system_llm_runs WHERE org_id = $1 AND job = $2`,
		rig.orgID, systemllm.JobProbe).Scan(&model, &cost); err != nil {
		t.Fatalf("read the probe's ledger row: %v", err)
	}
	if model != key {
		t.Errorf("ledger model = %q, want the probed key %q", model, key)
	}
	if cost <= 0 {
		t.Errorf("ledger cost = %v, want a real stamp", cost)
	}
}

// The eager pass after a credential bind: it tests every candidate of the named
// provider, skips the ones already verified (green is terminal for automatic
// transitions), and never touches the other provider's models.
func TestModelTestSweep_Postgres_SkipsVerifiedAndCoversTheRest(t *testing.T) {
	rig := newAvailabilityRig(t)
	first := modelKeyOn(t, modelcatalog.ProviderAnthropic)

	rig.provider.answerWith(http.StatusOK, "")
	if rec := rig.test(t, rig.admin, first); rec.Code != http.StatusOK {
		t.Fatalf("seed verify = %d (body: %s)", rec.Code, rec.Body.String())
	}
	probesBefore := rig.provider.Requests()

	rec := rig.sweep(t, rig.admin, modelcatalog.ProviderAnthropic)
	if rec.Code != http.StatusOK {
		t.Fatalf("sweep = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp modelTestSweepResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sweep: %v (body: %s)", err, rec.Body.String())
	}

	var anthropic int
	for _, e := range modelcatalog.UniverseFor(true).Models() {
		if e.Provider == modelcatalog.ProviderAnthropic {
			anthropic++
		}
	}
	if len(resp.Items) != anthropic {
		t.Fatalf("sweep covered %d models, want every Anthropic candidate (%d)", len(resp.Items), anthropic)
	}
	for _, item := range resp.Items {
		switch {
		case item.ModelKey == first:
			if item.Outcome != modelTestSkipped {
				t.Errorf("%s: outcome = %q, want %q — it was already verified", item.ModelKey, item.Outcome, modelTestSkipped)
			}
			if item.CheckedAt == nil {
				t.Errorf("%s: a skip carries no checked_at; the earlier verification's stamp is what makes the skip legible", item.ModelKey)
			}
		case item.Outcome != modelTestVerified:
			t.Errorf("%s: outcome = %q, want %q", item.ModelKey, item.Outcome, modelTestVerified)
		}
		if p, _ := modelcatalog.ProviderFor(item.ModelKey); p != modelcatalog.ProviderAnthropic {
			t.Errorf("%s is served by %s; the sweep names one provider", item.ModelKey, p)
		}
	}
	// One request per unskipped candidate, and not one for the skip.
	if spent := rig.provider.Requests() - probesBefore; spent != anthropic-1 {
		t.Errorf("sweep spent %d requests, want %d (every candidate but the verified one)", spent, anthropic-1)
	}
}

// A provider the org never connected refuses up front, before anything is
// attempted — so a sweep of it writes no rows at all and the caller is sent to
// the one page that fixes it.
func TestModelTest_Postgres_UnconnectedProviderRefusesBeforeSpending(t *testing.T) {
	rig := newAvailabilityRig(t)
	bedrock := modelKeyOn(t, modelcatalog.ProviderBedrock)
	before := rig.provider.Requests()

	rec := rig.test(t, rig.admin, bedrock)
	if rec.Code != http.StatusConflict {
		t.Fatalf("single test = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	sweep := rig.sweep(t, rig.admin, modelcatalog.ProviderBedrock)
	if sweep.Code != http.StatusConflict {
		t.Fatalf("sweep = %d, want 409 (body: %s)", sweep.Code, sweep.Body.String())
	}
	if rig.provider.Requests() != before {
		t.Error("a refusal for an unconnected provider still spent a request")
	}
	if n := rig.availabilityRows(t); n != 0 {
		t.Errorf("stored %d rows for a provider that is not connected, want 0", n)
	}
}

// The gates. Spending the org's money is an org-admin act; a model this build
// does not offer is not an address; another org's catalog is not visible.
func TestModelTest_Postgres_Gates(t *testing.T) {
	rig := newAvailabilityRig(t)
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)
	rig.provider.answerWith(http.StatusOK, "")

	t.Run("a_plain_member_cannot_spend", func(t *testing.T) {
		before := rig.provider.Requests()
		if rec := rig.test(t, rig.member, key); rec.Code != http.StatusForbidden {
			t.Errorf("member single test = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
		}
		if rec := rig.sweep(t, rig.member, modelcatalog.ProviderAnthropic); rec.Code != http.StatusForbidden {
			t.Errorf("member sweep = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
		}
		if rig.provider.Requests() != before {
			t.Error("a refused caller still spent a request")
		}
	})

	t.Run("a_member_still_reads_the_badge", func(t *testing.T) {
		// The read is member-level on purpose: a team admin who cannot touch
		// org settings still has to see why a model in their picker is badged.
		if got := len(rig.catalogRead(t, rig.member)); got != len(modelcatalog.Entries()) {
			t.Errorf("member read returned %d models, want the whole catalog", got)
		}
	})

	t.Run("a_model_this_build_does_not_offer_is_404", func(t *testing.T) {
		if rec := rig.test(t, rig.admin, "claude-not-a-model"); rec.Code != http.StatusNotFound {
			t.Errorf("unknown model = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an_unknown_provider_is_400", func(t *testing.T) {
		if rec := rig.sweep(t, rig.admin, "gerrit"); rec.Code != http.StatusBadRequest {
			t.Errorf("unknown provider = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
		rec := httptest.NewRecorder()
		rig.mdh.handleModelTestSweep(rec, rig.req(t, rig.admin, rig.orgID, map[string]any{}))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("absent provider = %d, want 400 — the sweep never means \"all of them\" (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("another_orgs_catalog_is_404", func(t *testing.T) {
		otherOrg, _, _ := pgtest.SeedOrgWithUser(t, rig.h, "availability-other")
		r := rig.req(t, rig.admin, otherOrg, nil)
		r.SetPathValue("model_key", key)
		rec := httptest.NewRecorder()
		rig.mdh.handleModelTest(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Errorf("cross-org test = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
		}
		read := httptest.NewRecorder()
		rig.mdh.handleModelsList(read, rig.req(t, rig.admin, otherOrg, nil))
		if read.Code != http.StatusNotFound {
			t.Errorf("cross-org read = %d, want 404 (body: %s)", read.Code, read.Body.String())
		}
	})
}

// One org's verdict is not another's. The rows are the org's own inference
// entitlements, and the read is scoped to the caller's org rather than to the
// build's catalog.
func TestModelAvailability_Postgres_IsPerOrg(t *testing.T) {
	rig := newAvailabilityRig(t)
	rig.provider.answerWith(http.StatusOK, "")
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)
	if rec := rig.test(t, rig.admin, key); rec.Code != http.StatusOK {
		t.Fatalf("test = %d (body: %s)", rec.Code, rec.Body.String())
	}

	// The neighbour is configured IDENTICALLY — same provider bound — so the
	// only thing that could differ between the two reads is the stored
	// verdict. Leave it unbound and the test passes for the wrong reason: it
	// would be reading "ambient", not "your neighbour's probe is not yours".
	otherOrg, otherAdmin, _ := pgtest.SeedOrgWithUser(t, rig.h, "availability-neighbour")
	pgtest.MustExec(t, rig.h.AdminDB,
		`INSERT INTO org_settings (org_id, anthropic_api_key_ref) VALUES ($1, 'anthropic_api_key')
		 ON CONFLICT (org_id) DO UPDATE SET anthropic_api_key_ref = 'anthropic_api_key'`, otherOrg)
	rec := httptest.NewRecorder()
	rig.mdh.handleModelsList(rec, rig.req(t, otherAdmin, otherOrg, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("neighbour read = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	for _, row := range decodeModels(t, rec.Body.Bytes()).Items {
		want := modelAvailabilityUnverified
		if row.Provider != modelcatalog.ProviderAnthropic {
			want = modelAvailabilityUnconfigured
		}
		if row.Availability != want {
			t.Errorf("%s: neighbour sees %q, want %q — the verdict belongs to the org that paid for it",
				row.Key, row.Availability, want)
		}
	}
}

// cannedProber answers with a fixed verdict. It stands in for the transport, so
// a route test establishes what the handler DOES with a verdict without
// spending a request or spawning the agent runtime — what each transport
// concludes is internal/modelprobe's own question.
type cannedProber struct {
	res modelprobe.Result
	err error
}

func (c cannedProber) Probe(context.Context, string, modelcatalog.Model) (modelprobe.Result, error) {
	return c.res, c.err
}

// bindLocalAnthropic puts the local org on its OWN Anthropic credential, which
// is what makes an availability verdict have a subject at all: a stored row
// describes a credential TF holds a ref to, and the ref is what a later unbind
// invalidates it through. Both fields, because that is what the bind route
// writes — the ref says which provider, the method says the org has stopped
// running on the host's.
func bindLocalAnthropic(t *testing.T, s *Server) {
	t.Helper()
	set, err := s.allStores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	set.AnthropicAPIKeyRef = secretKeyAnthropicAPIKey
	set.BedrockCredentialsRef = ""
	set.LLMAuthMethod = domain.LLMAuthBYOK
	if _, err := s.allStores.Orgs.UpdateSettings(t.Context(), runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("bind anthropic: %v", err)
	}
}

// localModelHandler wires the models handler over a local server with a canned
// verdict, and returns a request addressed at the local org as its own user.
func localModelHandler(s *Server, canned cannedProber) *modelsHandler {
	return &modelsHandler{az: s.az, tx: s.tx, prober: func() modelProber { return canned }}
}

func localModelTestRequest(modelKey string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/models", nil)
	r.SetPathValue("org_id", runmode.LocalDefaultOrgID)
	r.SetPathValue("model_key", modelKey)
	ctx := httpx.WithOrgID(r.Context(), runmode.LocalDefaultOrgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: runmode.LocalDefaultUserID})
	return r.WithContext(ctx)
}

// A local org that brought its OWN credential probes, and the verdict it
// reaches is stored and published exactly as multi's is. Its runs go through
// the agent runtime, which reports the provider's HTTP status on its terminal
// event, so the transport can answer for an alias as readily as for a wire id.
//
// The stored row is keyed (credential family, alias): the alias names no
// provider, so what the verdict is ABOUT is the credential it was spent
// against, and the family half records which one.
//
// Both conclusive verdicts, because they take different arms of the write and a
// refusal is the one that has to carry the provider's own message out to the
// catalog read.
func TestModelTest_LocalMode_ProbesAndPublishesTheVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict     modelprobe.Verdict
		detail      string
		wantOutcome string
		wantState   string
	}{
		"green": {modelprobe.VerdictGreen, "", modelTestVerified, modelAvailabilityVerified},
		"red":   {modelprobe.VerdictRed, "HTTP 403: Authentication error", modelTestRed, modelAvailabilityRed},
	} {
		t.Run(name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			bindLocalAnthropic(t, s)
			mdh := localModelHandler(s, cannedProber{res: modelprobe.Result{Verdict: tc.verdict, Detail: tc.detail}})

			key := domain.ModelAliasSonnet
			rec := httptest.NewRecorder()
			mdh.handleModelTest(rec, localModelTestRequest(key))

			if rec.Code != http.StatusOK {
				t.Fatalf("local test = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			got := decodeTestResult(t, rec.Body.Bytes())
			if got.ModelKey != key {
				t.Errorf("model_key = %q, want %q", got.ModelKey, key)
			}
			if got.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", got.Outcome, tc.wantOutcome)
			}
			if got.Detail != tc.detail {
				t.Errorf("detail = %q, want %q", got.Detail, tc.detail)
			}
			if got.CheckedAt == nil {
				t.Error("no checked_at on a conclusive verdict — the row was not stored")
			}

			// The row is keyed by the credential family it was spent against.
			var provider string
			if err := s.db.QueryRow(
				`SELECT provider FROM model_availability WHERE org_id = ? AND model_key = ?`,
				runmode.LocalDefaultOrgID, key).Scan(&provider); err != nil {
				t.Fatalf("read the stored row: %v", err)
			}
			if provider != modelcatalog.ProviderAnthropic {
				t.Errorf("stored provider = %q, want the credential family %q", provider, modelcatalog.ProviderAnthropic)
			}

			// The verdict reaches the catalog read, which is where anyone
			// choosing a model actually sees it.
			read := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
			if read.Code != http.StatusOK {
				t.Fatalf("catalog read = %d, want 200 (body: %s)", read.Code, read.Body.String())
			}
			for _, row := range decodeModels(t, read.Body.Bytes()).Items {
				if row.Key != key {
					continue
				}
				if row.Availability != tc.wantState {
					t.Errorf("availability = %q, want %q", row.Availability, tc.wantState)
				}
				if row.AvailabilityDetail != tc.detail {
					t.Errorf("availability_detail = %q, want %q", row.AvailabilityDetail, tc.detail)
				}
			}
		})
	}
}

// Under the host's credentials both test routes refuse, and they refuse as a
// SETUP GAP rather than answering — a verdict recorded there would be about the
// machine the process happens to be running on, keyed to nothing TF can name,
// watch, or invalidate. Nothing is stored, and the prober is never called: the
// refusal is decided before anything is spent.
func TestModelTests_LocalMode_SystemCredentialsRefuseWithoutSpending(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	spent := &countingProber{}
	mdh := &modelsHandler{az: s.az, tx: s.tx, prober: func() modelProber { return spent }}

	single := httptest.NewRecorder()
	mdh.handleModelTest(single, localModelTestRequest(domain.ModelAliasSonnet))
	if single.Code != http.StatusConflict {
		t.Errorf("single test = %d, want 409 (body: %s)", single.Code, single.Body.String())
	}

	body, err := json.Marshal(map[string]any{"provider": modelcatalog.ProviderAnthropic})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/models", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("org_id", runmode.LocalDefaultOrgID)
	ctx := httpx.WithOrgID(r.Context(), runmode.LocalDefaultOrgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: runmode.LocalDefaultUserID})
	sweep := httptest.NewRecorder()
	mdh.handleModelTestSweep(sweep, r.WithContext(ctx))
	if sweep.Code != http.StatusConflict {
		t.Errorf("sweep = %d, want 409 (body: %s)", sweep.Code, sweep.Body.String())
	}

	if spent.calls != 0 {
		t.Errorf("the prober was called %d times; a refused route must spend nothing", spent.calls)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM model_availability`).Scan(&rows); err != nil {
		t.Fatalf("count model_availability: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d availability rows stored under system credentials, want none", rows)
	}
}

// An org that has SAID it brings its own credentials and bound none is refused,
// and the refusal has to name THAT gap. There is no provider to name: an alias
// resolves its access path from the credential, so with nothing bound the model
// under test names nothing that could be connected — and a message built from a
// provider nobody chose reads as " is not connected for this organization".
//
// It is the same predicate the badge uses, so a caller told "unconfigured" by
// the read and refused by this is being told one thing about one credential.
func TestModelTest_LocalMode_NothingBoundRefusesWithoutNamingAProvider(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// Brings its own, binds nothing: the state the setup flow leaves behind
	// between choosing BYOK and pasting a key.
	set, err := s.allStores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	set.LLMAuthMethod = domain.LLMAuthBYOK
	if _, err := s.allStores.Orgs.UpdateSettings(t.Context(), runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("select byok: %v", err)
	}

	spent := &countingProber{}
	mdh := &modelsHandler{az: s.az, tx: s.tx, prober: func() modelProber { return spent }}
	rec := httptest.NewRecorder()
	mdh.handleModelTest(rec, localModelTestRequest(domain.ModelAliasSonnet))

	if rec.Code != http.StatusConflict {
		t.Fatalf("test = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	if spent.calls != 0 {
		t.Errorf("the prober was called %d times; a refused route must spend nothing", spent.calls)
	}
	var body struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode errors: %v (body: %s)", err, rec.Body.String())
	}
	if len(body.Errors) != 1 {
		t.Fatalf("reported %d errors, want 1: %s", len(body.Errors), rec.Body.String())
	}
	message := body.Errors[0].Message
	if strings.HasPrefix(message, " ") {
		t.Errorf("refusal opens with a blank provider name: %q", message)
	}
	// The remedy is where an admin goes, and it is the whole point of the
	// message: nothing is bound, so the fix is binding something.
	if !strings.Contains(message, "Settings") {
		t.Errorf("refusal does not say where to fix it: %q", message)
	}
	// The badge the read publishes for the same model agrees.
	read := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if read.Code != http.StatusOK {
		t.Fatalf("catalog read = %d, want 200 (body: %s)", read.Code, read.Body.String())
	}
	for _, row := range decodeModels(t, read.Body.Bytes()).Items {
		if row.Availability != modelAvailabilityUnconfigured {
			t.Errorf("%s: availability = %q, want %q", row.Key, row.Availability, modelAvailabilityUnconfigured)
		}
	}
}

// countingProber records that it was asked. It answers green so that a route
// which wrongly reached it would look like it succeeded, which is what makes
// the call count the assertion rather than the status.
type countingProber struct{ calls int }

func (c *countingProber) Probe(context.Context, string, modelcatalog.Model) (modelprobe.Result, error) {
	c.calls++
	return modelprobe.Result{Verdict: modelprobe.VerdictGreen}, nil
}

// A multi-mode pod that never had the prober wired says so as a configuration
// fault, not as a 500 — and never panics on the nil.
//
// Both nils are covered because they are different bugs with the same symptom.
// A nil GETTER is the unit-test shape. A getter that RETURNS nil is the live
// one: routes register during buildServer and SetModelProber runs afterwards,
// so the closure over s.modelProber hands back a nil interface until it does —
// and calling Probe on that panics rather than 409ing.
func TestModelTest_Postgres_NoProberConfigured(t *testing.T) {
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)
	for name, wire := range map[string]func(*modelsHandler){
		"nil_getter":         func(h *modelsHandler) { h.prober = nil },
		"getter_returns_nil": func(h *modelsHandler) { h.prober = func() modelProber { return nil } },
		"getter_returns_nil_ptr": func(h *modelsHandler) {
			// The shape server.go actually builds: a closure over a field that
			// is still the interface's zero value.
			var unset modelProber
			h.prober = func() modelProber { return unset }
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAvailabilityRig(t)
			wire(rig.mdh)
			if rec := rig.test(t, rig.admin, key); rec.Code != http.StatusConflict {
				t.Errorf("single test = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
			}
			if rec := rig.sweep(t, rig.admin, modelcatalog.ProviderAnthropic); rec.Code != http.StatusConflict {
				t.Errorf("sweep = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The state this whole value exists for: with only Anthropic bound, the four
// Bedrock rows must say so rather than inviting a test. "unverified" would mean
// "press test again" on a button the test route beside them refuses with 409,
// which is a read pointing users at something they cannot do.
func TestModelsList_Postgres_UnconnectedProviderIsUnconfigured(t *testing.T) {
	rig := newAvailabilityRig(t) // binds Anthropic only

	for _, row := range rig.catalogRead(t, rig.admin) {
		want := modelAvailabilityUnverified
		if row.Provider != modelcatalog.ProviderAnthropic {
			want = modelAvailabilityUnconfigured
		}
		if row.Availability != want {
			t.Errorf("%s (%s): availability = %q, want %q", row.Key, row.Provider, row.Availability, want)
		}
		if row.AvailabilityDetail != "" || row.AvailabilityCheckedAt != nil {
			t.Errorf("%s: an unprobed row carries a detail or a timestamp", row.Key)
		}
	}
}

// The derived fact outranks the stored one. A credential unbound after a
// successful probe leaves a green row that was true when it was written and is
// not true now — and the row cannot know that, because nothing re-probes.
// Reporting it as verified would send someone to pin a model that cannot run.
func TestModelsList_Postgres_UnconfiguredOutranksAStoredGreen(t *testing.T) {
	rig := newAvailabilityRig(t)
	rig.provider.answerWith(http.StatusOK, "")
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)

	if rec := rig.test(t, rig.admin, key); rec.Code != http.StatusOK {
		t.Fatalf("seed verify = %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rig.rowFor(t, rig.catalogRead(t, rig.admin), key).Availability; got != modelAvailabilityVerified {
		t.Fatalf("availability before the disconnect = %q, want %q", got, modelAvailabilityVerified)
	}

	// The org switches providers: Bedrock in, Anthropic out, the way the two
	// credential routes do it — each clears only its own ref. It has to still
	// hold SOMETHING, or it would be ambient, which is a different state with
	// a different answer (see the ambient test below). The green row is
	// deliberately left in place: nothing purges it, which is exactly why the
	// derivation has to outrank it.
	pgtest.MustExec(t, rig.h.AdminDB, `
		UPDATE org_settings
		   SET bedrock_credentials_ref = 'aws_bearer_token_bedrock',
		       anthropic_api_key_ref   = NULL
		 WHERE org_id = $1`, rig.orgID)

	if got := rig.rowFor(t, rig.catalogRead(t, rig.admin), key).Availability; got != modelAvailabilityUnconfigured {
		t.Errorf("availability after the disconnect = %q, want %q", got, modelAvailabilityUnconfigured)
	}
	var stored int
	if err := rig.h.AdminDB.QueryRow(
		`SELECT count(*) FROM model_availability WHERE org_id = $1 AND state = 'green'`, rig.orgID).Scan(&stored); err != nil {
		t.Fatalf("count stored rows: %v", err)
	}
	if stored != 1 {
		t.Errorf("stored green rows = %d, want the row left untouched — this is a read-side derivation, not a purge", stored)
	}
}

// A multi-mode org that binds nothing is unconfigured, not ambient. There are
// no host credentials for a hosted deployment to lend — the operator's
// environment is one environment shared by every tenant, and credential
// resolution refuses such an org outright — so anything softer here would badge
// a picker full of models whose every run fails on the way out.
func TestModelsList_Postgres_NothingBoundIsUnconfigured(t *testing.T) {
	rig := newAvailabilityRig(t)
	pgtest.MustExec(t, rig.h.AdminDB,
		`UPDATE org_settings SET anthropic_api_key_ref = NULL, bedrock_credentials_ref = NULL WHERE org_id = $1`, rig.orgID)

	for _, row := range rig.catalogRead(t, rig.admin) {
		if row.Availability != modelAvailabilityUnconfigured {
			t.Errorf("%s: availability = %q, want %q for an org that has bound nothing",
				row.Key, row.Availability, modelAvailabilityUnconfigured)
		}
	}
}

// And a row hand-written to say the org runs on the host's credentials does not
// change it: domain.EffectiveLLMAuthMethod resolves the mode's single legal
// value whatever the column says, because a hosted deployment has none to lend.
// The settings PATCH refuses to write it at all; this is the backstop for a row
// that arrived some other way.
func TestModelsList_Postgres_StoredHostCredentialsAreInert(t *testing.T) {
	rig := newAvailabilityRig(t)
	pgtest.MustExec(t, rig.h.AdminDB, `
		UPDATE org_settings
		   SET anthropic_api_key_ref = NULL,
		       bedrock_credentials_ref = NULL,
		       llm_auth_method = 'system'
		 WHERE org_id = $1`, rig.orgID)

	for _, row := range rig.catalogRead(t, rig.admin) {
		if row.Availability != modelAvailabilityUnconfigured {
			t.Errorf("%s: availability = %q, want %q — multi has no host credentials to run under",
				row.Key, row.Availability, modelAvailabilityUnconfigured)
		}
	}
}

// Availability is org truth, so the team-scoped read reports the same state for
// every entry that survives the team's provider restriction. The restriction
// removes entries; it never changes what a remaining one says.
func TestTeamModelsList_Postgres_ReportsTheSameAvailability(t *testing.T) {
	rig := newAvailabilityRig(t)
	rig.provider.answerWith(http.StatusOK, "")
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)
	if rec := rig.test(t, rig.admin, key); rec.Code != http.StatusOK {
		t.Fatalf("seed verify = %d (body: %s)", rec.Code, rec.Body.String())
	}

	orgRows := map[string]string{}
	for _, row := range rig.catalogRead(t, rig.admin) {
		orgRows[row.Key] = row.Availability
	}

	r := rig.req(t, rig.admin, rig.orgID, nil)
	r.SetPathValue("team_id", rig.teamID)
	rec := httptest.NewRecorder()
	rig.mdh.handleTeamModelsList(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("team read = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items
	if len(items) == 0 {
		t.Fatal("team read returned no models")
	}
	for _, row := range items {
		if want := orgRows[row.Key]; row.Availability != want {
			t.Errorf("%s: team read says %q, org read says %q — availability is org truth", row.Key, row.Availability, want)
		}
	}
}

// The zero-configuration local install: it runs on the host's credentials, so
// there is no TF-owned credential for a verdict to be about and the whole
// availability triple is absent from every row. Not "unverified" — that word
// promises a test button, and the routes beside this one refuse.
func TestModelsList_LocalMode_SystemCredentialsPublishNoAvailability(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items
	if len(items) == 0 {
		t.Fatal("catalog read returned no models")
	}
	for _, row := range items {
		if row.Availability != "" || row.AvailabilityDetail != "" || row.AvailabilityCheckedAt != nil {
			t.Errorf("%s: availability = %q on the host's credentials, want the triple absent", row.Key, row.Availability)
		}
	}
}

// A stored verdict does not survive the org going back to the host's
// credentials. The row stays in the table — nothing destroys it, and rebinding
// the same credential makes it meaningful again — but it is not published,
// because what it describes is a credential this org is no longer using.
func TestModelsList_LocalMode_SystemCredentialsSuppressAStoredVerdict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	if _, err := s.allStores.ModelAvailability.Record(t.Context(), runmode.LocalDefaultOrgID,
		modelcatalog.ProviderAnthropic, domain.ModelAliasSonnet, domain.ModelAvailabilityGreen, ""); err != nil {
		t.Fatalf("record a green verdict: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	for _, row := range decodeModels(t, rec.Body.Bytes()).Items {
		if row.Availability != "" {
			t.Errorf("%s: availability = %q, want the triple absent under system credentials", row.Key, row.Availability)
		}
	}
}

// A local org that has SAID it brings its own key and bound none is
// unconfigured, and the recorded choice is what decides it — the same emptiness
// that means "zero-config subscription" one row over. modelaccess.Ready refuses
// to dispatch for such an org rather than letting the run authenticate from
// whatever the operator's environment holds, so the badge is not overstating:
// it is naming the refusal a run would hit.
func TestModelsList_LocalMode_OwnCredentialsWithNoneBoundIsUnconfigured(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	set, err := s.allStores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	set.LLMAuthMethod = domain.LLMAuthBYOK
	if _, err := s.allStores.Orgs.UpdateSettings(t.Context(), runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("select byok: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items
	if len(items) == 0 {
		t.Fatal("catalog read returned no models")
	}
	for _, row := range items {
		if row.Availability != modelAvailabilityUnconfigured {
			t.Errorf("%s: availability = %q, want %q for an org bringing its own key with none bound",
				row.Key, row.Availability, modelAvailabilityUnconfigured)
		}
	}
}

// Local BYOK's half of the same state, and the stored verdict beside it.
//
// The org brought its own Anthropic credential, so the surface exists: a stored
// green renders verified, and everything it has not probed renders unverified —
// an answer it can act on, because the test route beside it will spend one.
// Nothing is unconfigured here, and nothing can be: an alias names no provider,
// so "connected the provider that serves this model" has no per-model answer and
// the org-wide one is what stands in.
func TestModelsList_LocalMode_BYOKReadsStoredVerdicts(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	bindLocalAnthropic(t, s)

	probed := domain.ModelAliasSonnet
	if _, err := s.allStores.ModelAvailability.Record(t.Context(), runmode.LocalDefaultOrgID,
		modelcatalog.ProviderAnthropic, probed, domain.ModelAvailabilityGreen, ""); err != nil {
		t.Fatalf("record a green verdict: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, modelsPath(runmode.LocalDefaultOrgID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items
	if len(items) == 0 {
		t.Fatal("catalog read returned no models")
	}
	for _, row := range items {
		want := modelAvailabilityUnverified
		if row.Key == probed {
			want = modelAvailabilityVerified
		}
		if row.Availability != want {
			t.Errorf("%s: availability = %q, want %q", row.Key, row.Availability, want)
		}
	}
}
