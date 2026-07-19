package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Fixed UUIDs for the auth_events SQLite tests. SQLite has no FK on these
// columns (the table is parity-only), so arbitrary uuids stand in for org / user
// / session without seeding anything.
const (
	authOrgA  = "11111111-1111-1111-1111-111111111111"
	authOrgB  = "44444444-4444-4444-4444-444444444444"
	authUserA = "22222222-2222-2222-2222-222222222222"
	authUserB = "55555555-5555-5555-5555-555555555555"
	authSessA = "33333333-3333-3333-3333-333333333333"
)

// TestAuthEventStore_SQLite_RoundTripAndNulls pins that RecordSystem inserts a
// row whose every field reads back intact, that an empty id is generated, and
// that the empty optional columns (org/user/session/ip/ua/detail — a pre-auth
// failure) land as SQL NULL. TFAC-76.
func TestAuthEventStore_SQLite_RoundTripAndNulls(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	in := domain.AuthEvent{
		OrgID:      authOrgA,
		UserID:     authUserA,
		SessionID:  authSessA,
		EventType:  domain.AuthEventLoginSuccess,
		IPAddress:  "203.0.113.7",
		UserAgent:  "Mozilla/5.0 (test)",
		DetailJSON: `{"method":"github","sso":false}`,
	}
	if err := stores.AuthEvents.RecordSystem(ctx, in); err != nil {
		t.Fatalf("RecordSystem: %v", err)
	}

	got, err := stores.AuthEvents.ListByOrgSystem(ctx, authOrgA, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	a := got[0]
	if a.ID == "" || a.CreatedAt.IsZero() {
		t.Errorf("expected generated id + created_at, got id=%q created=%v", a.ID, a.CreatedAt)
	}
	if a.OrgID != authOrgA || a.UserID != authUserA || a.SessionID != authSessA ||
		a.EventType != domain.AuthEventLoginSuccess || a.IPAddress != "203.0.113.7" ||
		a.UserAgent != "Mozilla/5.0 (test)" || a.DetailJSON != `{"method":"github","sso":false}` {
		t.Errorf("round-trip mismatch: %+v", a)
	}

	// A pre-auth failure: empty org/user/session/ip/ua → SQL NULL.
	if err := stores.AuthEvents.RecordSystem(ctx, domain.AuthEvent{
		EventType:  domain.AuthEventLoginFailure,
		DetailJSON: `{"reason":"missing_state_cookie"}`,
	}); err != nil {
		t.Fatalf("RecordSystem (nulls): %v", err)
	}
	var org, user, sess, ip, ua sql.NullString
	if err := conn.QueryRow(
		`SELECT org_id, user_id, session_id, ip_address, user_agent
		   FROM auth_events WHERE event_type = ?`, domain.AuthEventLoginFailure,
	).Scan(&org, &user, &sess, &ip, &ua); err != nil {
		t.Fatalf("read back null row: %v", err)
	}
	if org.Valid || user.Valid || sess.Valid || ip.Valid || ua.Valid {
		t.Errorf("empty optionals should be NULL: org=%v user=%v sess=%v ip=%v ua=%v", org, user, sess, ip, ua)
	}
}

// TestAuthEventStore_SQLite_ListScoping pins that ListByOrgSystem EXCLUDES the
// NULL-org rows (they belong to no org) and scopes to the requested org, and
// that ListByUserSystem scopes to the requested user (excluding NULL-user rows).
// TFAC-76.
func TestAuthEventStore_SQLite_ListScoping(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	seed := []domain.AuthEvent{
		{OrgID: authOrgA, UserID: authUserA, SessionID: authSessA, EventType: domain.AuthEventLoginSuccess},
		{OrgID: authOrgA, UserID: authUserB, EventType: domain.AuthEventLogout},
		{OrgID: authOrgB, UserID: authUserB, EventType: domain.AuthEventLoginSuccess},
		// A pre-auth failure with NO org and NO user — must never surface in an
		// org- or user-scoped read.
		{EventType: domain.AuthEventLoginFailure, DetailJSON: `{"reason":"state_parse_failed"}`},
	}
	for i, e := range seed {
		if err := stores.AuthEvents.RecordSystem(ctx, e); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	orgA, err := stores.AuthEvents.ListByOrgSystem(ctx, authOrgA, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem(A): %v", err)
	}
	if len(orgA) != 2 {
		t.Errorf("org A returned %d rows, want 2 (the NULL-org failure is excluded)", len(orgA))
	}
	for _, e := range orgA {
		if e.OrgID != authOrgA {
			t.Errorf("org A read leaked a row with org=%q", e.OrgID)
		}
	}

	byUserB, err := stores.AuthEvents.ListByUserSystem(ctx, authUserB, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByUserSystem(B): %v", err)
	}
	if len(byUserB) != 2 {
		t.Errorf("user B returned %d rows, want 2 (excludes the NULL-user failure)", len(byUserB))
	}
	for _, e := range byUserB {
		if e.UserID != authUserB {
			t.Errorf("user B read leaked a row with user=%q", e.UserID)
		}
	}
}

// TestAuthEventStore_SQLite_AppendOnly pins that two RecordSystem calls with the
// same logical content produce two distinct rows (server-generated uuid ids) —
// the audit log never collapses repeated authentication outcomes. TFAC-76.
func TestAuthEventStore_SQLite_AppendOnly(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	repeat := domain.AuthEvent{OrgID: authOrgA, UserID: authUserA, EventType: domain.AuthEventLoginFailure}
	for i := 0; i < 2; i++ {
		if err := stores.AuthEvents.RecordSystem(ctx, repeat); err != nil {
			t.Fatalf("RecordSystem %d: %v", i, err)
		}
	}
	rows, err := stores.AuthEvents.ListByOrgSystem(ctx, authOrgA, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("two repeated records should both append, got %d rows", len(rows))
	}
	if rows[0].ID == rows[1].ID || rows[0].ID == "" {
		t.Errorf("rows must carry distinct non-empty generated ids: %q vs %q", rows[0].ID, rows[1].ID)
	}
}

// TestAuthEventStore_SQLite_SeatUsage pins SeatUsageSystem: it counts DISTINCT
// login_success users in the window, ignores non-login_success rows and
// NULL-user rows, and reports whether a given user is among them.
func TestAuthEventStore_SQLite_SeatUsage(t *testing.T) {
	conn := newSQLiteForArtifactTestTimed(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	authUserC := "66666666-6666-6666-6666-666666666666"
	// user A logs in twice (still ONE distinct seat), user B once. A logout by
	// user B and a NULL-user login_failure must not count as seats.
	seeds := []struct {
		user, eventType, when string
	}{
		{authUserA, domain.AuthEventLoginSuccess, "2026-07-02 09:00:00"},
		{authUserA, domain.AuthEventLoginSuccess, "2026-07-10 09:00:00"},
		{authUserB, domain.AuthEventLoginSuccess, "2026-07-11 09:00:00"},
		{authUserB, domain.AuthEventLogout, "2026-07-12 09:00:00"},
		{"", domain.AuthEventLoginFailure, "2026-07-13 09:00:00"},
	}
	for i, s := range seeds {
		sid := authSessA[:len(authSessA)-1] + string(rune('a'+i))
		if err := stores.AuthEvents.RecordSystem(ctx, domain.AuthEvent{
			OrgID: authOrgA, UserID: s.user, SessionID: sid, EventType: s.eventType,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if _, err := conn.Exec(`UPDATE auth_events SET created_at = ? WHERE session_id = ?`, s.when, sid); err != nil {
			t.Fatalf("pin created_at: %v", err)
		}
	}

	monthStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// Over the whole month: 2 distinct seats (A, B). A is active; C is not.
	distinct, active, err := stores.AuthEvents.SeatUsageSystem(ctx, monthStart, authUserA)
	if err != nil {
		t.Fatalf("SeatUsageSystem(A): %v", err)
	}
	if distinct != 2 || !active {
		t.Errorf("month/A: got (distinct=%d, active=%v), want (2, true)", distinct, active)
	}
	distinct, active, err = stores.AuthEvents.SeatUsageSystem(ctx, monthStart, authUserC)
	if err != nil {
		t.Fatalf("SeatUsageSystem(C): %v", err)
	}
	if distinct != 2 || active {
		t.Errorf("month/C: got (distinct=%d, active=%v), want (2, false)", distinct, active)
	}

	// A tighter window that starts after A's logins: only B counts, A inactive.
	cutoff := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	distinct, active, err = stores.AuthEvents.SeatUsageSystem(ctx, cutoff, authUserA)
	if err != nil {
		t.Fatalf("SeatUsageSystem(cutoff/A): %v", err)
	}
	if distinct != 1 || active {
		t.Errorf("cutoff/A: got (distinct=%d, active=%v), want (1, false)", distinct, active)
	}
}

// TestAuthEventStore_SQLite_FiltersAndPaging pins the event_type filter, the
// half-open since/until time window, newest-first ordering, and limit/offset
// paging — over a _time_format=sqlite conn so a bound Since/Until compares
// against created_at correctly. TFAC-76.
func TestAuthEventStore_SQLite_FiltersAndPaging(t *testing.T) {
	conn := newSQLiteForArtifactTestTimed(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	seeds := []struct {
		eventType, when string
	}{
		{domain.AuthEventLoginSuccess, "2026-06-01 00:00:00"},
		{domain.AuthEventLoginFailure, "2026-06-02 00:00:00"},
		{domain.AuthEventLoginSuccess, "2026-06-03 00:00:00"},
		{domain.AuthEventLogout, "2026-06-04 00:00:00"},
		{domain.AuthEventLoginSuccess, "2026-06-05 00:00:00"},
	}
	for i, s := range seeds {
		// A distinct session id per row so the created_at pin targets one row.
		sid := authSessA[:len(authSessA)-1] + string(rune('a'+i))
		if err := stores.AuthEvents.RecordSystem(ctx, domain.AuthEvent{
			OrgID: authOrgA, UserID: authUserA, SessionID: sid, EventType: s.eventType,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if _, err := conn.Exec(`UPDATE auth_events SET created_at = ? WHERE session_id = ?`, s.when, sid); err != nil {
			t.Fatalf("pin created_at: %v", err)
		}
	}

	all, err := stores.AuthEvents.ListByOrgSystem(ctx, authOrgA, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("want 5 rows, got %d", len(all))
	}
	// Newest-first: the 06-05 login_success leads, the 06-01 login_success trails.
	if !all[0].CreatedAt.After(all[4].CreatedAt) {
		t.Errorf("not newest-first: first=%v last=%v", all[0].CreatedAt, all[4].CreatedAt)
	}

	// event_type filter: 3 login_success rows.
	successes, _ := stores.AuthEvents.ListByOrgSystem(ctx, authOrgA, domain.AuthEventListOpts{EventType: domain.AuthEventLoginSuccess})
	if len(successes) != 3 {
		t.Errorf("event_type=login_success returned %d, want 3", len(successes))
	}

	// Time window [06-02, 06-04): the login_failure (06-02) + login_success (06-03).
	windowed, _ := stores.AuthEvents.ListByOrgSystem(ctx, authOrgA, domain.AuthEventListOpts{
		Since: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
	})
	if len(windowed) != 2 {
		t.Fatalf("time window returned %d, want 2", len(windowed))
	}
	if windowed[0].EventType != domain.AuthEventLoginSuccess || windowed[1].EventType != domain.AuthEventLoginFailure {
		t.Errorf("windowed order = [%q,%q], want [login_success(06-03), login_failure(06-02)]",
			windowed[0].EventType, windowed[1].EventType)
	}

	// Paging: limit 2, offset 2 over 5 rows → 2 rows; offset 4 → 1 row.
	page2, err := stores.AuthEvents.ListByUserSystem(ctx, authUserA, domain.AuthEventListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListByUserSystem paging: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("limit 2 offset 2 returned %d, want 2", len(page2))
	}
	last, _ := stores.AuthEvents.ListByUserSystem(ctx, authUserA, domain.AuthEventListOpts{Limit: 2, Offset: 4})
	if len(last) != 1 {
		t.Errorf("limit 2 offset 4 returned %d, want 1", len(last))
	}
}
