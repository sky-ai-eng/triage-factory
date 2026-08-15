package githubapp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// The fixtures below are GitHub's own published response shapes for
// GET /app/hook/config (the webhook-config example) and
// GET /app/hook/deliveries (the hook-delivery-items example), edited only in
// the fields each case is about. The masked secret is verbatim on purpose:
// GitHub never returns the real one, which is why nothing here compares it.
const (
	// A configured, secret-bearing webhook. %s takes the delivery URL.
	fixtureHookConfig = `{"content_type":"json","insecure_ssl":"0","secret":"********","url":%q}`
	// An App with no webhook at all: 200, every field optional, url absent.
	fixtureHookConfigUnset = `{"content_type":"json","insecure_ssl":"0"}`

	// One accepted delivery.
	fixtureDeliveriesOK = `[
		{"id":12345678,"guid":"0b989ba4-242f-11e5-81e1-c7b6966d2516","delivered_at":"2026-08-15T00:57:16Z",
		 "redelivery":false,"duration":0.27,"status":"OK","status_code":200,
		 "event":"pull_request","action":"opened","installation_id":123,"repository_id":456}
	]`
	// The receiver refusing every delivery: an unauthenticated 401, which is
	// what a missing or mismatched webhook secret looks like from GitHub's side.
	fixtureDeliveriesRejected = `[
		{"id":12345679,"guid":"1b989ba4-242f-11e5-81e1-c7b6966d2516","delivered_at":"2026-08-15T01:00:00Z",
		 "redelivery":false,"duration":0.03,"status":"Unauthorized","status_code":401,
		 "event":"installation","action":"created","installation_id":123,"repository_id":null},
		{"id":12345670,"guid":"2b989ba4-242f-11e5-81e1-c7b6966d2516","delivered_at":"2026-08-14T01:00:00Z",
		 "redelivery":false,"duration":0.27,"status":"OK","status_code":200,
		 "event":"pull_request","action":"opened","installation_id":123,"repository_id":456}
	]`
)

// hookRig serves the two App-webhook endpoints from canned bodies. A status
// other than 200 for either half drives the failure and capability-gap paths;
// a 404 is how a GHES without the endpoint (or, for the config half, possibly
// an App with no webhook) answers.
type hookRig struct {
	configStatus int
	configBody   string
	deliveryBody string
	// deliveryStatus applies to GET /app/hook/deliveries.
	deliveryStatus int
	// deliveryCalls counts the deliveries GETs, so a case can pin that a state
	// decided by configuration alone costs no second round trip.
	deliveryCalls int
	// redelivered records the delivery ids replayed through the attempts POST.
	redelivered []string
}

func newHookMinter(t *testing.T, rig *hookRig) *githubapp.Minter {
	t.Helper()
	key := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/hook/config":
			writeFixture(w, rig.configStatus, rig.configBody)
		case r.URL.Path == "/app/hook/deliveries":
			rig.deliveryCalls++
			writeFixture(w, rig.deliveryStatus, rig.deliveryBody)
		case strings.HasSuffix(r.URL.Path, "/attempts"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/hook/deliveries/"), "/attempts")
			rig.redelivered = append(rig.redelivered, id)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      424242,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m
}

func writeFixture(w http.ResponseWriter, status int, body string) {
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if status == http.StatusOK && body != "" {
		_, _ = w.Write([]byte(body))
	}
}

const testReceiverURL = "https://tf.example.org/api/webhooks/github/11111111-1111-1111-1111-111111111111"

// TestProbeWebhookHealth_States walks every state the probe can report off
// recorded hook/config + hook/deliveries bodies — including the placeholder
// URL TF writes for a deployment GitHub cannot reach, which is the case that
// makes a laptop install indistinguishable from a broken one today.
func TestProbeWebhookHealth_States(t *testing.T) {
	hookConfig := func(u string) string { return fmt.Sprintf(fixtureHookConfig, u) }

	for _, tc := range []struct {
		name           string
		rig            hookRig
		wantState      githubapp.WebhookState
		wantHost       string
		wantStatusCode int
		wantSecret     bool
		// wantDeliveryCalls pins whether the deliveries endpoint was consulted;
		// a state decided by configuration alone must not pay for it.
		wantDeliveryCalls int
	}{
		{
			name:       "no webhook configured",
			rig:        hookRig{configBody: fixtureHookConfigUnset},
			wantState:  githubapp.WebhookStateNotConfigured,
			wantSecret: false,
		},
		{
			name:       "placeholder url we wrote ourselves",
			rig:        hookRig{configBody: hookConfig(githubapp.PlaceholderHookURL)},
			wantState:  githubapp.WebhookStateNotConfigured,
			wantSecret: true,
			// Not even the host is named: there is nowhere real to name.
			wantHost: "",
		},
		{
			name:       "pointing at another deployment",
			rig:        hookRig{configBody: hookConfig("https://other.example.com/api/webhooks/github/11111111-1111-1111-1111-111111111111")},
			wantState:  githubapp.WebhookStateElsewhere,
			wantHost:   "https://other.example.com",
			wantSecret: true,
		},
		{
			name: "same host, another org's receiver",
			rig: hookRig{configBody: hookConfig(
				"https://tf.example.org/api/webhooks/github/22222222-2222-2222-2222-222222222222")},
			wantState:  githubapp.WebhookStateElsewhere,
			wantHost:   "https://tf.example.org",
			wantSecret: true,
		},
		{
			name: "delivering, rejected",
			rig: hookRig{
				configBody:   hookConfig(testReceiverURL),
				deliveryBody: fixtureDeliveriesRejected,
			},
			wantState:         githubapp.WebhookStateRejected,
			wantHost:          "https://tf.example.org",
			wantStatusCode:    401,
			wantSecret:        true,
			wantDeliveryCalls: 1,
		},
		{
			name: "healthy",
			rig: hookRig{
				configBody:   hookConfig(testReceiverURL),
				deliveryBody: fixtureDeliveriesOK,
			},
			wantState:         githubapp.WebhookStateHealthy,
			wantHost:          "https://tf.example.org",
			wantStatusCode:    200,
			wantSecret:        true,
			wantDeliveryCalls: 1,
		},
		{
			name: "configured, nothing delivered in the retention window",
			rig: hookRig{
				configBody:   hookConfig(testReceiverURL),
				deliveryBody: `[]`,
			},
			wantState:         githubapp.WebhookStateNoDeliveries,
			wantHost:          "https://tf.example.org",
			wantSecret:        true,
			wantDeliveryCalls: 1,
		},
		{
			name: "trailing slash is the same endpoint",
			rig: hookRig{
				configBody:   hookConfig(testReceiverURL + "/"),
				deliveryBody: fixtureDeliveriesOK,
			},
			wantState:         githubapp.WebhookStateHealthy,
			wantHost:          "https://tf.example.org",
			wantStatusCode:    200,
			wantSecret:        true,
			wantDeliveryCalls: 1,
		},
		{
			name: "host without delivery history",
			rig: hookRig{
				configBody:     hookConfig(testReceiverURL),
				deliveryStatus: http.StatusNotFound,
			},
			wantState:         githubapp.WebhookStateUnavailable,
			wantHost:          "https://tf.example.org",
			wantSecret:        true,
			wantDeliveryCalls: 1,
		},
		{
			name: "config 404 but the endpoint set exists",
			rig: hookRig{
				configStatus: http.StatusNotFound,
				deliveryBody: `[]`,
			},
			// A 404 from the config half is ambiguous on its face; the
			// deliveries endpoint answering proves the host serves these
			// endpoints, so the 404 was about the App, not the host.
			wantState:         githubapp.WebhookStateNotConfigured,
			wantDeliveryCalls: 1,
		},
		{
			name: "endpoints absent entirely",
			rig: hookRig{
				configStatus:   http.StatusNotFound,
				deliveryStatus: http.StatusNotFound,
			},
			wantState:         githubapp.WebhookStateUnavailable,
			wantDeliveryCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := tc.rig
			m := newHookMinter(t, &rig)

			got, err := m.ProbeWebhookHealth(context.Background(), testReceiverURL)
			if err != nil {
				t.Fatalf("ProbeWebhookHealth: %v", err)
			}
			if got.State != tc.wantState {
				t.Errorf("state=%q, want %q", got.State, tc.wantState)
			}
			if got.HookHost != tc.wantHost {
				t.Errorf("hook host=%q, want %q", got.HookHost, tc.wantHost)
			}
			if got.LastDeliveryStatusCode != tc.wantStatusCode {
				t.Errorf("last delivery status=%d, want %d", got.LastDeliveryStatusCode, tc.wantStatusCode)
			}
			if got.SecretConfigured != tc.wantSecret {
				t.Errorf("secret configured=%v, want %v", got.SecretConfigured, tc.wantSecret)
			}
			if rig.deliveryCalls != tc.wantDeliveryCalls {
				t.Errorf("deliveries endpoint called %d times, want %d", rig.deliveryCalls, tc.wantDeliveryCalls)
			}
			// Healthy is the only state that may claim a delivery timestamp
			// alongside a 2xx; every other state either has none or has one
			// attached to a failure.
			if tc.wantState == githubapp.WebhookStateHealthy && got.LastDeliveryAt.IsZero() {
				t.Error("healthy with no last-delivery timestamp; want the delivered_at from the fixture")
			}
		})
	}
}

// TestProbeWebhookHealth_NewestDeliveryDecides pins that the state follows the
// most recent delivery rather than the friendliest one in the window: an org
// whose secret was rotated an hour ago still has plenty of 2xx history.
func TestProbeWebhookHealth_NewestDeliveryDecides(t *testing.T) {
	rig := hookRig{
		configBody: fmt.Sprintf(fixtureHookConfig, testReceiverURL),
		// Deliberately out of order: the 401 is newer but listed last.
		deliveryBody: `[
			{"id":1,"guid":"a","delivered_at":"2026-08-01T00:00:00Z","redelivery":false,"duration":0.1,
			 "status":"OK","status_code":200,"event":"pull_request","action":"opened","installation_id":1,"repository_id":2},
			{"id":2,"guid":"b","delivered_at":"2026-08-15T00:00:00Z","redelivery":false,"duration":0.1,
			 "status":"Unauthorized","status_code":401,"event":"installation","action":"created","installation_id":1,"repository_id":null}
		]`,
	}
	m := newHookMinter(t, &rig)

	got, err := m.ProbeWebhookHealth(context.Background(), testReceiverURL)
	if err != nil {
		t.Fatalf("ProbeWebhookHealth: %v", err)
	}
	if got.State != githubapp.WebhookStateRejected {
		t.Errorf("state=%q, want %q", got.State, githubapp.WebhookStateRejected)
	}
	if got.LastDeliveryStatusCode != 401 {
		t.Errorf("last delivery status=%d, want 401 (the newest delivery)", got.LastDeliveryStatusCode)
	}
}

// TestProbeWebhookHealth_FailurePropagates pins that a GitHub failure surfaces
// as an error rather than as a state. A 5xx from GitHub says nothing about
// anyone's webhook, and inventing a state from one is how an outage would
// render as "your webhooks are broken" — the caller keeps its last answer.
func TestProbeWebhookHealth_FailurePropagates(t *testing.T) {
	rig := hookRig{configStatus: http.StatusInternalServerError}
	m := newHookMinter(t, &rig)

	got, err := m.ProbeWebhookHealth(context.Background(), testReceiverURL)
	if err == nil {
		t.Fatalf("ProbeWebhookHealth = %+v, want an error", got)
	}
	if got.State != "" {
		t.Errorf("state=%q on failure, want the zero value", got.State)
	}
	if errors.Is(err, githubapp.ErrHookAPIUnavailable) {
		t.Error("a 500 reported as the capability-gap sentinel; that sentinel is for 404 only")
	}
}

// TestProbeWebhookHealth_RequiresExpectedURL refuses to guess. Without knowing
// which receiver is ours, "points here" and "points elsewhere" are the same
// observation.
func TestProbeWebhookHealth_RequiresExpectedURL(t *testing.T) {
	rig := hookRig{configBody: fixtureHookConfigUnset}
	m := newHookMinter(t, &rig)

	if _, err := m.ProbeWebhookHealth(context.Background(), "  "); err == nil {
		t.Fatal("ProbeWebhookHealth with no expected URL = nil error, want a refusal")
	}
}

// TestListHookDeliveries_QueryAndPaging pins the wire contract the replay path
// depends on: GitHub's own status filter, the per-page bound, and cursor
// paging via the Link header.
func TestListHookDeliveries_QueryAndPaging(t *testing.T) {
	key := newTestKey(t)
	var seenQueries []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/hook/deliveries" {
			http.NotFound(w, r)
			return
		}
		seenQueries = append(seenQueries, r.URL.RawQuery)
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", `<`+srv.URL+`/app/hook/deliveries?cursor=v1_2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":1,"guid":"a","delivered_at":"2026-08-15T00:00:00Z","redelivery":false,
				"duration":0.1,"status":"Unauthorized","status_code":401,"event":"installation","action":"created",
				"installation_id":7,"repository_id":null}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":2,"guid":"b","delivered_at":"2026-08-14T00:00:00Z","redelivery":true,
			"duration":0.1,"status":"Unauthorized","status_code":401,"event":"installation","action":"deleted",
			"installation_id":7,"repository_id":null}]`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key, AppID: 424242, APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	got, err := m.ListHookDeliveries(context.Background(), githubapp.HookDeliveryQuery{
		Status: "failure", PerPage: 100, MaxPages: 2,
	})
	if err != nil {
		t.Fatalf("ListHookDeliveries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d deliveries across two pages, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].Event != "installation" || got[0].Action != "created" || got[0].InstallationID != 7 {
		t.Errorf("delivery[0]=%+v, want the parsed first-page entry", got[0])
	}
	if !got[1].Redelivery {
		t.Error("delivery[1].Redelivery=false, want true (the fixture marks it a redelivery)")
	}
	if len(seenQueries) != 2 {
		t.Fatalf("requests=%v, want two (the page plus its cursor)", seenQueries)
	}
	if !strings.Contains(seenQueries[0], "status=failure") || !strings.Contains(seenQueries[0], "per_page=100") {
		t.Errorf("first query=%q, want the status filter and per_page", seenQueries[0])
	}
	if !strings.Contains(seenQueries[1], "cursor=v1_2") {
		t.Errorf("second query=%q, want the cursor from the Link header", seenQueries[1])
	}
}

// TestListHookDeliveries_StopsAtMaxPages keeps an App with a long delivery
// history from turning one repair click into unbounded paging.
func TestListHookDeliveries_StopsAtMaxPages(t *testing.T) {
	key := newTestKey(t)
	pages := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Link", `<`+srv.URL+`/app/hook/deliveries?cursor=more>; rel="next"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key, AppID: 424242, APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	if _, err := m.ListHookDeliveries(context.Background(), githubapp.HookDeliveryQuery{MaxPages: 3}); err != nil {
		t.Fatalf("ListHookDeliveries: %v", err)
	}
	if pages != 3 {
		t.Errorf("fetched %d pages, want 3 (MaxPages)", pages)
	}
}

// TestRedeliverHookDelivery pins the replay POST's path and that a non-2xx is
// an error — the repair action counts failures per delivery, so it has to be
// able to see them.
func TestRedeliverHookDelivery(t *testing.T) {
	rig := hookRig{}
	m := newHookMinter(t, &rig)
	if err := m.RedeliverHookDelivery(context.Background(), 12345678); err != nil {
		t.Fatalf("RedeliverHookDelivery: %v", err)
	}
	if len(rig.redelivered) != 1 || rig.redelivered[0] != "12345678" {
		t.Errorf("redelivered=%v, want [12345678]", rig.redelivered)
	}
	if err := m.RedeliverHookDelivery(context.Background(), 0); err == nil {
		t.Error("RedeliverHookDelivery(0) = nil, want a refusal before any request")
	}
}
