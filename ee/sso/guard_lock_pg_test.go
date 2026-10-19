package sso_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The Require SSO guards check one table and write another, so two of them
// running at once could each pass against the state the other is replacing.
// Every guarded change takes the org's connection lock before it reads. Each
// test here holds that lock in an open transaction, makes the competing change
// inside it, and shows that the request waits for the lock and then decides
// against the change once it is committed.

// holdConnectionLock opens an admin-pool transaction that row-locks orgID's
// connections, as each guarded change does, then applies change inside it,
// uncommitted.
func (r *authRig) holdConnectionLock(orgID uuid.UUID, change string, args ...any) *sql.Tx {
	r.t.Helper()
	tx, err := r.h.AdminDB.BeginTx(context.Background(), nil)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	r.t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`SELECT id FROM sso_connections WHERE org_id = $1 FOR UPDATE`, orgID); err != nil {
		r.t.Fatalf("lock connections: %v", err)
	}
	if _, err := tx.Exec(change, args...); err != nil {
		r.t.Fatalf("competing change: %v", err)
	}
	return tx
}

// waitsForCommit fires the request in the background, checks it is still
// waiting while tx holds the lock, commits tx, and returns the response.
func waitsForCommit(t *testing.T, tx *sql.Tx, fire func() *http.Response) *http.Response {
	t.Helper()
	done := make(chan *http.Response, 1)
	go func() { done <- fire() }()
	select {
	case resp := <-done:
		t.Fatalf("request finished (status=%d) while another change held the lock; it must wait", resp.StatusCode)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit competing change: %v", err)
	}
	select {
	case resp := <-done:
		return resp
	case <-time.After(10 * time.Second):
		t.Fatal("request never finished after the lock was released")
		return nil
	}
}

// guardRig is an org whose connection is enabled and tested, with one verified
// domain, signed into as its owner, who is the sole break-glass principal.
func guardRig(t *testing.T) (r *authRig, orgID, owner uuid.UUID, providerID, sid string) {
	r = newAuthRig(t)
	owner = r.seedUser()
	orgID, providerID = r.seedRequiringOrg(owner, "acme", "corp.example")
	r.addBreakGlass(orgID, owner)
	sid = r.signInAs(owner)
	return r, orgID, owner, providerID, sid
}

func want409(t *testing.T, what string, resp *http.Response, message string) {
	t.Helper()
	body := readBody(resp)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, message) {
		t.Errorf("%s: status=%d body=%s, want 409 %q", what, resp.StatusCode, body, message)
	}
}

func TestGuardLock_DomainDeleteWaitsForEnforcement(t *testing.T) {
	r, orgID, _, providerID, sid := guardRig(t)
	var domainID string
	if err := r.h.AdminDB.QueryRow(`SELECT id::text FROM sso_domains WHERE org_id = $1`, orgID).Scan(&domainID); err != nil {
		t.Fatalf("read domain: %v", err)
	}

	tx := r.holdConnectionLock(orgID, `UPDATE sso_connections SET enforced = true WHERE org_id = $1`, orgID)
	resp := waitsForCommit(t, tx, func() *http.Response {
		return doReq(r, http.MethodDelete, "/api/sso/domains/"+domainID, sid, nil)
	})

	want409(t, "delete the last verified domain", resp, "Turn off Require SSO first.")
	if !connEnforced(t, r, providerID) {
		t.Fatal("the competing enable did not land")
	}
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_domains WHERE org_id = $1 AND verified_at IS NOT NULL`, orgID).Scan(&n); err != nil {
		t.Fatalf("count domains: %v", err)
	}
	if n != 1 {
		t.Errorf("verified domains=%d, want 1: SSO is required with nothing left to recognise", n)
	}
}

func TestGuardLock_EnforcementWaitsForDomainDelete(t *testing.T) {
	r, orgID, _, providerID, sid := guardRig(t)

	tx := r.holdConnectionLock(orgID, `DELETE FROM sso_domains WHERE org_id = $1`, orgID)
	resp := waitsForCommit(t, tx, func() *http.Response {
		return doReq(r, http.MethodPatch, "/api/sso/connection/enforcement", sid, map[string]bool{"enforced": true})
	})

	want409(t, "require SSO", resp, "verify a domain before requiring SSO")
	if connEnforced(t, r, providerID) {
		t.Error("SSO is required with no verified domain")
	}
}

func TestGuardLock_DisableWaitsForEnforcement(t *testing.T) {
	r, orgID, _, providerID, sid := guardRig(t)

	tx := r.holdConnectionLock(orgID, `UPDATE sso_connections SET enforced = true WHERE org_id = $1`, orgID)
	resp := waitsForCommit(t, tx, func() *http.Response {
		return doReq(r, http.MethodPatch, "/api/sso/connection", sid, map[string]bool{"enabled": false})
	})

	want409(t, "disable the connection", resp, "Turn off Require SSO first.")
	if !connEnabled(t, r, providerID) {
		t.Error("the connection is disabled while SSO is required, which stops the requirement applying")
	}
}

func TestGuardLock_EnforcementWaitsForDisable(t *testing.T) {
	r, orgID, _, providerID, sid := guardRig(t)

	tx := r.holdConnectionLock(orgID, `UPDATE sso_connections SET enabled = false WHERE org_id = $1`, orgID)
	resp := waitsForCommit(t, tx, func() *http.Response {
		return doReq(r, http.MethodPatch, "/api/sso/connection/enforcement", sid, map[string]bool{"enforced": true})
	})

	want409(t, "require SSO", resp, "enable the connection before requiring SSO")
	if connEnforced(t, r, providerID) {
		t.Error("SSO is required on a disabled connection, where it silently does not apply")
	}
}

func TestGuardLock_BreakGlassRemovalWaitsForEnforcement(t *testing.T) {
	r, orgID, owner, _, sid := guardRig(t)

	tx := r.holdConnectionLock(orgID, `UPDATE sso_connections SET enforced = true WHERE org_id = $1`, orgID)
	resp := waitsForCommit(t, tx, func() *http.Response {
		return doReq(r, http.MethodDelete, "/api/sso/break-glass/"+owner.String(), sid, nil)
	})

	want409(t, "remove the last break-glass principal", resp, "can't remove the last break-glass principal")
	if n := breakGlassCount(t, r, orgID); n != 1 {
		t.Errorf("break-glass count=%d, want 1: SSO is required with no recovery path", n)
	}
}

func connEnabled(t *testing.T, r *authRig, providerID string) bool {
	t.Helper()
	var enabled bool
	if err := r.h.AdminDB.QueryRow(
		`SELECT enabled FROM sso_connections WHERE provider_id = $1`, providerID,
	).Scan(&enabled); err != nil {
		t.Fatalf("read enabled: %v", err)
	}
	return enabled
}
