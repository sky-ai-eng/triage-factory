package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// seedAuthExpiredSession inserts a public.sessions row for userID that is already
// older than any plausible retention window (so ReapExpiredSystem deletes it),
// with dummy ciphertext blobs — the retention test never decrypts it. created_at
// is backdated to satisfy the (expires_at > created_at) + (jwt_expires_at <=
// expires_at) CHECKs.
func seedAuthExpiredSession(t *testing.T, h *pgtest.Harness, sessID, userID string) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO public.sessions
			(id, user_id, jwt_enc, jwt_nonce, refresh_token_enc, refresh_nonce,
			 jwt_expires_at, expires_at, created_at, last_seen_at)
		VALUES ($1, $2, '\x00', '\x00', '\x00', '\x00',
			now() - interval '61 days', now() - interval '60 days',
			now() - interval '62 days', now() - interval '60 days')
	`, sessID, userID)
}

// TestAuthEventStore_Postgres_RoundTripAndNulls drives RecordSystem +
// ListByOrgSystem against the AdminDB-wired store (RLS bypassed — this is a
// system-only table). Pins every field round-trips (detail_json via parse, since
// jsonb normalizes its text form), an empty id is server-generated, and an
// all-empty pre-auth failure lands org/user/session/ip/ua as SQL NULL. TFAC-76.
func TestAuthEventStore_Postgres_RoundTripAndNulls(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	sessID := "66666666-6666-6666-6666-666666666666"
	seedAuthExpiredSession(t, h, sessID, userID)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	in := domain.AuthEvent{
		OrgID:      orgID,
		UserID:     userID,
		SessionID:  sessID,
		EventType:  domain.AuthEventLoginSuccess,
		IPAddress:  "203.0.113.7",
		UserAgent:  "Mozilla/5.0 (test)",
		DetailJSON: `{"method":"github","sso":false}`,
	}
	if err := stores.AuthEvents.RecordSystem(ctx, in); err != nil {
		t.Fatalf("RecordSystem: %v", err)
	}

	got, err := stores.AuthEvents.ListByOrgSystem(ctx, orgID, domain.AuthEventListOpts{})
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
	if a.OrgID != orgID || a.UserID != userID || a.SessionID != sessID ||
		a.EventType != domain.AuthEventLoginSuccess || a.IPAddress != "203.0.113.7" ||
		a.UserAgent != "Mozilla/5.0 (test)" {
		t.Errorf("round-trip mismatch: %+v", a)
	}
	// jsonb normalizes the text form (key order / whitespace), so parse + compare.
	var detail map[string]any
	if err := json.Unmarshal([]byte(a.DetailJSON), &detail); err != nil {
		t.Fatalf("detail not valid json: %q (%v)", a.DetailJSON, err)
	}
	if detail["method"] != "github" || detail["sso"] != false {
		t.Errorf("detail = %v, want method=github sso=false", detail)
	}

	// A pre-auth failure: empty org/user/session/ip/ua → SQL NULL.
	if err := stores.AuthEvents.RecordSystem(ctx, domain.AuthEvent{
		EventType:  domain.AuthEventLoginFailure,
		DetailJSON: `{"reason":"missing_state_cookie"}`,
	}); err != nil {
		t.Fatalf("RecordSystem (nulls): %v", err)
	}
	var org, user, sess, ip, ua sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT org_id, user_id, session_id, host(ip_address), user_agent
		   FROM auth_events WHERE event_type = $1`, domain.AuthEventLoginFailure,
	).Scan(&org, &user, &sess, &ip, &ua); err != nil {
		t.Fatalf("read back null row: %v", err)
	}
	if org.Valid || user.Valid || sess.Valid || ip.Valid || ua.Valid {
		t.Errorf("empty optionals should be NULL: org=%v user=%v sess=%v ip=%v ua=%v", org, user, sess, ip, ua)
	}
}

// TestAuthEventStore_Postgres_ListScoping pins that ListByOrgSystem EXCLUDES the
// NULL-org rows and scopes to the requested org, and ListByUserSystem scopes to
// the requested user. TFAC-76.
func TestAuthEventStore_Postgres_ListScoping(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "bob")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	seed := []domain.AuthEvent{
		{OrgID: orgA, UserID: userA, EventType: domain.AuthEventLoginSuccess},
		{OrgID: orgA, UserID: userB, EventType: domain.AuthEventLogout},
		{OrgID: orgB, UserID: userB, EventType: domain.AuthEventLoginSuccess},
		// Pre-auth failure: NULL org + NULL user — never in an org/user read.
		{EventType: domain.AuthEventLoginFailure, DetailJSON: `{"reason":"state_parse_failed"}`},
	}
	for i, e := range seed {
		if err := stores.AuthEvents.RecordSystem(ctx, e); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	gotA, err := stores.AuthEvents.ListByOrgSystem(ctx, orgA, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem(A): %v", err)
	}
	if len(gotA) != 2 {
		t.Errorf("org A returned %d rows, want 2 (NULL-org failure excluded)", len(gotA))
	}

	byUserB, err := stores.AuthEvents.ListByUserSystem(ctx, userB, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByUserSystem(B): %v", err)
	}
	if len(byUserB) != 2 {
		t.Errorf("user B returned %d rows, want 2 (NULL-user failure excluded)", len(byUserB))
	}

	// event_type filter + paging round-trip on the admin pool.
	successes, _ := stores.AuthEvents.ListByOrgSystem(ctx, orgA, domain.AuthEventListOpts{EventType: domain.AuthEventLoginSuccess})
	if len(successes) != 1 || successes[0].EventType != domain.AuthEventLoginSuccess {
		t.Errorf("event_type filter returned %+v, want one login_success", successes)
	}
	page := stores.AuthEvents
	first, _ := page.ListByOrgSystem(ctx, orgA, domain.AuthEventListOpts{Limit: 1, Offset: 0})
	second, _ := page.ListByOrgSystem(ctx, orgA, domain.AuthEventListOpts{Limit: 1, Offset: 1})
	if len(first) != 1 || len(second) != 1 || first[0].ID == second[0].ID {
		t.Errorf("paging overlap: first=%v second=%v", first, second)
	}
}

// TestAuthEventStore_Postgres_SeatClaims pins ClaimSeatSystem's sequential
// semantics: idempotent per (period, user), the cap blocks a new distinct
// claimant, a returning claimant is always admitted, and the period is an
// independent bucket.
func TestAuthEventStore_Postgres_SeatClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	_, userA, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	_, userB, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	_, userC, _ := pgtest.SeedOrgWithUser(t, h, "carol")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	claim := func(period, user string, cap int) (bool, int) {
		t.Helper()
		adm, seats, err := stores.AuthEvents.ClaimSeatSystem(ctx, period, user, cap)
		if err != nil {
			t.Fatalf("ClaimSeatSystem(%s,%s): %v", period, user, err)
		}
		return adm, seats
	}

	const period = "2026-07"
	if adm, seats := claim(period, userA, 2); !adm || seats != 1 {
		t.Fatalf("A first claim = (%v, %d), want (true, 1)", adm, seats)
	}
	if adm, seats := claim(period, userA, 2); !adm || seats != 1 {
		t.Fatalf("A repeat claim = (%v, %d), want (true, 1) — idempotent", adm, seats)
	}
	if adm, seats := claim(period, userB, 2); !adm || seats != 2 {
		t.Fatalf("B claim = (%v, %d), want (true, 2)", adm, seats)
	}
	if adm, seats := claim(period, userC, 2); adm || seats != 2 {
		t.Fatalf("C over-cap claim = (%v, %d), want (false, 2)", adm, seats)
	}
	if adm, _ := claim(period, userA, 2); !adm {
		t.Fatal("A returning claim must be admitted even at cap")
	}
	if adm, seats := claim("2026-08", userC, 2); !adm || seats != 1 {
		t.Fatalf("C next-period claim = (%v, %d), want (true, 1)", adm, seats)
	}
}

// TestAuthEventStore_Postgres_SeatClaims_Concurrent is the TOCTOU regression
// guard: N distinct users race to claim seats against a cap of K, and EXACTLY K
// are admitted — proving the count+insert is atomic under real concurrency (the
// per-period advisory lock serializes claim evaluations so two first-logins can't
// both observe an under-cap count and both slip past the cap).
func TestAuthEventStore_Postgres_SeatClaims_Concurrent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	const cap = 3
	const n = 16
	users := make([]string, n)
	for i := range users {
		_, u, _ := pgtest.SeedOrgWithUser(t, h, fmt.Sprintf("racer%d", i))
		users[i] = u
	}

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	const period = "2026-09"

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	var firstErr error
	for _, u := range users {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			adm, _, err := stores.AuthEvents.ClaimSeatSystem(context.Background(), period, uid, cap)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if adm {
				admitted++
			}
		}(u)
	}
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent claim error: %v", firstErr)
	}
	if admitted != cap {
		t.Fatalf("admitted=%d under concurrency, want exactly cap=%d — the cap leaked", admitted, cap)
	}
	// The ledger holds exactly cap rows for the period — no over-claim persisted.
	var rows int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM seat_claims WHERE period = $1`, period).Scan(&rows); err != nil {
		t.Fatalf("count seat_claims: %v", err)
	}
	if rows != cap {
		t.Fatalf("seat_claims has %d rows for the period, want exactly cap=%d", rows, cap)
	}
}

// TestAuthEventStore_Postgres_DeniedToAppRole proves auth_events is a system-only
// table: tf_app (the app pool, under claims) is REVOKEd all access and has no RLS
// policy, so a read is denied — exactly the public.user_identities posture. The
// store's methods only ever run on the admin pool, so this guards the schema's
// deny-by-default, not the store. TFAC-76.
func TestAuthEventStore_Postgres_DeniedToAppRole(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "alice")

	err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		var n int
		return tx.QueryRow(`SELECT count(*) FROM auth_events`).Scan(&n)
	})
	if err == nil {
		t.Fatal("tf_app could read auth_events — a system-only table must deny the app roles")
	}
}

// TestAuthEventStore_Postgres_RetentionLeavesAuthEvents pins the load-bearing
// SOC2 property: the session reaper deletes sessions but auth_events OUTLIVE them
// — the FK ON DELETE SET NULL nulls session_id while the audit row survives. The
// reaper touches only the sessions table; auth_events has no reaper. TFAC-76.
func TestAuthEventStore_Postgres_RetentionLeavesAuthEvents(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	sessID := "77777777-7777-7777-7777-777777777777"
	seedAuthExpiredSession(t, h, sessID, userID)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	if err := stores.AuthEvents.RecordSystem(ctx, domain.AuthEvent{
		OrgID: orgID, UserID: userID, SessionID: sessID, EventType: domain.AuthEventLoginSuccess,
	}); err != nil {
		t.Fatalf("RecordSystem: %v", err)
	}

	// Reap with a 30-day window — the seeded session (expired 60 days ago) goes.
	sessStore := sessions.NewStore(h.AdminDB, sessions.Key{})
	n, err := sessStore.ReapExpiredSystem(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ReapExpiredSystem: %v", err)
	}
	if n < 1 {
		t.Fatalf("reaper deleted %d sessions, want ≥1", n)
	}

	// The session is gone …
	var sessCount int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM public.sessions WHERE id = $1`, sessID).Scan(&sessCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessCount != 0 {
		t.Errorf("session survived the reap: %d rows", sessCount)
	}

	// … but the auth_events row OUTLIVES it, with session_id nulled.
	got, err := stores.AuthEvents.ListByOrgSystem(ctx, orgID, domain.AuthEventListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("auth_events count after reap = %d, want 1 (the durable record must survive)", len(got))
	}
	if got[0].SessionID != "" {
		t.Errorf("session_id = %q after reap, want '' (FK ON DELETE SET NULL)", got[0].SessionID)
	}
}
