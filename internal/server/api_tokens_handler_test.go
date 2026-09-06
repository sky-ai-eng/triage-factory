package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/apitokens"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ---------- rig helpers ----------

// tokensJSON fires a JSON request at s.mux carrying exactly one credential —
// a sid cookie when sid is set, a Bearer when bearer is — so a test names which
// credential it is exercising at the call site rather than in setup.
func (r *authRig) tokensJSON(method, path string, body any, sid, bearer string) *httptest.ResponseRecorder {
	r.t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			r.t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// signIn drives the OAuth callback for a seeded user and returns the sid.
func (r *authRig) signIn(userID uuid.UUID) string {
	r.t.Helper()
	resp, _ := r.driveCallback(userID)
	return r.sidFromResp(resp)
}

// errorItems decodes the one error envelope every /api/* fault answers with.
func errorItems(t *testing.T, rec *httptest.ResponseRecorder) []struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field"`
} {
	t.Helper()
	var env struct {
		Errors []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Errors
}

// assertFault pins one fault's status and reason — the pair a client branches
// on, and the pair this ticket specifies route by route.
func assertFault(t *testing.T, rec *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	items := errorItems(t, rec)
	if len(items) != 1 {
		t.Fatalf("want exactly 1 error item, got %d: %s", len(items), rec.Body.String())
	}
	if items[0].Reason != reason {
		t.Errorf("reason = %q, want %q: %s", items[0].Reason, reason, rec.Body.String())
	}
}

// tokenCreated is the create response, the only shape that ever carries a
// plaintext token.
type tokenCreated struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	OrgID              string   `json:"org_id"`
	TokenPrefix        string   `json:"token_prefix"`
	CreatedAt          string   `json:"created_at"`
	LastUsedAt         *string  `json:"last_used_at"`
	ExpiresAt          *string  `json:"expires_at"`
	EffectiveExpiresAt *string  `json:"effective_expires_at"`
	AllowedCIDRs       []string `json:"allowed_cidrs"`
	Token              string   `json:"token"`
}

type tokenList struct {
	Items         []tokenCreated `json:"items"`
	NextPageToken string         `json:"next_page_token"`
	TotalCount    *int           `json:"total_count"`
}

// createToken mints through the HTTP surface (not the store) so every test that
// needs a token also exercises the route that makes one.
func (r *authRig) createToken(sid string, body map[string]any) tokenCreated {
	r.t.Helper()
	rec := r.tokensJSON("POST", "/api/me/tokens", body, sid, "")
	if rec.Code != http.StatusCreated {
		r.t.Fatalf("create token = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var out tokenCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		r.t.Fatalf("decode create response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func (r *authRig) listTokens(sid, bearer string, body map[string]any) tokenList {
	r.t.Helper()
	rec := r.tokensJSON("POST", "/api/me/tokens/list", body, sid, bearer)
	if rec.Code != http.StatusOK {
		r.t.Fatalf("list tokens = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out tokenList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		r.t.Fatalf("decode list response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// setTokenAgeCap writes the org's api_token_max_age_days directly, which is the
// setting surface's business rather than this ticket's.
func (r *authRig) setTokenAgeCap(orgID uuid.UUID, days int) {
	r.t.Helper()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO org_settings (org_id, api_token_max_age_days) VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET api_token_max_age_days = EXCLUDED.api_token_max_age_days
	`, orgID, days); err != nil {
		r.t.Fatalf("set token age cap: %v", err)
	}
}

// ---------- policy ----------

// TestAPITokenPolicy_MemberReadsTheCap pins the one read a member has of the
// policy that binds their tokens: null while the org sets no cap, the number
// once it does, and the same 404 for an org the caller is not in as every
// other org-addressed read. A plain member (not an admin) is the caller, since
// admins could already read it off the settings row.
func TestAPITokenPolicy_MemberReadsTheCap(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "policy-org")
	member := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		member, orgID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	sid := r.signIn(member)
	path := "/api/orgs/" + orgID.String() + "/api-token-policy"

	read := func() map[string]any {
		t.Helper()
		rec := r.tokensJSON("GET", path, nil, sid, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET policy = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return out
	}

	// Uncapped: the key is present and null — "no cap" is an answer.
	if v, has := read()["max_age_days"]; !has || v != nil {
		t.Errorf("uncapped max_age_days = %v (present=%v), want null", v, has)
	}
	r.setTokenAgeCap(orgID, 90)
	if v := read()["max_age_days"]; v != float64(90) {
		t.Errorf("capped max_age_days = %v, want 90", v)
	}

	// A token sealed to this org reads it too — the surface it is about is
	// reachable under both credentials.
	_, plaintext := r.mintToken(member, orgID, "reader")
	rec := r.tokensJSON("GET", path, nil, "", plaintext)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"max_age_days":90`) {
		t.Errorf("GET policy under bearer = %d %s, want 200 with the cap", rec.Code, rec.Body.String())
	}

	// Not a member: 404, indistinguishable from an org that does not exist.
	stranger := r.seedUser()
	rec = r.tokensJSON("GET", path, nil, r.signIn(stranger), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET policy as non-member = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// ---------- create ----------

// TestAPITokenCreate_ReturnsPlaintextExactlyOnce is the shape of the whole
// feature: the secret exists in one response and in no read that follows.
func TestAPITokenCreate_ReturnsPlaintextExactlyOnce(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "mint-org")
	sid := r.signIn(userID)

	got := r.createToken(sid, map[string]any{"name": "ci runner", "org_id": orgID.String()})

	if !strings.HasPrefix(got.Token, apitokens.Prefix) {
		t.Errorf("token %q does not start with %q", got.Token, apitokens.Prefix)
	}
	if len(got.Token) != len(apitokens.Prefix)+43 {
		t.Errorf("token length = %d, want %d", len(got.Token), len(apitokens.Prefix)+43)
	}
	if !strings.HasPrefix(got.Token, got.TokenPrefix) {
		t.Errorf("token_prefix %q is not a prefix of the token", got.TokenPrefix)
	}
	if got.Name != "ci runner" || got.OrgID != orgID.String() {
		t.Errorf("create returned name=%q org=%q, want %q / %q", got.Name, got.OrgID, "ci runner", orgID)
	}
	if got.LastUsedAt != nil {
		t.Errorf("last_used_at = %v on a token nobody has used, want null", *got.LastUsedAt)
	}
	if got.ExpiresAt != nil || got.EffectiveExpiresAt != nil {
		t.Errorf("expiries = %v / %v on an unexpiring token in an uncapped org, want null/null",
			got.ExpiresAt, got.EffectiveExpiresAt)
	}
	if got.AllowedCIDRs == nil {
		t.Error("allowed_cidrs = null, want [] — no restriction is an answer, not an absence")
	}

	// The read beside it never carries the secret, and the raw JSON is checked
	// rather than a decoded field: a `token` key under any shape would be a leak.
	rec := r.tokensJSON("POST", "/api/me/tokens/list", map[string]any{}, sid, "")
	if strings.Contains(rec.Body.String(), got.Token) {
		t.Fatalf("list response contained the plaintext token: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("list response carried a token field: %s", rec.Body.String())
	}
}

// TestAPITokenCreate_ReportsEveryFieldFault is the all-fields rule: one
// response names every problem, so a caller fixing one does not discover the
// next on the next request.
func TestAPITokenCreate_ReportsEveryFieldFault(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "faults-org")
	sid := r.signIn(userID)

	rec := r.tokensJSON("POST", "/api/me/tokens", map[string]any{
		"name":          "   ",
		"org_id":        orgID.String(),
		"expires_at":    "next tuesday",
		"allowed_cidrs": []string{"10.0.0.0/8", "not-an-ip"},
	}, sid, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	items := errorItems(t, rec)
	if len(items) != 3 {
		t.Fatalf("want 3 faults (name, expires_at, allowed_cidrs), got %d: %s", len(items), rec.Body.String())
	}
	fields := map[string]bool{}
	for _, it := range items {
		fields[it.Field] = true
		if it.Reason != "INVALID_FIELD" {
			t.Errorf("reason on %s = %q, want INVALID_FIELD", it.Field, it.Reason)
		}
	}
	for _, want := range []string{"name", "expires_at", "allowed_cidrs"} {
		if !fields[want] {
			t.Errorf("no fault reported for %s: %s", want, rec.Body.String())
		}
	}
}

// TestAPITokenCreate_FieldValidation walks each single-field refusal.
func TestAPITokenCreate_FieldValidation(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "validate-org")
	sid := r.signIn(userID)

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"empty name", map[string]any{"name": "", "org_id": orgID.String()}, "name"},
		{"whitespace name", map[string]any{"name": "\t\n ", "org_id": orgID.String()}, "name"},
		{"oversize name", map[string]any{
			"name": strings.Repeat("x", apitokens.MaxNameLen+1), "org_id": orgID.String()}, "name"},
		{"missing org", map[string]any{"name": "n"}, "org_id"},
		{"malformed org", map[string]any{"name": "n", "org_id": "not-a-uuid"}, "org_id"},
		{"past expiry", map[string]any{"name": "n", "org_id": orgID.String(),
			"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, "expires_at"},
		{"non-rfc3339 expiry", map[string]any{"name": "n", "org_id": orgID.String(),
			"expires_at": "2026-08-30"}, "expires_at"},
		{"bad cidr", map[string]any{"name": "n", "org_id": orgID.String(),
			"allowed_cidrs": []string{"300.0.0.0/8"}}, "allowed_cidrs"},
		// Refused rather than masked: 10.0.0.1/8 is a typo for one of two very
		// different things, and picking either silently is the bug.
		{"unmasked cidr", map[string]any{"name": "n", "org_id": orgID.String(),
			"allowed_cidrs": []string{"10.0.0.1/8"}}, "allowed_cidrs"},
		{"duplicate cidr", map[string]any{"name": "n", "org_id": orgID.String(),
			"allowed_cidrs": []string{"10.0.0.0/8", "10.0.0.0/8"}}, "allowed_cidrs"},
		// Two spellings of one range are the same duplicate.
		{"duplicate host spelling", map[string]any{"name": "n", "org_id": orgID.String(),
			"allowed_cidrs": []string{"10.0.0.1", "10.0.0.1/32"}}, "allowed_cidrs"},
		{"too many cidrs", map[string]any{"name": "n", "org_id": orgID.String(),
			"allowed_cidrs": manyCIDRs(apitokens.MaxAllowedCIDRs + 1)}, "allowed_cidrs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.tokensJSON("POST", "/api/me/tokens", tc.body, sid, "")
			assertFault(t, rec, http.StatusUnprocessableEntity, "INVALID_FIELD")
			if got := errorItems(t, rec)[0].Field; got != tc.field {
				t.Errorf("field = %q, want %q", got, tc.field)
			}
		})
	}
}

func manyCIDRs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("10.%d.0.0/16", i)
	}
	return out
}

// TestAPITokenCreate_CanonicalizesCIDRs pins that a bare address is stored as
// the host range it means, and that the round-trip shows the canonical form.
func TestAPITokenCreate_CanonicalizesCIDRs(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "cidr-org")
	sid := r.signIn(userID)

	got := r.createToken(sid, map[string]any{
		"name": "fixed-ip", "org_id": orgID.String(),
		"allowed_cidrs": []string{"203.0.113.7", "2001:db8::/32"},
	})
	want := []string{"203.0.113.7/32", "2001:db8::/32"}
	if len(got.AllowedCIDRs) != len(want) {
		t.Fatalf("allowed_cidrs = %v, want %v", got.AllowedCIDRs, want)
	}
	for i, w := range want {
		if got.AllowedCIDRs[i] != w {
			t.Errorf("allowed_cidrs[%d] = %q, want %q", i, got.AllowedCIDRs[i], w)
		}
	}
}

// TestAPITokenCreate_UnknownFieldRejected is the strict-decode rule, and the
// specific reason expires_in_days is not a field: a client that sends one must
// hear so rather than silently get a token that never expires.
func TestAPITokenCreate_UnknownFieldRejected(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "strict-org")
	sid := r.signIn(userID)

	rec := r.tokensJSON("POST", "/api/me/tokens", map[string]any{
		"name": "n", "org_id": orgID.String(), "expires_in_days": 30,
	}, sid, "")
	assertFault(t, rec, http.StatusBadRequest, "UNKNOWN_FIELD")
}

// TestAPITokenCreate_NonMemberOrgIs404 is the disclosure rule: an org the
// caller does not belong to is not confirmed to exist.
func TestAPITokenCreate_NonMemberOrgIs404(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "mine-org")
	stranger := r.seedUser()
	otherOrg, _ := r.seedOrg(stranger, "theirs-org")
	sid := r.signIn(userID)

	rec := r.tokensJSON("POST", "/api/me/tokens",
		map[string]any{"name": "n", "org_id": otherOrg.String()}, sid, "")
	assertFault(t, rec, http.StatusNotFound, "NOT_FOUND")
}

// TestAPITokenCreate_HonorsOrgAgeCap: an explicit expiry beyond the org's cap
// is refused with the cap named, never quietly shortened. An omitted expiry
// under the same cap succeeds, and the cap shows up as effective_expires_at.
func TestAPITokenCreate_HonorsOrgAgeCap(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "capped-org")
	sid := r.signIn(userID)
	r.setTokenAgeCap(orgID, 7)

	rec := r.tokensJSON("POST", "/api/me/tokens", map[string]any{
		"name": "too long", "org_id": orgID.String(),
		"expires_at": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}, sid, "")
	assertFault(t, rec, http.StatusUnprocessableEntity, "INVALID_FIELD")
	item := errorItems(t, rec)[0]
	if item.Field != "expires_at" {
		t.Errorf("field = %q, want expires_at", item.Field)
	}
	if !strings.Contains(item.Message, "7 days") {
		t.Errorf("message %q does not name the cap", item.Message)
	}

	// Under the cap: accepted, and expires_at is what was asked for.
	want := time.Now().Add(3 * 24 * time.Hour).UTC().Truncate(time.Second)
	under := r.createToken(sid, map[string]any{
		"name": "fits", "org_id": orgID.String(), "expires_at": want.Format(time.RFC3339),
	})
	if under.ExpiresAt == nil {
		t.Fatal("expires_at = null on a token minted with one")
	}

	// Omitted: no stored expiry, but the cap still binds, so the effective
	// expiry is the one the org's policy computes.
	capped := r.createToken(sid, map[string]any{"name": "capped", "org_id": orgID.String()})
	if capped.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want null — nothing was asked for", *capped.ExpiresAt)
	}
	if capped.EffectiveExpiresAt == nil {
		t.Fatal("effective_expires_at = null under a 7-day cap, want the computed bound")
	}
	eff, err := time.Parse(time.RFC3339, *capped.EffectiveExpiresAt)
	if err != nil {
		t.Fatalf("parse effective_expires_at: %v", err)
	}
	if delta := eff.Sub(time.Now().Add(7 * 24 * time.Hour)); delta > time.Minute || delta < -time.Minute {
		t.Errorf("effective_expires_at = %s, want ~7 days out", eff)
	}
}

// TestAPITokenCreate_LimitIs409 pins the store's per-(user, org) cap onto the
// status a client can act on.
func TestAPITokenCreate_LimitIs409(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "limit-org")
	sid := r.signIn(userID)

	// The store's own cap is what is being surfaced, so fill it there rather
	// than paying for MaxPerUserOrg HTTP round trips.
	for i := 0; i < apitokens.MaxPerUserOrg; i++ {
		r.mintToken(userID, orgID, fmt.Sprintf("filler-%d", i))
	}
	rec := r.tokensJSON("POST", "/api/me/tokens",
		map[string]any{"name": "one too many", "org_id": orgID.String()}, sid, "")
	assertFault(t, rec, http.StatusConflict, "CONFLICT")
}

// ---------- list ----------

// TestAPITokenList_PagingContract walks the shared list contract on this route:
// a default window, an explicit page, count-only, and a page token that is
// refused once the filters change under it.
func TestAPITokenList_PagingContract(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "page-org-a")
	orgB, _ := r.seedOrg(userID, "page-org-b")
	sid := r.signIn(userID)

	for i := 0; i < 3; i++ {
		r.mintToken(userID, orgA, fmt.Sprintf("a-%d", i))
	}
	r.mintToken(userID, orgB, "b-0")

	all := r.listTokens(sid, "", map[string]any{})
	if *all.TotalCount != 4 || len(all.Items) != 4 {
		t.Fatalf("unfiltered list = %d items / total %d, want 4/4", len(all.Items), *all.TotalCount)
	}

	filtered := r.listTokens(sid, "", map[string]any{"org_id": orgA.String()})
	if *filtered.TotalCount != 3 {
		t.Errorf("org-filtered total = %d, want 3", *filtered.TotalCount)
	}
	for _, it := range filtered.Items {
		if it.OrgID != orgA.String() {
			t.Errorf("filtered list carried a token from org %s", it.OrgID)
		}
	}

	// Count-only: the filtered total with no rows to pay for.
	countOnly := r.listTokens(sid, "", map[string]any{"page_size": 0})
	if len(countOnly.Items) != 0 {
		t.Errorf("page_size:0 returned %d items, want 0", len(countOnly.Items))
	}
	if *countOnly.TotalCount != 4 {
		t.Errorf("page_size:0 total = %d, want 4", *countOnly.TotalCount)
	}

	// A page walk with the token from the first page.
	first := r.listTokens(sid, "", map[string]any{"page_size": 2})
	if len(first.Items) != 2 || first.NextPageToken == "" {
		t.Fatalf("page 1 = %d items, next=%q, want 2 and a token", len(first.Items), first.NextPageToken)
	}
	second := r.listTokens(sid, "", map[string]any{"page_size": 2, "page_token": first.NextPageToken})
	if len(second.Items) != 2 {
		t.Errorf("page 2 = %d items, want 2", len(second.Items))
	}
	for _, a := range first.Items {
		for _, b := range second.Items {
			if a.ID == b.ID {
				t.Errorf("token %s appeared on both pages", a.ID)
			}
		}
	}

	// The same token against a different filter set is refused rather than
	// silently addressing a different result set.
	rec := r.tokensJSON("POST", "/api/me/tokens/list", map[string]any{
		"page_size": 2, "page_token": first.NextPageToken, "org_id": orgA.String(),
	}, sid, "")
	assertFault(t, rec, http.StatusBadRequest, "INVALID_PARAM")

	// And an out-of-range window is a fault, never a clamp.
	rec = r.tokensJSON("POST", "/api/me/tokens/list", map[string]any{"page_size": 5000}, sid, "")
	assertFault(t, rec, http.StatusBadRequest, "OUT_OF_RANGE")
}

// TestAPITokenList_MalformedFilterIsRejected: a filter that cannot be parsed
// must never widen the answer back to every org.
func TestAPITokenList_MalformedFilterIsRejected(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "badfilter-org")
	sid := r.signIn(userID)
	r.mintToken(userID, orgID, "visible")

	rec := r.tokensJSON("POST", "/api/me/tokens/list", map[string]any{"org_id": "nope"}, sid, "")
	assertFault(t, rec, http.StatusBadRequest, "INVALID_FIELD")
}

// TestAPITokenList_HidesOtherUsersAndRevoked: the list is the caller's own live
// tokens and nothing else.
func TestAPITokenList_HidesOtherUsersAndRevoked(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "scope-org")
	sid := r.signIn(userID)

	mine := r.createToken(sid, map[string]any{"name": "mine", "org_id": orgID.String()})
	doomed := r.createToken(sid, map[string]any{"name": "doomed", "org_id": orgID.String()})

	// A second member of the same org, so only the user predicate can be what
	// keeps their token out of this answer.
	other := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		other, orgID); err != nil {
		t.Fatalf("add second member: %v", err)
	}
	r.mintToken(other, orgID, "not mine")

	if rec := r.tokensJSON("DELETE", "/api/me/tokens/"+doomed.ID, nil, sid, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204: %s", rec.Code, rec.Body.String())
	}

	got := r.listTokens(sid, "", map[string]any{})
	if len(got.Items) != 1 || got.Items[0].ID != mine.ID {
		t.Fatalf("list = %+v, want only %s", got.Items, mine.ID)
	}
	if *got.TotalCount != 1 {
		t.Errorf("total = %d, want 1 — the count must match the same filters", *got.TotalCount)
	}
}

// ---------- revoke ----------

// TestAPITokenRevoke_Faults pins each refusal to its status.
func TestAPITokenRevoke_Faults(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "revoke-org")
	sid := r.signIn(userID)

	t.Run("malformed id", func(t *testing.T) {
		rec := r.tokensJSON("DELETE", "/api/me/tokens/not-a-uuid", nil, sid, "")
		assertFault(t, rec, http.StatusBadRequest, "INVALID_ID")
	})
	t.Run("unknown id", func(t *testing.T) {
		rec := r.tokensJSON("DELETE", "/api/me/tokens/"+uuid.NewString(), nil, sid, "")
		assertFault(t, rec, http.StatusNotFound, "NOT_FOUND")
	})
	t.Run("another user's token is invisible, not forbidden", func(t *testing.T) {
		other := r.seedUser()
		otherOrg, _ := r.seedOrg(other, "revoke-other-org")
		theirs, _ := r.mintToken(other, otherOrg, "theirs")
		rec := r.tokensJSON("DELETE", "/api/me/tokens/"+theirs.ID, nil, sid, "")
		assertFault(t, rec, http.StatusNotFound, "NOT_FOUND")
	})
	t.Run("double revoke", func(t *testing.T) {
		tok := r.createToken(sid, map[string]any{"name": "twice", "org_id": orgID.String()})
		if rec := r.tokensJSON("DELETE", "/api/me/tokens/"+tok.ID, nil, sid, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("first revoke = %d, want 204", rec.Code)
		}
		rec := r.tokensJSON("DELETE", "/api/me/tokens/"+tok.ID, nil, sid, "")
		assertFault(t, rec, http.StatusNotFound, "NOT_FOUND")
	})
}

// TestAPITokenRevoke_RevokedTokenStopsWorking closes the loop: revocation is
// not bookkeeping, the credential dies.
func TestAPITokenRevoke_RevokedTokenStopsWorking(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "dies-org")
	sid := r.signIn(userID)

	tok := r.createToken(sid, map[string]any{"name": "short-lived", "org_id": orgID.String()})
	if rec := r.serve(r.bearerReq("GET", "/api/me", tok.Token)); rec.Code != http.StatusOK {
		t.Fatalf("token before revoke = %d, want 200", rec.Code)
	}
	if rec := r.tokensJSON("DELETE", "/api/me/tokens/"+tok.ID, nil, sid, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if rec := r.serve(r.bearerReq("GET", "/api/me", tok.Token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token after revoke = %d, want 401", rec.Code)
	}
}

// ---------- the headless flow, and the same-org rules that bound it ----------

// TestAPITokens_HeadlessRotation is the acceptance shape: a session mints the
// first token, and from there the token alone lists, mints its replacement in
// the same org, and revokes itself. No cookie after the first call.
func TestAPITokens_HeadlessRotation(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "rotate-org")
	sid := r.signIn(userID)

	first := r.createToken(sid, map[string]any{"name": "deploy", "org_id": orgID.String()})

	// Mint the replacement with the old token. Same name deliberately —
	// rotation must not force a rename.
	rec := r.tokensJSON("POST", "/api/me/tokens",
		map[string]any{"name": "deploy", "org_id": orgID.String()}, "", first.Token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("token-authed mint = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var second tokenCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second.ID == first.ID || second.Token == first.Token {
		t.Fatal("rotation returned the same credential")
	}

	// The new token sees both, then retires the old one.
	listed := r.listTokens("", second.Token, map[string]any{})
	if len(listed.Items) != 2 {
		t.Fatalf("list under the new token = %d items, want 2", len(listed.Items))
	}
	if rec := r.tokensJSON("DELETE", "/api/me/tokens/"+first.ID, nil, "", second.Token); rec.Code != http.StatusNoContent {
		t.Fatalf("token-authed revoke = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if rec := r.serve(r.bearerReq("GET", "/api/me", first.Token)); rec.Code != http.StatusUnauthorized {
		t.Errorf("retired token = %d, want 401", rec.Code)
	}

	// Self-revoke is allowed, and the response still goes out.
	if rec := r.tokensJSON("DELETE", "/api/me/tokens/"+second.ID, nil, "", second.Token); rec.Code != http.StatusNoContent {
		t.Fatalf("self-revoke = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if rec := r.serve(r.bearerReq("GET", "/api/me", second.Token)); rec.Code != http.StatusUnauthorized {
		t.Errorf("self-revoked token = %d, want 401", rec.Code)
	}
}

// TestAPITokens_TokenCannotLeaveItsOrg is the containment property: a leaked
// token for org A can neither mint one for org B, nor read B's tokens, nor
// revoke them — while the same calls under a session succeed, which is what
// proves the refusal is the credential's and not a missing membership.
func TestAPITokens_TokenCannotLeaveItsOrg(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "cross-org-a")
	orgB, _ := r.seedOrg(userID, "cross-org-b")
	sid := r.signIn(userID)

	aToken := r.createToken(sid, map[string]any{"name": "a-only", "org_id": orgA.String()})
	bToken := r.createToken(sid, map[string]any{"name": "b-side", "org_id": orgB.String()})

	t.Run("cross-org mint", func(t *testing.T) {
		rec := r.tokensJSON("POST", "/api/me/tokens",
			map[string]any{"name": "escalation", "org_id": orgB.String()}, "", aToken.Token)
		assertFault(t, rec, http.StatusForbidden, "FORBIDDEN")
	})
	t.Run("cross-org list filter", func(t *testing.T) {
		rec := r.tokensJSON("POST", "/api/me/tokens/list",
			map[string]any{"org_id": orgB.String()}, "", aToken.Token)
		assertFault(t, rec, http.StatusForbidden, "FORBIDDEN")
	})
	t.Run("cross-org revoke", func(t *testing.T) {
		rec := r.tokensJSON("DELETE", "/api/me/tokens/"+bToken.ID, nil, "", aToken.Token)
		assertFault(t, rec, http.StatusForbidden, "FORBIDDEN")
	})

	// An unfiltered list under a token is its org's list, never every org's.
	listed := r.listTokens("", aToken.Token, map[string]any{})
	if len(listed.Items) != 1 || listed.Items[0].OrgID != orgA.String() {
		t.Fatalf("unfiltered token list = %+v, want only org A's token", listed.Items)
	}
	if *listed.TotalCount != 1 {
		t.Errorf("total = %d, want 1 — the default filter must count what it returns", *listed.TotalCount)
	}

	// The same three calls under the session, which spans both orgs, succeed.
	if rec := r.tokensJSON("POST", "/api/me/tokens",
		map[string]any{"name": "fine", "org_id": orgB.String()}, sid, ""); rec.Code != http.StatusCreated {
		t.Errorf("session mint in org B = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := r.tokensJSON("POST", "/api/me/tokens/list",
		map[string]any{"org_id": orgB.String()}, sid, ""); rec.Code != http.StatusOK {
		t.Errorf("session list of org B = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := r.tokensJSON("DELETE", "/api/me/tokens/"+bToken.ID, nil, sid, ""); rec.Code != http.StatusNoContent {
		t.Errorf("session revoke in org B = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

// TestAPITokens_MeUnderBearer: /api/me answers for a token caller, with the
// token's sealed org as the cursor. The route reads its active org from
// request context rather than the session row, which is what makes it tolerate
// having no session at all.
func TestAPITokens_MeUnderBearer(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "me-org-a")
	orgB, _ := r.seedOrg(userID, "me-org-b")
	sid := r.signIn(userID)

	// Point the SESSION at org A, then call with a token sealed to org B: the
	// answer must be the token's org, not whatever a session cursor says.
	if rec := r.tokensJSON("POST", "/api/me/active-org",
		map[string]any{"org_id": orgA.String()}, sid, ""); rec.Code != http.StatusOK {
		t.Fatalf("set active org = %d: %s", rec.Code, rec.Body.String())
	}
	bToken := r.createToken(sid, map[string]any{"name": "b", "org_id": orgB.String()})

	rec := r.serve(r.bearerReq("GET", "/api/me", bToken.Token))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me under Bearer = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var me struct {
		ID          string `json:"id"`
		ActiveOrgID string `json:"active_org_id"`
		Orgs        []struct {
			ID string `json:"id"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if me.ID != userID.String() {
		t.Errorf("id = %q, want %q", me.ID, userID)
	}
	if me.ActiveOrgID != orgB.String() {
		t.Errorf("active_org_id = %q, want the token's org %q", me.ActiveOrgID, orgB)
	}
	// Memberships still span both orgs: the token narrows what may be ACTED on,
	// not what the principal is.
	if len(me.Orgs) != 2 {
		t.Errorf("orgs = %d, want 2", len(me.Orgs))
	}
}

// TestAPITokens_NoOriginHeader is the shape a real headless client actually
// sends: a Bearer and no Origin at all. The mutating routes sit behind the
// same-origin check, which skips a Bearer request precisely so this works — a
// cross-site browser context cannot attach the header without a preflight this
// server never approves, so its presence already says the request was
// deliberate.
func TestAPITokens_NoOriginHeader(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "curl-org")
	sid := r.signIn(userID)
	tok := r.createToken(sid, map[string]any{"name": "curl", "org_id": orgID.String()})

	// No Origin, no cookie — just the Authorization header.
	post := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			if payload, err = json.Marshal(body); err != nil {
				t.Fatalf("marshal: %v", err)
			}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("POST", "/api/me/tokens/list", map[string]any{}); rec.Code != http.StatusOK {
		t.Errorf("Origin-less list = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec := post("POST", "/api/me/tokens", map[string]any{"name": "minted-headless", "org_id": orgID.String()})
	if rec.Code != http.StatusCreated {
		t.Fatalf("Origin-less mint = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var made tokenCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec := post("DELETE", "/api/me/tokens/"+made.ID, nil); rec.Code != http.StatusNoContent {
		t.Errorf("Origin-less revoke = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

// ---------- session-verb guards ----------

// TestSessionVerbs_RefuseTokenAuth: a verb whose subject is the session itself
// refuses a token with 403 and a message that says what is required — not 401,
// which would send an automation off to re-authenticate a working credential.
func TestSessionVerbs_RefuseTokenAuth(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "guarded-org")
	sid := r.signIn(userID)
	tok := r.createToken(sid, map[string]any{"name": "guard-probe", "org_id": orgID.String()})

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"active-org switch", "POST", "/api/me/active-org", map[string]any{"org_id": orgID.String()}},
		{"logout", "POST", "/api/auth/logout", map[string]any{}},
		{"logout all", "POST", "/api/auth/logout/all", map[string]any{}},
		{"invite accept", "POST", "/api/invites/accept", map[string]any{"token": "whatever"}},
		{"org create", "POST", "/api/orgs", map[string]any{"name": "Token Made This"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.tokensJSON(tc.method, tc.path, tc.body, "", tok.Token)
			assertFault(t, rec, http.StatusForbidden, "FORBIDDEN")
			if msg := errorItems(t, rec)[0].Message; !strings.Contains(strings.ToLower(msg), "session") {
				t.Errorf("message %q does not name the session requirement", msg)
			}
		})
	}

	// The refusal is not just a status: nothing was created. A token's org is
	// sealed at mint, so an org minted under one would be reachable by no
	// credential its owner holds.
	var orgs int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM orgs WHERE name = 'Token Made This'`).Scan(&orgs); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if orgs != 0 {
		t.Errorf("a token created %d org(s); it must create none", orgs)
	}

	// The guard is about the credential, not the route: the same calls under a
	// cookie still behave as they always did.
	if rec := r.tokensJSON("POST", "/api/me/active-org",
		map[string]any{"org_id": orgID.String()}, sid, ""); rec.Code != http.StatusOK {
		t.Errorf("session active-org = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := r.tokensJSON("POST", "/api/auth/logout", map[string]any{}, sid, ""); rec.Code != http.StatusNoContent {
		t.Errorf("session logout = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

// TestSessionVerbs_LogoutRefusesBearerWithoutSession is the case the shared
// guard exists for: POST /api/auth/logout sits outside withSession, so nothing
// has resolved the credential by the time it runs and only the header's shape
// says a token was presented. Without the check it would revoke nothing and
// answer 204 — success for work that never happened.
func TestSessionVerbs_LogoutRefusesBearerWithoutSession(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "logout-guard-org")

	rec := r.tokensJSON("POST", "/api/auth/logout", map[string]any{}, "", "tf_not-a-real-token")
	assertFault(t, rec, http.StatusForbidden, "FORBIDDEN")
}

// TestSessionVerbs_NonAPITokenBearerIsNotTokenAuth pins the boundary of the
// guard's header fallback: it asks for OUR token's shape, not for a Bearer.
//
// This server accepts exactly one Bearer form. A credential in any other shape
// is not an API token, so the session verbs must not answer it "you need a
// session" — that is both untrue and, on the pre-auth logout mount, a change to
// what a caller who never touched a token gets from a route that has always
// ignored the header. Logout still ends the session the cookie names.
func TestSessionVerbs_NonAPITokenBearerIsNotTokenAuth(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "foreign-bearer-org")

	// A GoTrue-style JWT, and a bare opaque string: neither is one of ours.
	for _, cred := range []string{
		"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.not.a.tf.token",
		"some-other-service-credential",
	} {
		t.Run(cred[:12], func(t *testing.T) {
			sid := r.signIn(userID)
			// Sanity: the cookie alone logs out, so a difference below is the
			// header's doing.
			rec := r.tokensJSON("POST", "/api/auth/logout", map[string]any{}, sid, cred)
			if rec.Code == http.StatusForbidden {
				t.Fatalf("logout with a non-API-token Bearer = 403; the guard must not "+
					"claim a credential it does not accept is an API token: %s", rec.Body.String())
			}
			if rec.Code != http.StatusNoContent {
				t.Fatalf("logout with a non-API-token Bearer = %d, want 204 (unchanged "+
					"from before the guard existed): %s", rec.Code, rec.Body.String())
			}
			// The session really ended — the route did its work rather than
			// being waved through.
			if rec := r.tokensJSON("POST", "/api/me/active-org",
				map[string]any{"org_id": uuid.NewString()}, sid, ""); rec.Code != http.StatusUnauthorized {
				t.Errorf("session after logout = %d, want 401 — logout must have revoked it", rec.Code)
			}
		})
	}
}

// TestSessionVerbs_NonAPITokenBearerOnGuardedRouteIs401 is the same boundary on
// the routes that DO sit behind withSession: a credential this server does not
// accept is refused there as an unusable credential, before any handler runs.
// 401 and not the guard's 403 — the fault is the credential, not the verb.
func TestSessionVerbs_NonAPITokenBearerOnGuardedRouteIs401(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "guarded-401-org")

	for _, path := range []string{"/api/me/active-org", "/api/auth/logout/all", "/api/invites/accept", "/api/orgs"} {
		rec := r.tokensJSON("POST", path,
			map[string]any{"org_id": orgID.String(), "token": "x"}, "", "definitely-not-ours")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a non-API-token Bearer = %d, want 401: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// ---------- mode ----------

// TestAPITokens_LocalMode404 — the routes are multi-only, like every
// session-auth route: local mode's identity is already headless, so there is
// nothing there for a token to be.
func TestAPITokens_LocalMode404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/me/tokens", map[string]any{"name": "n", "org_id": uuid.NewString()}},
		{"POST", "/api/me/tokens/list", map[string]any{}},
		{"DELETE", "/api/me/tokens/" + uuid.NewString(), nil},
		{"GET", "/api/orgs/" + uuid.NewString() + "/api-token-policy", nil},
	} {
		rec := doJSON(t, s, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s in local mode = %d, want 404: %s",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
