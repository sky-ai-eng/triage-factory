package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestMe_IsOperator_ReadsVerifiedIdentities pins /api/me's is_operator to the
// same read the fleet gate makes: whether one of the principal's login
// identities holds an operator address with TF's trust. The token's email
// claim is not consulted, so an SSO sign-in whose identity provider asserts an
// operator's address on an untrusted domain is not an operator, and an
// operator calling with an API token still is (the console refuses the token
// with 403 at its own gate; the flag describes the person).
func TestMe_IsOperator_ReadsVerifiedIdentities(t *testing.T) {
	r := newAuthRig(t)
	ctx := context.Background()
	const opEmail = "ops@corp.example"
	if err := r.srv.allStores.Operators.Add(ctx, opEmail, "test"); err != nil {
		t.Fatalf("add operator: %v", err)
	}

	meIsOperator := func(t *testing.T, req *http.Request) bool {
		t.Helper()
		rec := httptest.NewRecorder()
		r.srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/me status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var body struct {
			IsOperator bool `json:"is_operator"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /api/me: %v", err)
		}
		return body.IsOperator
	}
	newAuthUser := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		r.h.SeedAuthUser(t, id.String(), id.String()+"@auth.test")
		return id
	}

	// The operator signs in with GitHub, which reports the address verified.
	opAuth := newAuthUser(t)
	claims := validClaimsFor(opAuth)
	claims["email"] = opEmail
	claims["email_verified"] = true
	resp, _ := r.driveCallbackClaims(claims)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("github callback status = %d; want 302", resp.StatusCode)
	}
	opSid := r.sidFromResp(resp)

	t.Run("verified github identity is an operator", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: opSid})
		if !meIsOperator(t, req) {
			t.Error("is_operator = false; want true for a GitHub identity holding the operator address verified")
		}
	})

	t.Run("untrusted sso identity with the address is not", func(t *testing.T) {
		samlAuth := newAuthUser(t)
		c := validClaimsFor(samlAuth)
		c["email"] = opEmail
		c["email_verified"] = true
		resp := r.driveSSOCallbackClaims(c, uuid.NewString())
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("sso callback status = %d; want 302", resp.StatusCode)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: r.sidFromResp(resp)})
		if meIsOperator(t, req) {
			t.Error("is_operator = true for an SSO sign-in whose only claim to the operator address is its assertion")
		}
	})

	t.Run("operator with an api token is still an operator", func(t *testing.T) {
		var opPrincipal uuid.UUID
		if err := r.h.AdminDB.QueryRow(
			`SELECT user_id FROM user_identities WHERE auth_user_id = $1`, opAuth,
		).Scan(&opPrincipal); err != nil {
			t.Fatalf("resolve operator principal: %v", err)
		}
		orgID, _ := r.seedOrg(opPrincipal, "ops-org")
		_, plaintext := r.mintToken(opPrincipal, orgID, "ops")
		if !meIsOperator(t, r.bearerReq(http.MethodGet, "/api/me", plaintext)) {
			t.Error("is_operator = false for an operator's token; want true")
		}
	})
}
