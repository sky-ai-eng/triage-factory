package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestMe_IsOperator_ReadsVerifiedIdentities: is_operator reads what the fleet
// gate reads. A GitHub sign-in verifying a grant's address binds it to the
// principal; an SSO sign-in asserting it on an untrusted domain binds nothing;
// and an operator calling with an API token is still reported as one.
func TestMe_IsOperator_ReadsVerifiedIdentities(t *testing.T) {
	r := newAuthRig(t)
	ctx := context.Background()
	const (
		opEmail      = "ops@corp.example"
		pendingEmail = "pending-ops@corp.example"
	)
	for _, email := range []string{opEmail, pendingEmail} {
		if _, err := r.srv.allStores.Operators.Add(ctx, email, "test"); err != nil {
			t.Fatalf("add operator %s: %v", email, err)
		}
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
	sidReq := func(sid string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
		return req
	}
	newAuthUser := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		r.h.SeedAuthUser(t, id.String(), id.String()+"@auth.test")
		return id
	}
	// githubSignIn drives a GitHub sign-in asserting email, which GitHub
	// reports verified, and returns its session id.
	githubSignIn := func(t *testing.T, authID uuid.UUID, email string) string {
		t.Helper()
		claims := validClaimsFor(authID)
		claims["email"] = email
		claims["email_verified"] = true
		resp, _ := r.driveCallbackClaims(claims)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("github callback status = %d; want 302", resp.StatusCode)
		}
		return r.sidFromResp(resp)
	}
	grant := func(t *testing.T, email string) domain.Operator {
		t.Helper()
		list, err := r.srv.allStores.Operators.List(ctx)
		if err != nil {
			t.Fatalf("list operators: %v", err)
		}
		for _, op := range list {
			if op.Email == email {
				return op
			}
		}
		t.Fatalf("no operator grant for %s in %+v", email, list)
		return domain.Operator{}
	}

	// The operator signs in with GitHub, which reports the address verified.
	opAuth := newAuthUser(t)
	opSid := githubSignIn(t, opAuth, opEmail)
	var opPrincipal string
	if err := r.h.AdminDB.QueryRow(`SELECT user_id FROM user_identities WHERE auth_user_id = $1`, opAuth).Scan(&opPrincipal); err != nil {
		t.Fatalf("resolve the operator's principal: %v", err)
	}

	t.Run("the verifying sign-in binds the pending grant", func(t *testing.T) {
		if got := grant(t, opEmail).UserID; got != opPrincipal {
			t.Errorf("grant bound to %q after the operator's sign-in; want %s", got, opPrincipal)
		}
		if !meIsOperator(t, sidReq(opSid)) {
			t.Error("is_operator = false; want true for the principal the grant is bound to")
		}
	})

	t.Run("untrusted sso identity with a pending address is not, and binds nothing", func(t *testing.T) {
		samlAuth := newAuthUser(t)
		c := validClaimsFor(samlAuth)
		c["email"] = pendingEmail
		c["email_verified"] = true
		resp := r.driveSSOCallbackClaims(c, uuid.NewString())
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("sso callback status = %d; want 302", resp.StatusCode)
		}
		if meIsOperator(t, sidReq(r.sidFromResp(resp))) {
			t.Error("is_operator = true for an SSO sign-in whose only claim to the operator address is its assertion")
		}
		if op := grant(t, pendingEmail); !op.Pending() {
			t.Errorf("an untrusted sign-in bound the pending grant to %q", op.UserID)
		}
	})

	t.Run("operator with an api token is still an operator", func(t *testing.T) {
		opPrincipalID := uuid.MustParse(opPrincipal)
		orgID, _ := r.seedOrg(opPrincipalID, "ops-org")
		_, plaintext := r.mintToken(opPrincipalID, orgID, "ops")
		if !meIsOperator(t, r.bearerReq(http.MethodGet, "/api/me", plaintext)) {
			t.Error("is_operator = false for an operator's token; want true")
		}
	})

	t.Run("the grant stays with its principal when the address moves", func(t *testing.T) {
		// The operator's GitHub account now reports another address, which
		// frees the operator address; a different person then signs in
		// holding it verified.
		opSid = githubSignIn(t, opAuth, "former-ops@corp.example")
		successorSid := githubSignIn(t, newAuthUser(t), opEmail)

		if meIsOperator(t, sidReq(successorSid)) {
			t.Error("is_operator = true for the principal that verified the address after the grant was bound")
		}
		if !meIsOperator(t, sidReq(opSid)) {
			t.Error("is_operator = false for the principal the grant is bound to, after its address changed")
		}
		if got := grant(t, opEmail).UserID; got != opPrincipal {
			t.Errorf("grant moved to %q; want it kept on %s", got, opPrincipal)
		}
	})
}
