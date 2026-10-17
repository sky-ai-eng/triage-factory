package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestGate_Postgres_OperatorFromVerifiedIdentity drives the multi-mode gate
// against real login identities. Every caller's token claims the operator
// address, because that is what an identity provider can assert; the gate must
// admit only the principal whose identity holds it with TF's trust.
func TestGate_Postgres_OperatorFromVerifiedIdentity(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	const opEmail = "ops@corp.example"
	if err := stores.Operators.Add(context.Background(), opEmail, "test"); err != nil {
		t.Fatalf("add operator: %v", err)
	}

	// The operator's GitHub identity holds the address verified.
	operator := pgtest.SeedUser(t, h, "operator")
	pgtest.SeedIdentity(t, h, operator, opEmail, true)

	// The impostor's only identity with the address is a SAML sign-in on a
	// domain nobody verified for its connection: recorded untrusted.
	impostor := pgtest.SeedUser(t, h, "impostor")
	var samlAuth string
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()::text`).Scan(&samlAuth); err != nil {
		t.Fatalf("gen auth id: %v", err)
	}
	h.SeedAuthUser(t, samlAuth, samlAuth+"@auth.test")
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $2, 'saml', $3, false)
	`, samlAuth, impostor, opEmail)

	hd := &handler{stores: stores}
	gate := func(t *testing.T, subject string, token bool) (*verify.Claims, int) {
		t.Helper()
		ctx := httpx.WithClaims(context.Background(), &verify.Claims{Subject: subject, Email: opEmail})
		if token {
			ctx = httpx.WithTokenAuth(ctx, &httpx.TokenAuth{TokenID: "tok-1", OrgID: "org-1"})
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/fleet/overview", nil).WithContext(ctx)
		claims := hd.gate(rec, req)
		return claims, rec.Code
	}

	t.Run("verified github identity passes", func(t *testing.T) {
		if claims, code := gate(t, operator, false); claims == nil {
			t.Fatalf("operator refused with %d; want admitted", code)
		}
	})

	t.Run("untrusted saml identity is hidden", func(t *testing.T) {
		claims, code := gate(t, impostor, false)
		if claims != nil || code != http.StatusNotFound {
			t.Fatalf("impostor: admitted=%v code=%d; want refused with 404", claims != nil, code)
		}
	})

	t.Run("operator with an api token is refused 403", func(t *testing.T) {
		claims, code := gate(t, operator, true)
		if claims != nil || code != http.StatusForbidden {
			t.Fatalf("token-authed operator: admitted=%v code=%d; want refused with 403", claims != nil, code)
		}
	})
}
