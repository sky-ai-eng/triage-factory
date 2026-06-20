package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSSOConnectionStore_Postgres_CRUDRoundTrip: create reads back through
// GetByID + ListByOrg with the schema defaults (saml / member / enabled),
// Update flips the mutable fields, and Delete removes the row. All on the
// app pool under the org owner's (admin) claims.
func TestSSOConnectionStore_Postgres_CRUDRoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "sso-conn-crud")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	providerID := uuid.NewString()
	var connID string
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		connID, e = tx.SSOConnections.Create(ctx, domain.CreateSSOConnectionParams{
			OrgID:      orgID,
			ProviderID: providerID, // kind + default_role left empty → defaults
		})
		return e
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if connID == "" {
		t.Fatal("Create returned empty id")
	}

	// GetByID reflects the schema defaults.
	var got *domain.SSOConnection
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = tx.SSOConnections.GetByID(ctx, orgID, connID)
		return e
	})
	if got == nil {
		t.Fatal("GetByID returned nil for a just-created connection")
	}
	if got.Kind != "saml" || got.DefaultRole != "member" || !got.Enabled {
		t.Errorf("defaults wrong: kind=%q role=%q enabled=%v; want saml/member/true",
			got.Kind, got.DefaultRole, got.Enabled)
	}
	if got.ProviderID != providerID {
		t.Errorf("provider_id = %q; want %q", got.ProviderID, providerID)
	}

	// ListByOrg sees exactly the one row.
	var list []domain.SSOConnection
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		list, e = tx.SSOConnections.ListByOrg(ctx, orgID)
		return e
	})
	if len(list) != 1 || list[0].ID != connID {
		t.Fatalf("ListByOrg = %d rows; want 1 (the created connection)", len(list))
	}

	// Update flips the mutable fields ('admin' is a legal default_role; only
	// 'owner' is barred).
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.SSOConnections.Update(ctx, orgID, connID, domain.UpdateSSOConnectionParams{
			DefaultRole: "admin",
			Enabled:     false,
		})
	})
	// The admin-pool GetByProviderID reflects the update.
	b, err := stores.SSOConnections.GetByProviderID(ctx, providerID)
	if err != nil {
		t.Fatalf("GetByProviderID after update: %v", err)
	}
	if b == nil || b.DefaultRole != "admin" || b.Enabled {
		t.Errorf("post-update binding = %+v; want default_role=admin enabled=false", b)
	}

	// Delete removes the row.
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.SSOConnections.Delete(ctx, orgID, connID)
	})
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = tx.SSOConnections.GetByID(ctx, orgID, connID)
		return e
	})
	if got != nil {
		t.Errorf("GetByID after delete = %+v; want nil", got)
	}
}

// TestSSOConnectionStore_Postgres_GetByProviderID: the admin-pool JIT read
// resolves a provider_id to its org binding, and returns (nil, nil) for an
// unbound provider.
func TestSSOConnectionStore_Postgres_GetByProviderID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "sso-by-provider")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	_, providerID := mustSeedConn(t, h, orgID)

	b, err := stores.SSOConnections.GetByProviderID(ctx, providerID)
	if err != nil {
		t.Fatalf("GetByProviderID: %v", err)
	}
	if b == nil {
		t.Fatal("binding not found by provider_id")
	}
	if b.OrgID != orgID {
		t.Errorf("org = %q; want %q", b.OrgID, orgID)
	}
	if b.DefaultRole != "member" {
		t.Errorf("default_role = %q; want member", b.DefaultRole)
	}
	if !b.Enabled {
		t.Error("freshly seeded connection should be enabled")
	}

	none, err := stores.SSOConnections.GetByProviderID(ctx, uuid.NewString())
	if err != nil {
		t.Fatalf("GetByProviderID(unknown): %v", err)
	}
	if none != nil {
		t.Errorf("got %+v; want nil for an unbound provider_id", none)
	}
}

// TestSSOConnectionStore_Postgres_OwnerDefaultRoleRejected: the
// sso_connections_role_not_owner CHECK bars 'owner' as a JIT default role,
// even via the admin pool (which bypasses RLS but not table constraints).
func TestSSOConnectionStore_Postgres_OwnerDefaultRoleRejected(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "sso-owner-role")

	_, err := h.AdminDB.Exec(`
		INSERT INTO sso_connections (org_id, provider_id, default_role)
		VALUES ($1, $2, 'owner')
	`, orgID, uuid.NewString())
	if err == nil {
		t.Fatal("default_role='owner' insert succeeded; want CHECK violation")
	}
}

// TestSSOConnectionStore_Postgres_ProviderIDUnique: a provider_id binds to
// at most one org deployment-wide (sso_connections_provider_uniq) — a second
// connection reusing it, even in a different org, is refused.
func TestSSOConnectionStore_Postgres_ProviderIDUnique(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, _ := pgtest.SeedOrgWithUser(t, h, "sso-uniq-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "sso-uniq-b")

	providerID := uuid.NewString()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO sso_connections (org_id, provider_id) VALUES ($1, $2)
	`, orgA, providerID); err != nil {
		t.Fatalf("first connection insert: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO sso_connections (org_id, provider_id) VALUES ($1, $2)
	`, orgB, providerID); err == nil {
		t.Fatal("cross-org duplicate provider_id insert succeeded; want unique violation")
	}
}

// TestSSODomainStore_Postgres_CRUDRoundTrip: create a pending claim, read it
// back (verified_at nil), stamp it via SetVerified, and delete it. App pool,
// owner (admin) claims.
func TestSSODomainStore_Postgres_CRUDRoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "sso-dom-crud")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	connID, _ := mustSeedConn(t, h, orgID)

	var domID string
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		domID, e = tx.SSODomains.Create(ctx, domain.CreateSSODomainParams{
			ConnectionID: connID, OrgID: orgID, Domain: "Corp.com", Token: "dns-tok",
		})
		return e
	})
	if domID == "" {
		t.Fatal("Create returned empty id")
	}

	var got *domain.SSODomain
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = tx.SSODomains.GetByID(ctx, orgID, domID)
		return e
	})
	if got == nil {
		t.Fatal("GetByID returned nil for a just-created domain")
	}
	if got.VerifiedAt != nil {
		t.Errorf("new claim verified_at = %v; want nil (pending)", got.VerifiedAt)
	}
	if got.Domain != "Corp.com" {
		t.Errorf("domain stored as %q; want verbatim Corp.com", got.Domain)
	}

	// SetVerified stamps verified_at.
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.SSODomains.SetVerified(ctx, orgID, domID)
	})
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = tx.SSODomains.GetByID(ctx, orgID, domID)
		return e
	})
	if got == nil || got.VerifiedAt == nil {
		t.Fatalf("after SetVerified, verified_at still nil: %+v", got)
	}

	// Delete removes the row.
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.SSODomains.Delete(ctx, orgID, domID)
	})
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = tx.SSODomains.GetByID(ctx, orgID, domID)
		return e
	})
	if got != nil {
		t.Errorf("GetByID after delete = %+v; want nil", got)
	}
}

// TestSSODomainStore_Postgres_VerifiedGlobalUnique: two orgs can both hold a
// *pending* claim for the same domain, but the second to verify trips
// sso_domains_verified_global_uniq — the cross-org isolation linchpin.
func TestSSODomainStore_Postgres_VerifiedGlobalUnique(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "sso-glob-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "sso-glob-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	connA, _ := mustSeedConn(t, h, orgA)
	connB, _ := mustSeedConn(t, h, orgB)

	// Both orgs claim corp.com pending — allowed (per-org unique only).
	domA := createDomain(t, stores, ctx, orgA, userA, connA, "corp.com")
	domB := createDomain(t, stores, ctx, orgB, userB, connB, "corp.com")

	// Org A verifies first → OK.
	if err := stores.Tx.WithTx(ctx, orgA, userA, func(tx db.TxStores) error {
		return tx.SSODomains.SetVerified(ctx, orgA, domA)
	}); err != nil {
		t.Fatalf("orgA SetVerified: %v", err)
	}

	// Org B verifies the same domain → global-unique violation.
	if err := stores.Tx.WithTx(ctx, orgB, userB, func(tx db.TxStores) error {
		return tx.SSODomains.SetVerified(ctx, orgB, domB)
	}); err == nil {
		t.Fatal("second org SetVerified succeeded; want verified-global-unique violation")
	}

	// Org A's verified claim is intact; org B's stayed pending (rolled back).
	r, err := stores.SSODomains.GetVerifiedByDomain(ctx, "corp.com")
	if err != nil {
		t.Fatalf("GetVerifiedByDomain: %v", err)
	}
	if r == nil || r.OrgID != orgA {
		t.Fatalf("verified corp.com routes to %+v; want orgA %s", r, orgA)
	}
}

// TestSSODomainStore_Postgres_CrossOrgConnectionRejectedBySchema: the
// tenant-scoped composite FK (connection_id, org_id) → sso_connections
// (id, org_id) makes a domain pointing at another org's connection
// impossible to store, even via the admin pool (which bypasses RLS). This is
// the backstop beneath the RLS INSERT WITH CHECK: RLS pins org_id to the
// writer's own org but can't stop a self-org row from referencing a foreign
// connection_id — only this FK can, and it holds on every write path.
func TestSSODomainStore_Postgres_CrossOrgConnectionRejectedBySchema(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, _ := pgtest.SeedOrgWithUser(t, h, "sso-xorg-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "sso-xorg-b")
	connA, _ := mustSeedConn(t, h, orgA)
	connB, _ := mustSeedConn(t, h, orgB)

	// Same-org domain inserts fine (control).
	if _, err := h.AdminDB.Exec(`
		INSERT INTO sso_domains (connection_id, org_id, domain, token)
		VALUES ($1, $2, 'ok.com', 'tok')
	`, connA, orgA); err != nil {
		t.Fatalf("same-org domain insert should succeed: %v", err)
	}

	// Cross-org: org A's domain pointing at org B's connection is refused by
	// the composite FK, even on the admin pool.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO sso_domains (connection_id, org_id, domain, token)
		VALUES ($1, $2, 'bad.com', 'tok')
	`, connB, orgA); err == nil {
		t.Fatal("cross-org connection_id insert succeeded; want composite-FK violation")
	}
}

// TestSSODomainStore_Postgres_GetVerifiedByDomain_ExactMatch: the routing
// read is exact (corp.com never matches eng.corp.com), case-insensitive, and
// ignores pending rows.
func TestSSODomainStore_Postgres_GetVerifiedByDomain_ExactMatch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "sso-exact")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	connID, providerID := mustSeedConn(t, h, orgID)

	// Verified corp.com; pending eng.corp.com (never routes).
	verifiedID := createDomain(t, stores, ctx, orgID, userID, connID, "corp.com")
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.SSODomains.SetVerified(ctx, orgID, verifiedID)
	}); err != nil {
		t.Fatalf("SetVerified: %v", err)
	}
	createDomain(t, stores, ctx, orgID, userID, connID, "eng.corp.com") // stays pending

	// Exact verified match resolves to the connection + provider.
	r, err := stores.SSODomains.GetVerifiedByDomain(ctx, "corp.com")
	if err != nil {
		t.Fatalf("GetVerifiedByDomain(corp.com): %v", err)
	}
	if r == nil {
		t.Fatal("verified corp.com not found")
	}
	if r.ConnectionID != connID || r.OrgID != orgID || r.ProviderID != providerID {
		t.Errorf("route = %+v; want conn=%s org=%s provider=%s", r, connID, orgID, providerID)
	}

	// Case-insensitive.
	if up, err := stores.SSODomains.GetVerifiedByDomain(ctx, "CORP.COM"); err != nil || up == nil {
		t.Errorf("GetVerifiedByDomain(CORP.COM) = (%+v, %v); want a hit (case-insensitive)", up, err)
	}

	// A pending row never routes — even on an exact match.
	if p, err := stores.SSODomains.GetVerifiedByDomain(ctx, "eng.corp.com"); err != nil || p != nil {
		t.Errorf("GetVerifiedByDomain(eng.corp.com pending) = (%+v, %v); want (nil, nil)", p, err)
	}

	// No suffix / longest-match: a subdomain of a verified domain doesn't hit.
	if sub, err := stores.SSODomains.GetVerifiedByDomain(ctx, "mail.corp.com"); err != nil || sub != nil {
		t.Errorf("GetVerifiedByDomain(mail.corp.com) = (%+v, %v); want (nil, nil) — no suffix match", sub, err)
	}
}

// TestSSOStores_Postgres_RLS_AdminVsNonAdmin: the sso_connections_* /
// sso_domains_* policies gate every app-pool operation on org-admin. A plain
// member sees nothing and can't insert/update/delete either table; an org
// admin can.
func TestSSOStores_Postgres_RLS_AdminVsNonAdmin(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, ownerID, teamID := pgtest.SeedOrgWithUser(t, h, "sso-rls")
	memberID := pgtest.SeedUser(t, h, "sso-rls-member")
	pgtest.AddOrgMember(t, h, memberID, orgID, teamID, "member", "member")

	connID, _ := mustSeedConn(t, h, orgID)
	var domID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO sso_domains (connection_id, org_id, domain, token)
		VALUES ($1, $2, 'corp.com', 'tok') RETURNING id::text
	`, connID, orgID).Scan(&domID); err != nil {
		t.Fatalf("seed sso_domain: %v", err)
	}

	// Non-admin member: SELECT returns nothing (RLS hides both tables).
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		var conns, doms int
		if e := tx.QueryRow(`SELECT count(*) FROM sso_connections`).Scan(&conns); e != nil {
			return e
		}
		if e := tx.QueryRow(`SELECT count(*) FROM sso_domains`).Scan(&doms); e != nil {
			return e
		}
		if conns != 0 || doms != 0 {
			t.Errorf("member sees conns=%d doms=%d; want 0/0 (RLS hides)", conns, doms)
		}
		return nil
	}); err != nil {
		t.Fatalf("member select tx: %v", err)
	}

	// Non-admin member: INSERT is rejected by the WITH CHECK.
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO sso_connections (org_id, provider_id) VALUES ($1, $2)`,
			orgID, uuid.NewString())
		return e
	}); err == nil {
		t.Error("member INSERT into sso_connections succeeded; want RLS rejection")
	}

	// Non-admin member: UPDATE / DELETE match no rows (USING hides them).
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE sso_connections SET enabled = false WHERE id = $1`, connID)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("member UPDATE sso_connections affected %d rows; want 0", n)
		}
		res, e = tx.Exec(`DELETE FROM sso_domains WHERE id = $1`, domID)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("member DELETE sso_domains affected %d rows; want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("member modify tx: %v", err)
	}

	// Org admin (owner): SELECT sees both rows.
	if err := h.WithUser(t, ownerID, orgID, func(tx *sql.Tx) error {
		var conns, doms int
		if e := tx.QueryRow(`SELECT count(*) FROM sso_connections`).Scan(&conns); e != nil {
			return e
		}
		if e := tx.QueryRow(`SELECT count(*) FROM sso_domains`).Scan(&doms); e != nil {
			return e
		}
		if conns != 1 || doms != 1 {
			t.Errorf("admin sees conns=%d doms=%d; want 1/1", conns, doms)
		}
		return nil
	}); err != nil {
		t.Fatalf("admin select tx: %v", err)
	}

	// Org admin: UPDATE matches the row.
	if err := h.WithUser(t, ownerID, orgID, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE sso_connections SET enabled = false WHERE id = $1`, connID)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("admin UPDATE sso_connections affected %d rows; want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("admin update tx: %v", err)
	}
}

// mustWithUser runs fn inside a claims-bound app-pool tx and fails the test
// on error — sugar for the store-method round-trips that don't need to
// inspect the WithTx error beyond "it worked".
func mustWithUser(t *testing.T, stores db.Stores, ctx context.Context, orgID, userID string, fn func(db.TxStores) error) {
	t.Helper()
	if err := stores.Tx.WithTx(ctx, orgID, userID, fn); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// mustSeedConn inserts an sso_connections row via the admin pool (fixture,
// bypasses RLS) and returns (id, provider_id). The provider_id is a fresh
// UUID, matching the GoTrue shape the spike confirmed.
func mustSeedConn(t *testing.T, h *pgtest.Harness, orgID string) (id, providerID string) {
	t.Helper()
	providerID = uuid.NewString()
	if err := h.AdminDB.QueryRow(`
		INSERT INTO sso_connections (org_id, provider_id) VALUES ($1, $2)
		RETURNING id::text
	`, orgID, providerID).Scan(&id); err != nil {
		t.Fatalf("seed sso_connection: %v", err)
	}
	return id, providerID
}

// createDomain creates a pending sso_domains claim through the store (app
// pool, the org's admin claims) and returns its id.
func createDomain(t *testing.T, stores db.Stores, ctx context.Context, orgID, userID, connID, dom string) string {
	t.Helper()
	var id string
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		id, e = tx.SSODomains.Create(ctx, domain.CreateSSODomainParams{
			ConnectionID: connID, OrgID: orgID, Domain: dom, Token: "tok-" + dom,
		})
		return e
	}); err != nil {
		t.Fatalf("createDomain(%s): %v", dom, err)
	}
	return id
}
