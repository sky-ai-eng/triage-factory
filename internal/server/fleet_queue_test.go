package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

type fakeFleetQueue struct {
	shares []db.OrgQueueShare
	err    error
}

func (f *fakeFleetQueue) FleetQueueShares(context.Context) ([]db.OrgQueueShare, error) {
	return f.shares, f.err
}

func capPtr(n int) *int { return &n }

func TestHandleFleetQueue_ReturnsOrgShare(t *testing.T) {
	fq := &fakeFleetQueue{shares: []db.OrgQueueShare{
		{OrgID: "org1", Active: 3, Queued: 7, MaxConcurrentRuns: capPtr(3)},
		{OrgID: "org2", Active: 1, Queued: 0},
	}}
	s := &Server{az: &authz.Checker{}, fleetQueue: fq}

	rec := httptest.NewRecorder()
	s.handleFleetQueue(rec, localModeReq("/api/fleet/queue?org=org1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto fleetQueueShareDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.OrgID != "org1" || dto.Active != 3 || dto.Queued != 7 {
		t.Fatalf("unexpected dto: %+v", dto)
	}
	// active (3) >= cap (3) → at_cap true.
	if dto.MaxConcurrentRuns == nil || *dto.MaxConcurrentRuns != 3 || !dto.AtCap {
		t.Fatalf("cap/at_cap wrong: %+v", dto)
	}
}

// A quiet org (no active/queued runs, so absent from the fleet-wide result)
// reports an all-zero share rather than 404 — a stable shape for pollers.
func TestHandleFleetQueue_QuietOrgReturnsZeroShare(t *testing.T) {
	fq := &fakeFleetQueue{shares: []db.OrgQueueShare{{OrgID: "other", Active: 2, Queued: 5}}}
	s := &Server{az: &authz.Checker{}, fleetQueue: fq}

	rec := httptest.NewRecorder()
	s.handleFleetQueue(rec, localModeReq("/api/fleet/queue?org=org1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var dto fleetQueueShareDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.OrgID != "org1" || dto.Active != 0 || dto.Queued != 0 || dto.AtCap {
		t.Fatalf("quiet org should be all-zero: %+v", dto)
	}
	if dto.MaxConcurrentRuns != nil {
		t.Fatalf("quiet org with no cap should omit max_concurrent_runs: %+v", dto)
	}
}

// The org-facing per-org cap read-out requires an explicit ?org=; the
// deployment-wide operator view is a separate EE surface (/api/fleet/backlog),
// so this endpoint deliberately does not serve a no-org "whole fleet" request.
func TestHandleFleetQueue_RequiresOrgParam(t *testing.T) {
	s := &Server{az: &authz.Checker{}, fleetQueue: &fakeFleetQueue{}}
	rec := httptest.NewRecorder()
	s.handleFleetQueue(rec, localModeReq("/api/fleet/queue"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing org should 400, got %d", rec.Code)
	}
}

func TestHandleFleetQueue_UnwiredReaderNotConfigured(t *testing.T) {
	s := &Server{az: &authz.Checker{}} // fleetQueue nil
	rec := httptest.NewRecorder()
	s.handleFleetQueue(rec, localModeReq("/api/fleet/queue?org=org1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("nil reader should 409 NOT_CONFIGURED, got %d", rec.Code)
	}
}
