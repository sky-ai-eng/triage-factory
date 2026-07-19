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

// fakeSeatAuthStore is a db.AuthEventStore stub whose SeatUsageSystem returns
// canned counts and whose RecordSystem captures written rows.
type fakeSeatAuthStore struct {
	distinct int
	active   bool
	usageErr error
	recorded []domain.AuthEvent
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
func (f *fakeSeatAuthStore) SeatUsageSystem(_ context.Context, _ time.Time, _ string) (int, bool, error) {
	return f.distinct, f.active, f.usageErr
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

	t.Run("uncapped is a no-op", func(t *testing.T) {
		setSeatCap(t, 0)
		fake := &fakeSeatAuthStore{distinct: 100, active: false}
		s := &Server{authEvents: fake}
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("uncapped deployment must never block")
		}
		if len(fake.recorded) != 0 {
			t.Fatalf("uncapped must record no event, got %d", len(fake.recorded))
		}
	})

	t.Run("under cap is admitted", func(t *testing.T) {
		setSeatCap(t, 2)
		fake := &fakeSeatAuthStore{distinct: 1, active: false}
		s := &Server{authEvents: fake}
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("a new user under the cap must be admitted")
		}
		if len(fake.recorded) != 0 {
			t.Fatalf("no rejection event expected, got %d", len(fake.recorded))
		}
	})

	t.Run("new distinct user at cap is blocked and recorded", func(t *testing.T) {
		setSeatCap(t, 2)
		fake := &fakeSeatAuthStore{distinct: 2, active: false}
		s := &Server{authEvents: fake}
		if !s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("the (maxSeats+1)th distinct user must be blocked")
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

	t.Run("returning user at cap is admitted", func(t *testing.T) {
		setSeatCap(t, 2)
		// Already active this period (holds a seat) even though the deployment is
		// well over the cap in raw count — must not be bounced.
		fake := &fakeSeatAuthStore{distinct: 9, active: true}
		s := &Server{authEvents: fake}
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("a returning, already-active user must never be blocked")
		}
		if len(fake.recorded) != 0 {
			t.Fatalf("no event expected for a returning user, got %d", len(fake.recorded))
		}
	})

	t.Run("fail-open on count error", func(t *testing.T) {
		setSeatCap(t, 2)
		fake := &fakeSeatAuthStore{distinct: 5, active: false, usageErr: errors.New("db down")}
		s := &Server{authEvents: fake}
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("a count error must fail OPEN (allow the login)")
		}
		if len(fake.recorded) != 0 {
			t.Fatalf("fail-open must record no rejection, got %d", len(fake.recorded))
		}
	})

	t.Run("no audit store wired is a no-op", func(t *testing.T) {
		setSeatCap(t, 2)
		s := &Server{} // authEvents nil
		if s.enforceSeatLimit(req, seatTestUser) {
			t.Fatal("with no audit store the gate cannot count and must allow")
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
