package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Signup provisions no tenant; creating an org is a deliberate user action.
// The OAuth callback resolves an existing membership and never mints orgs,
// teams or memberships.

// TestLookupEarliestMembership_ZeroMemberships: a user with no memberships
// resolves to an invalid NullUUID, so the session lands with a NULL
// active_org_id and the frontend routes them to onboarding.
func TestLookupEarliestMembership_ZeroMemberships(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()

	got, err := r.srv.lookupEarliestMembership(context.Background(), r.srv.db, userID, "")
	if err != nil {
		t.Fatalf("lookupEarliestMembership: %v", err)
	}
	if got.Valid {
		t.Errorf("got valid org %s; want invalid for a membership-less user", got.UUID)
	}
}

// TestLookupEarliestMembership_ReturnsEarliest pins the returning-user
// fast path: the session defaults to the user's earliest membership.
func TestLookupEarliestMembership_ReturnsEarliest(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "earliest-org")

	got, err := r.srv.lookupEarliestMembership(context.Background(), r.srv.db, userID, "")
	if err != nil {
		t.Fatalf("lookupEarliestMembership: %v", err)
	}
	if !got.Valid || got.UUID != orgID {
		t.Errorf("got %v; want %s (the user's only membership)", got, orgID)
	}
}

// TestSignupCallback_CreatesNoTenant: the OAuth callback for a user with no
// memberships creates no org and no membership rows, and the session lands with
// a NULL active_org_id.
func TestSignupCallback_CreatesNoTenant(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()

	resp, _ := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}

	// No org owned by, and no membership rows for, this user.
	var ownedOrgs int
	if err := r.h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM public.orgs WHERE owner_user_id = $1`, userID,
	).Scan(&ownedOrgs); err != nil {
		t.Fatalf("count owned orgs: %v", err)
	}
	if ownedOrgs != 0 {
		t.Errorf("orgs owned by fresh user = %d, want 0 (signup provisions nothing)", ownedOrgs)
	}
	for _, table := range []string{"org_memberships", "memberships"} {
		var n int
		if err := r.h.AdminDB.QueryRow(
			`SELECT COUNT(*) FROM public.`+table+` WHERE user_id = $1`, userID,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s rows for fresh user = %d, want 0", table, n)
		}
	}

	// Session lands with active_org_id NULL.
	sid := r.sidFromResp(resp)
	var activeOrg uuid.NullUUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT active_org_id FROM public.sessions WHERE id = $1`, uuid.MustParse(sid),
	).Scan(&activeOrg); err != nil {
		t.Fatalf("select active_org_id: %v", err)
	}
	if activeOrg.Valid {
		t.Errorf("active_org_id = %s, want NULL (no memberships)", activeOrg.UUID)
	}
}

// TestSignupCallback_ExistingMembership_DefaultsToEarliest pins the
// returning-user end-to-end path: a user who already belongs to an org
// gets a session defaulted to it, with no new tenant minted.
func TestSignupCallback_ExistingMembership_DefaultsToEarliest(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "returning-org")

	resp, _ := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}

	sid := r.sidFromResp(resp)
	var activeOrg uuid.NullUUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT active_org_id FROM public.sessions WHERE id = $1`, uuid.MustParse(sid),
	).Scan(&activeOrg); err != nil {
		t.Fatalf("select active_org_id: %v", err)
	}
	if !activeOrg.Valid || activeOrg.UUID != orgID {
		t.Errorf("active_org_id = %v, want %s (existing membership)", activeOrg, orgID)
	}

	// Still exactly the one org the test seeded — signup minted nothing.
	var ownedOrgs int
	if err := r.h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM public.orgs WHERE owner_user_id = $1`, userID,
	).Scan(&ownedOrgs); err != nil {
		t.Fatalf("count owned orgs: %v", err)
	}
	if ownedOrgs != 1 {
		t.Errorf("orgs owned by returning user = %d, want 1 (the seeded org only)", ownedOrgs)
	}
}
