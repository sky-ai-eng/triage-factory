package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// testApp is the registration the probe runs against. Its contents don't
// matter to the cache — the GitHub half is stubbed — only that it is non-nil,
// which is what makes a probe worth running at all.
var testApp = &domain.OrgGitHubApp{
	OrgID: runmode.LocalDefaultOrgID, AppID: "123", Slug: "acme-bot", PEMRef: "pem", Active: true,
}

const testExpectedReceiver = "https://tf.example.org/api/webhooks/github/" + runmode.LocalDefaultOrgID

// TestRefreshWebhookHealth_FailurePreservesPriorState is the invariant the
// whole probe hangs on: a failed probe is not an answer. GitHub being
// unreachable says nothing about whether an org's webhooks are configured, so
// the last known state survives it untouched — only the retry schedule moves.
func TestRefreshWebhookHealth_FailurePreservesPriorState(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	s.SetDeployConfig("https://tf.example.org", [32]byte{})
	ctx := context.Background()

	s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
		return githubapp.WebhookHealth{
			State:                  githubapp.WebhookStateHealthy,
			HookHost:               "https://tf.example.org",
			SecretConfigured:       true,
			LastDeliveryAt:         time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
			LastDeliveryStatusCode: 200,
		}, nil
	}
	s.refreshWebhookHealth(ctx, runmode.LocalDefaultOrgID, testApp, testExpectedReceiver)

	dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, testApp)
	if dto == nil || dto.State != string(githubapp.WebhookStateHealthy) {
		t.Fatalf("after a successful probe: webhook_health=%+v, want healthy", dto)
	}
	checkedAt := dto.CheckedAt

	// Now GitHub falls over.
	s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
		return githubapp.WebhookHealth{}, errors.New("github unreachable")
	}
	s.refreshWebhookHealth(ctx, runmode.LocalDefaultOrgID, testApp, testExpectedReceiver)

	after := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, testApp)
	if after == nil {
		t.Fatal("webhook_health=null after a failed probe, want the prior answer")
	}
	if after.State != string(githubapp.WebhookStateHealthy) {
		t.Errorf("state=%q after a failed probe, want the prior %q", after.State, githubapp.WebhookStateHealthy)
	}
	if after.LastDeliveryStatusCode != 200 || after.LastDeliveryAt == "" {
		t.Errorf("delivery facts=%+v after a failed probe, want the prior ones intact", after)
	}
	// checked_at is when the last real answer was obtained; a failure must not
	// advance it, or a stale state would read as freshly confirmed.
	if after.CheckedAt != checkedAt {
		t.Errorf("checked_at=%q after a failed probe, want the prior %q", after.CheckedAt, checkedAt)
	}
}

// TestWebhookHealthDTO_AppSwapDropsTheCachedAnswer pins that a cached answer
// belongs to the registration it was probed for, not to the org.
//
// An org can tear its App down and register another — switch to a PAT and back,
// or discard a staged App and import a different one — while its id never
// changes. Serving the old App's state for the rest of the TTL would report a
// health, a last-delivery time, and a repair button for something the workspace
// no longer has.
func TestWebhookHealthDTO_AppSwapDropsTheCachedAnswer(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	s.SetDeployConfig("https://tf.example.org", [32]byte{})
	ctx := context.Background()

	appA := &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "111", PEMRef: "pem-a"}
	appB := &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "222", PEMRef: "pem-b"}

	var probedFor []string
	s.hookProbe = func(_ context.Context, _ string, app *domain.OrgGitHubApp, _ string) (githubapp.WebhookHealth, error) {
		probedFor = append(probedFor, app.AppID)
		return githubapp.WebhookHealth{
			State: githubapp.WebhookStateHealthy, HookHost: "https://tf.example.org",
			SecretConfigured: true, LastDeliveryStatusCode: 200,
			LastDeliveryAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		}, nil
	}
	s.refreshWebhookHealth(ctx, runmode.LocalDefaultOrgID, appA, testExpectedReceiver)
	if dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, appA); dto == nil {
		t.Fatal("App A: webhook_health=null after a successful probe")
	}

	// The org swaps to a different App. Nothing has probed it yet, so there is
	// no answer to give — and A's answer is not it.
	if dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, appB); dto != nil {
		t.Errorf("App B: webhook_health=%+v before B was ever probed, want null", dto)
	}
	waitForWebhookProbeIdle(t, s, runmode.LocalDefaultOrgID)
	if len(probedFor) != 2 || probedFor[1] != "222" {
		t.Fatalf("probed for %v, want a probe of the new App B", probedFor)
	}
	if dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, appB); dto == nil {
		t.Fatal("App B: webhook_health=null after its own probe landed")
	}

	// Re-importing the SAME App id is a new registration too — the stored
	// webhook secret travels with the row, so the answer about the old one
	// (rejecting, say, for the secret that was missing) must not carry over.
	reimported := &domain.OrgGitHubApp{
		OrgID: runmode.LocalDefaultOrgID, AppID: "222", PEMRef: "pem-b",
		RegisteredAt: time.Date(2026, 8, 15, 13, 0, 0, 0, time.UTC),
	}
	if dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, reimported); dto != nil {
		t.Errorf("re-imported App: webhook_health=%+v, want null until it is probed itself", dto)
	}
	waitForWebhookProbeIdle(t, s, runmode.LocalDefaultOrgID)
}

// TestRefreshWebhookHealth_LateProbeForAReplacedAppIsDropped closes the half an
// invalidate-on-teardown call could not: a probe that was already in flight when
// the registration changed. Writing its answer would stamp the OLD App's state
// as freshly checked, and the entry would then look current for a full TTL.
func TestRefreshWebhookHealth_LateProbeForAReplacedAppIsDropped(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	s.SetDeployConfig("https://tf.example.org", [32]byte{})
	ctx := context.Background()

	appA := &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "111", PEMRef: "pem-a"}
	appB := &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "222", PEMRef: "pem-b"}

	// B is the org's current App, probed and cached.
	s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
		return githubapp.WebhookHealth{State: githubapp.WebhookStateNoDeliveries}, nil
	}
	s.refreshWebhookHealth(ctx, runmode.LocalDefaultOrgID, appB, testExpectedReceiver)

	// A's probe — started before the swap — finally answers.
	s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
		return githubapp.WebhookHealth{State: githubapp.WebhookStateRejected, LastDeliveryStatusCode: 401}, nil
	}
	s.refreshWebhookHealth(ctx, runmode.LocalDefaultOrgID, appA, testExpectedReceiver)

	dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, appB)
	if dto == nil {
		t.Fatal("webhook_health=null, want App B's own answer")
	}
	if dto.State != string(githubapp.WebhookStateNoDeliveries) {
		t.Errorf("state=%q, want %q — the replaced App's late probe overwrote it",
			dto.State, githubapp.WebhookStateNoDeliveries)
	}
	waitForWebhookProbeIdle(t, s, runmode.LocalDefaultOrgID)
}

// TestWebhookHealthDTO_NothingToProbe pins the two cases that produce no probe
// at all: an org with no App (nothing to ask about) and a deployment with no
// public identity (no receiver URL to compare a hook against, so any answer
// would be invented).
func TestWebhookHealthDTO_NothingToProbe(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	ctx := context.Background()

	t.Run("no app", func(t *testing.T) {
		s := newTestServer(t)
		s.SetDeployConfig("https://tf.example.org", [32]byte{})
		probed := false
		s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
			probed = true
			return githubapp.WebhookHealth{}, nil
		}
		if dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, nil); dto != nil {
			t.Errorf("webhook_health=%+v with no App, want null", dto)
		}
		if probed {
			t.Error("probed an org with no registered App")
		}
	})

	t.Run("no deployment identity", func(t *testing.T) {
		s := newTestServer(t) // newTestServer leaves deployCfg nil
		probed := false
		s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
			probed = true
			return githubapp.WebhookHealth{}, nil
		}
		if dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, testApp); dto != nil {
			t.Errorf("webhook_health=%+v with no deployment identity, want null", dto)
		}
		if probed {
			t.Error("probed without a receiver URL to compare against")
		}
	})
}

// TestWebhookHealthDTO_LocalPlaceholderIsAState pins that a local-mode
// deployment — whose App carries the inert placeholder hook URL TF wrote
// itself — reports "not configured" as an ordinary state. Nothing about it is
// an error, and the read that carries it is a plain 200.
func TestWebhookHealthDTO_LocalPlaceholderIsAState(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	s.SetDeployConfig("http://127.0.0.1:3000", [32]byte{})
	ctx := context.Background()

	s.hookProbe = func(_ context.Context, _ string, _ *domain.OrgGitHubApp, expectedURL string) (githubapp.WebhookHealth, error) {
		if expectedURL != "http://127.0.0.1:3000/api/webhooks/github/"+runmode.LocalDefaultOrgID {
			t.Errorf("expectedURL=%q, want this deployment's receiver route", expectedURL)
		}
		// What the real probe returns for an App wearing PlaceholderHookURL.
		return githubapp.WebhookHealth{State: githubapp.WebhookStateNotConfigured}, nil
	}
	s.refreshWebhookHealth(ctx, runmode.LocalDefaultOrgID, testApp, s.webhookReceiverURL(runmode.LocalDefaultOrgID))

	dto := s.webhookHealthDTO(ctx, runmode.LocalDefaultOrgID, testApp)
	if dto == nil {
		t.Fatal("webhook_health=null, want the not-configured state")
	}
	if dto.State != string(githubapp.WebhookStateNotConfigured) {
		t.Errorf("state=%q, want %q", dto.State, githubapp.WebhookStateNotConfigured)
	}
	if dto.HookHost != "" || dto.LastDeliveryAt != "" {
		t.Errorf("not-configured carries hook_host=%q last_delivery_at=%q, want both empty",
			dto.HookHost, dto.LastDeliveryAt)
	}
}

// TestGitHubAppStatus_ProbeFailureDoesNotFailTheRead pins the second half of
// the off-the-critical-path rule at the handler: with GitHub failing every
// probe, the status endpoint still answers 200 and still serves the last known
// state. The probe kicked here runs in the background and can only preserve.
func TestGitHubAppStatus_ProbeFailureDoesNotFailTheRead(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	s.SetDeployConfig("https://tf.example.org", [32]byte{})
	seedLocalGitHubApp(t, s, "Iv1.probe", true)

	// Seed the prior answer against the registration the handler will actually
	// load, not a stand-in: a cached answer is scoped to its App, so one probed
	// for a different row is (correctly) not this org's answer at all.
	registered, err := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil || registered == nil {
		t.Fatalf("read seeded app: %+v, %v", registered, err)
	}
	s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
		return githubapp.WebhookHealth{
			State: githubapp.WebhookStateNoDeliveries, HookHost: "https://tf.example.org", SecretConfigured: true,
		}, nil
	}
	s.refreshWebhookHealth(context.Background(), runmode.LocalDefaultOrgID, registered, testExpectedReceiver)

	// Every subsequent probe fails, and the entry is due, so the read kicks one.
	probes := make(chan struct{}, 4)
	s.hookProbe = func(context.Context, string, *domain.OrgGitHubApp, string) (githubapp.WebhookHealth, error) {
		probes <- struct{}{}
		return githubapp.WebhookHealth{}, errors.New("github unreachable")
	}
	s.expireWebhookHealthForTest(runmode.LocalDefaultOrgID)

	rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 — a failed probe must not fail the read", rec.Code, rec.Body.String())
	}
	var out githubAppStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.WebhookHealth == nil || out.WebhookHealth.State != string(githubapp.WebhookStateNoDeliveries) {
		t.Fatalf("webhook_health=%+v, want the last known state preserved", out.WebhookHealth)
	}

	// Let the background probe land before the test's server goes away, and
	// confirm the failure still didn't disturb the stored answer.
	select {
	case <-probes:
	case <-time.After(5 * time.Second):
		t.Fatal("the due status read never kicked a probe")
	}
	waitForWebhookProbeIdle(t, s, runmode.LocalDefaultOrgID)
	after := s.webhookHealthDTO(context.Background(), runmode.LocalDefaultOrgID, registered)
	if after == nil || after.State != string(githubapp.WebhookStateNoDeliveries) {
		t.Errorf("webhook_health=%+v after the failed background probe, want it unchanged", after)
	}
}

// TestReplayCandidates pins what the repair action will and won't send again:
// installation deliveries only (the mirror is what a rejecting receiver
// corrupts), newest first, bounded.
func TestReplayCandidates(t *testing.T) {
	deliveries := []githubapp.HookDelivery{
		{ID: 5, Event: "installation", Action: "created", StatusCode: 401},
		{ID: 4, Event: "pull_request", Action: "opened", StatusCode: 401},
		{ID: 3, Event: "installation", Action: "deleted", StatusCode: 0},
		{ID: 2, Event: "installation_repositories", Action: "added", StatusCode: 401},
		{ID: 1, Event: "installation", Action: "suspend", StatusCode: 404},
		// An accepted delivery, which a host that ignores the query's status
		// filter (the GHES descriptions don't carry it) would hand back too.
		// Replaying it would be harmless and counting it would be a lie.
		{ID: 0, Event: "installation", Action: "created", StatusCode: 204},
	}

	attempt, candidates := replayCandidates(deliveries, 50)
	if candidates != 3 {
		t.Errorf("candidates=%d, want 3 (the installation deliveries)", candidates)
	}
	if len(attempt) != 3 {
		t.Fatalf("attempt=%d deliveries, want 3", len(attempt))
	}
	for _, d := range attempt {
		if d.Event != "installation" {
			t.Errorf("selected a %q delivery; only installation events heal the mirror", d.Event)
		}
	}

	// The cap keeps the newest, and still reports the true total so a "replayed
	// 2 of 3" is legible rather than looking like nothing was missed.
	capped, total := replayCandidates(deliveries, 2)
	if total != 3 {
		t.Errorf("candidates under a cap=%d, want the full 3", total)
	}
	if len(capped) != 2 || capped[0].ID != 5 || capped[1].ID != 3 {
		t.Errorf("capped=%+v, want the two newest installation deliveries", capped)
	}
}

// TestGitHubAppWebhookReplay_Gates pins the endpoint's refusals: an org that
// isn't on a BYO App has nothing to replay and is 404'd before any GitHub call,
// and an org whose App key can't be read fails as a gateway error rather than
// as a 404 (which would read as "no App registered").
func TestGitHubAppWebhookReplay_Gates(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	path := "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/app/webhook/replay"

	t.Run("no app", func(t *testing.T) {
		s := newTestServer(t)
		if rec := doJSON(t, s, "POST", path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
		}
	})

	t.Run("app registered, key unreadable", func(t *testing.T) {
		s := newTestServer(t)
		// seedLocalGitHubApp points pem_ref at a secret nobody stored, so the
		// minter build fails before any request leaves the process.
		seedLocalGitHubApp(t, s, "Iv1.replay", true)
		rec := doJSON(t, s, "POST", path, nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
		}
		var out struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out.Errors) == 0 || out.Errors[0].Message == "" {
			t.Error("502 with no error message; the panel renders this string")
		}
	})
}

// expireWebhookHealthForTest makes the org's next read treat its cached answer
// as due, without waiting out the TTL.
func (s *Server) expireWebhookHealthForTest(orgID string) {
	s.invalidateWebhookHealth(orgID)
}

// waitForWebhookProbeIdle blocks until no probe is in flight for orgID, so a
// test can assert on the cache the background refresh writes to.
func waitForWebhookProbeIdle(t *testing.T, s *Server, orgID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.webhookHealthMu.Lock()
		e, ok := s.webhookHealth[orgID]
		idle := !ok || !e.probing
		s.webhookHealthMu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a webhook probe was still in flight after 5s")
}
