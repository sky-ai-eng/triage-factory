package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// fakeSeatAuthStore is a db.AuthEventStore stub whose ClaimSeatSystem returns a
// canned decision (and captures its args), and whose RecordSystem captures rows.
type fakeSeatAuthStore struct {
	admitted bool
	seats    int
	claimErr error
	recorded []domain.AuthEvent

	claimCalls int
	gotPeriod  string
	gotUser    string
	gotCap     int
}

func (f *fakeSeatAuthStore) RecordSystem(_ context.Context, e domain.AuthEvent) error {
	f.recorded = append(f.recorded, e)
	return nil
}
func (f *fakeSeatAuthStore) ListByOrgSystem(context.Context, string, domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	return nil, nil
}
func (f *fakeSeatAuthStore) ListByUserSystem(context.Context, string, domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	return nil, nil
}
func (f *fakeSeatAuthStore) ClaimSeatSystem(_ context.Context, period, userID string, maxSeats int) (bool, int, error) {
	f.claimCalls++
	f.gotPeriod, f.gotUser, f.gotCap = period, userID, maxSeats
	return f.admitted, f.seats, f.claimErr
}

var _ db.AuthEventStore = (*fakeSeatAuthStore)(nil)

type seatStubDeployment struct{ ent entitlements.Entitlements }

func (s seatStubDeployment) Active() entitlements.Entitlements { return s.ent }

// setSeatCap registers a deployment entitlements snapshot with the given cap
// (cap <= 0 means uncapped — no LimitSeats), restoring the default on cleanup.
func setSeatCap(t *testing.T, cap int) {
	t.Helper()
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	var limits map[entitlements.Limit]int
	if cap > 0 {
		limits = map[entitlements.Limit]int{entitlements.LimitSeats: cap}
	}
	entitlements.RegisterDeploymentProvider(seatStubDeployment{ent: entitlements.New(nil, limits)})
}

const seatTestUser = "22222222-2222-2222-2222-222222222222"

func TestEnforceSeatLimit(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/auth/callback", nil)

	t.Run("uncapped is a no-op and never claims", func(t *testing.T) {
		setSeatCap(t, 0)
		fake := &fakeSeatAuthStore{admitted: false, seats: 100}
		s := &Server{authEvents: fake}
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("uncapped deployment must never block")
		}
		if fake.claimCalls != 0 {
			t.Fatalf("uncapped must not attempt a seat claim, got %d calls", fake.claimCalls)
		}
		if len(fake.recorded) != 0 {
			t.Fatalf("uncapped must record no event, got %d", len(fake.recorded))
		}
	})

	t.Run("admitted claim proceeds", func(t *testing.T) {
		setSeatCap(t, 2)
		fake := &fakeSeatAuthStore{admitted: true, seats: 2}
		s := &Server{authEvents: fake}
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("an admitted claim must not block")
		}
		if fake.gotUser != seatTestUser || fake.gotCap != 2 {
			t.Errorf("claim args = (user=%q cap=%d), want (%q, 2)", fake.gotUser, fake.gotCap, seatTestUser)
		}
		if len(fake.recorded) != 0 {
			t.Fatalf("no rejection event expected, got %d", len(fake.recorded))
		}
	})

	t.Run("rejected claim blocks and records", func(t *testing.T) {
		setSeatCap(t, 2)
		fake := &fakeSeatAuthStore{admitted: false, seats: 2}
		s := &Server{authEvents: fake}
		if !s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("a rejected claim (over cap) must block")
		}
		if len(fake.recorded) != 1 {
			t.Fatalf("expected one seat_limit_rejected event, got %d", len(fake.recorded))
		}
		e := fake.recorded[0]
		if e.EventType != domain.AuthEventSeatLimitRejected {
			t.Errorf("event type = %q, want %q", e.EventType, domain.AuthEventSeatLimitRejected)
		}
		if e.UserID != seatTestUser {
			t.Errorf("event user = %q, want %q", e.UserID, seatTestUser)
		}
		if e.OrgID != "" {
			t.Errorf("seat rejection is deployment-scoped; OrgID should be empty, got %q", e.OrgID)
		}
		// Detail carries the billable numbers for out-of-band true-up.
		for _, want := range []string{`"max_seats":2`, `"active_users":2`, `"period":"`} {
			if !strings.Contains(e.DetailJSON, want) {
				t.Errorf("detail %q missing %q", e.DetailJSON, want)
			}
		}
	})

	t.Run("fail-open on claim error", func(t *testing.T) {
		setSeatCap(t, 2)
		fake := &fakeSeatAuthStore{claimErr: errors.New("db down")}
		s := &Server{authEvents: fake}
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("a claim error must fail OPEN (allow the login)")
		}
		if len(fake.recorded) != 0 {
			t.Fatalf("fail-open must record no rejection, got %d", len(fake.recorded))
		}
	})

	t.Run("no seat store wired is a no-op", func(t *testing.T) {
		setSeatCap(t, 2)
		s := &Server{} // authEvents nil
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("with no seat store the gate cannot claim and must allow")
		}
	})
}

func TestBillingPeriodStart(t *testing.T) {
	got := billingPeriodStart(time.Date(2026, 7, 19, 13, 45, 30, 0, time.UTC))
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("billingPeriodStart = %v, want %v", got, want)
	}
	// A non-UTC instant late on the last day normalizes into the correct UTC month.
	loc := time.FixedZone("UTC-5", -5*3600)
	got = billingPeriodStart(time.Date(2026, 7, 31, 21, 0, 0, 0, loc)) // 2026-08-01 02:00 UTC
	want = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("cross-tz billingPeriodStart = %v, want %v", got, want)
	}
}
