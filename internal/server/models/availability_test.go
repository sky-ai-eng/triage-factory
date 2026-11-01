package models

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
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelprobe"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/systemjob/systemllm"
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
	mdh      *Handler
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
		mdh:      New(authz.New(h.AdminDB, stores.Tx), stores.Tx, func() Prober { return prober }),
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

// A provider that answers records verified (and the probe's spend), one that
// refuses records red with its own message, and one that fails records nothing,
// so a transient 500 cannot turn a model red.
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

		// Every probe is a real charge, so it lands in the ledger under job=probe.
		var (
			model string
			cost  float64
		)
		if err := rig.h.AdminDB.QueryRow(
			`SELECT model, total_cost_usd FROM system_llm_runs WHERE org_id = $1 AND job = $2`,
			rig.orgID, systemllm.JobProbe).Scan(&model, &cost); err != nil {
			t.Fatalf("read the probe's ledger row: %v", err)
		}
		if model != key || cost <= 0 {
			t.Errorf("ledger row = (%q, %v), want the probed key %q at a real cost", model, cost, key)
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

// One org's verdict is not another's: the read is scoped to the caller's org.
// The neighbour, holding only Anthropic, sees its Anthropic rows unverified and
// every other provider's unconfigured, with no detail or timestamp on any.
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
		if row.AvailabilityDetail != "" || row.AvailabilityCheckedAt != nil {
			t.Errorf("%s: an unprobed row carries a detail or a timestamp", row.Key)
		}
	}
}

// cannedProber answers with a fixed verdict. It stands in for the transport, so
// a route test establishes what the handler DOES with a verdict without
// spending a request or spawning the agent runtime — what each transport
// concludes is internal/llm/modelprobe's own question.
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
func bindLocalAnthropic(t *testing.T, l *localRig) {
	t.Helper()
	if _, err := l.stores.Orgs.SetAnthropicKeyRef(t.Context(), runmode.LocalDefaultOrgID, "anthropic_api_key"); err != nil {
		t.Fatalf("bind anthropic: %v", err)
	}
	if _, err := l.stores.Orgs.SetBedrockCredentialsRef(t.Context(), runmode.LocalDefaultOrgID, ""); err != nil {
		t.Fatalf("unbind bedrock: %v", err)
	}
}

// localModelTestRequest is a test-route request addressed at the local org as
// its own user.
func localModelTestRequest(modelKey string) *http.Request {
	r := localRequest()
	r.SetPathValue("model_key", modelKey)
	return r
}

// A local org on its own credential probes, and the verdict is stored and
// published as multi's is. The row is keyed (credential family, alias), since an
// alias names no provider. Both conclusive verdicts take different write arms.
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
			l := newLocalRig(t)
			bindLocalAnthropic(t, l)
			mdh := l.handler(cannedProber{res: modelprobe.Result{Verdict: tc.verdict, Detail: tc.detail}})

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
			if err := l.db.QueryRow(
				`SELECT provider FROM model_availability WHERE org_id = ? AND model_key = ?`,
				runmode.LocalDefaultOrgID, key).Scan(&provider); err != nil {
				t.Fatalf("read the stored row: %v", err)
			}
			if provider != modelcatalog.ProviderAnthropic {
				t.Errorf("stored provider = %q, want the credential family %q", provider, modelcatalog.ProviderAnthropic)
			}

			// The verdict reaches the catalog read, which is where anyone
			// choosing a model actually sees it.
			read := l.list(t)
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

// Under the host's credentials both test routes refuse with 409 before calling
// the prober and store nothing: a verdict there would describe the machine the
// process runs on, keyed to nothing TF can name or invalidate.
func TestModelTests_LocalMode_SystemCredentialsRefuseWithoutSpending(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	l := newLocalRig(t)
	spent := &countingProber{}
	mdh := l.handler(spent)

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
	if err := l.db.QueryRow(`SELECT count(*) FROM model_availability`).Scan(&rows); err != nil {
		t.Fatalf("count model_availability: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d availability rows stored under system credentials, want none", rows)
	}
}

// A local org on its own credentials with none bound: the test route refuses
// without naming a provider (an alias names none), and the catalog read badges
// every model unconfigured, the same predicate.
func TestModelTest_LocalMode_NothingBoundRefusesWithoutNamingAProvider(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	l := newLocalRig(t)

	// Brings its own, binds nothing: the state the setup flow leaves behind
	// between choosing BYOK and pasting a key.
	set, err := l.stores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	set.LLMAuthMethod = domain.LLMAuthBYOK
	if _, err := l.stores.Orgs.UpdateSettingsVersioned(t.Context(), runmode.LocalDefaultOrgID, set, set.Version); err != nil {
		t.Fatalf("select byok: %v", err)
	}

	spent := &countingProber{}
	mdh := l.handler(spent)
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
	read := l.list(t)
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

// A multi-mode pod with no prober wired answers 409, not a 500 or a panic. A
// getter that returns nil is the live shape: routes register before
// SetModelProber runs, so the closure hands back a nil interface until it does.
func TestModelTest_Postgres_NoProberConfigured(t *testing.T) {
	key := modelKeyOn(t, modelcatalog.ProviderAnthropic)
	for name, wire := range map[string]func(*Handler){
		"nil_getter":         func(h *Handler) { h.prober = nil },
		"getter_returns_nil": func(h *Handler) { h.prober = func() Prober { return nil } },
		"getter_returns_nil_ptr": func(h *Handler) {
			// The shape server.go actually builds: a closure over a field that
			// is still the interface's zero value.
			var unset Prober
			h.prober = func() Prober { return unset }
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

// The derived fact outranks the stored one: a green row left by a probe before
// the credential was unbound reads unconfigured, because nothing re-probes and
// verified would send someone to pin a model that cannot run.
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
	// credential routes do it — each clears only its own ref. The green row is
	// left in place: nothing purges it, which is why the derivation has to
	// outrank it.
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

// A multi-mode org that binds nothing is unconfigured, even with a row
// hand-written to say it runs on the host's credentials: multi has none to lend,
// so domain.EffectiveLLMAuthMethod ignores the column.
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
// every entry that survives the team's enable-set. The set removes entries; it
// never changes what a remaining one says.
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

// A local org on the host's credentials has no TF-owned credential for a
// verdict to be about, so every row omits the availability triple, even with a
// stored verdict (kept for a later rebind) in the table.
func TestModelsList_LocalMode_SystemCredentialsSuppressAStoredVerdict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	l := newLocalRig(t)
	if _, err := l.stores.ModelAvailability.Record(t.Context(), runmode.LocalDefaultOrgID,
		modelcatalog.ProviderAnthropic, domain.ModelAliasSonnet, domain.ModelAvailabilityGreen, ""); err != nil {
		t.Fatalf("record a green verdict: %v", err)
	}

	rec := l.list(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	items := decodeModels(t, rec.Body.Bytes()).Items
	if len(items) == 0 {
		t.Fatal("catalog read returned no models")
	}
	for _, row := range items {
		if row.Availability != "" || row.AvailabilityDetail != "" || row.AvailabilityCheckedAt != nil {
			t.Errorf("%s: availability = %q, want the triple absent under system credentials", row.Key, row.Availability)
		}
	}
}

// A local org on its own Anthropic credential reads a stored green as verified
// and everything unprobed as unverified. Nothing is unconfigured: an alias names
// no provider, so the org-wide credential answers for every model.
func TestModelsList_LocalMode_BYOKReadsStoredVerdicts(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	l := newLocalRig(t)
	bindLocalAnthropic(t, l)

	probed := domain.ModelAliasSonnet
	if _, err := l.stores.ModelAvailability.Record(t.Context(), runmode.LocalDefaultOrgID,
		modelcatalog.ProviderAnthropic, probed, domain.ModelAvailabilityGreen, ""); err != nil {
		t.Fatalf("record a green verdict: %v", err)
	}

	rec := l.list(t)
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
