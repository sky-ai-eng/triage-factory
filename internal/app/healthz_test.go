package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
)

// TestExecutorHealthy pins the 200/503 decision table, including the
// fenced-is-informational override (#624).
func TestExecutorHealthy(t *testing.T) {
	cases := []struct {
		name                                              string
		dispatcherAlive, heartbeatFresh, brokerOK, fenced bool
		want                                              bool
	}{
		{"all green", true, true, true, false, true},
		{"dispatcher dead", false, true, true, false, false},
		{"heartbeat stale", true, false, true, false, false},
		{"broker down", true, true, false, false, false},
		{"fenced overrides stale heartbeat", true, false, true, true, true},
		{"fenced overrides dead broker", true, true, false, true, true},
		{"fenced overrides dead dispatcher", false, false, false, true, true},
	}
	for _, tc := range cases {
		if got := executorHealthy(tc.dispatcherAlive, tc.heartbeatFresh, tc.brokerOK, tc.fenced); got != tc.want {
			t.Errorf("%s: executorHealthy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestHandleExecutorHealthz_BrokerDown503 exercises the full handler: a
// spawner with no dispatcher/heartbeat activity and a broker Ping that
// fails must 503, and the body must carry broker_ok=false. This also pins
// that a nil-brokerPing host reports broker_ok=true (nothing to verify).
func TestHandleExecutorHealthz_BrokerDown503(t *testing.T) {
	a := &App{spawner: delegate.NewSpawner(nil, db.Stores{}, nil, nil, "")}

	// Broker that fails its Ping → broker_ok=false → 503 (dispatcher also
	// isn't alive in this bare spawner, so it's 503 regardless; the point is
	// the field plumbs through).
	a.brokerPing = func(context.Context) error { return context.DeadlineExceeded }

	rec := httptest.NewRecorder()
	a.handleExecutorHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	var body executorHealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.BrokerOK {
		t.Error("broker_ok = true, want false when the broker Ping fails")
	}
	if body.DispatcherAlive {
		t.Error("dispatcher_alive = true on a never-started dispatcher")
	}

	// nil brokerPing → nothing to verify → broker_ok=true.
	a.brokerPing = nil
	if !a.checkBrokerOK(context.Background()) {
		t.Error("checkBrokerOK with a nil pinger should report ok (no broker to verify)")
	}
}
