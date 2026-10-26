package commitsigner

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// syncBuffer is a log sink safe to write from the watch's goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	t.Cleanup(logging.SetOutput(buf))
	return buf
}

// deploymentHealth is a control pod's health keeper for a fresh deployment
// signer, over the fixture's stores with an in-memory health table, reading a
// fake GitHub on which the account TriageFactory holds the signer's key. The
// gauge reports to the reader returned. Its clock is set by the caller.
func (f *fixture) deploymentHealth(t *testing.T) (*DeploymentHealth, *healthGitHub, *memDeploymentSigners, func() (int64, bool), *time.Time) {
	t.Helper()
	d := newDeploymentSigner(t)
	health := newMemDeploymentSigners()
	f.stores.DeploymentCommitSigners = health
	gh, url := newHealthGitHub(t, d.PublicKey)
	gh.set(func(g *healthGitHub) { g.id, g.login, g.name = deploymentAccount, "TriageFactory", "Triage Factory" })

	reader := sdkmetric.NewManualReader()
	h := NewDeploymentHealth(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), d, f.stores, healthResolver{url: url})
	h.client = ghclient.NewClient(url, "")
	clock := time.Now()
	h.now = func() time.Time { return clock }
	up := func() (int64, bool) {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "deployment_commit_signer.up" {
					continue
				}
				points := m.Data.(metricdata.Gauge[int64]).DataPoints
				if len(points) != 1 {
					t.Fatalf("deployment_commit_signer.up has %d series, want 1", len(points))
				}
				return points[0].Value, true
			}
		}
		return 0, false
	}
	return h, gh, health, up, &clock
}

func (m *memDeploymentSigners) only(t *testing.T) domain.DeploymentCommitSigner {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rows) != 1 {
		t.Fatalf("health table holds %d rows, want 1", len(m.rows))
	}
	for _, row := range m.rows {
		return row
	}
	panic("unreachable")
}

// The first tick on a fresh deployment registers its key, reads the account
// the configured login names and finds the key there, so the signer is
// active, the gauge reads 1, and later ticks inside the interval read GitHub
// no more. A key removed from the account breaks it at the next due check, the
// gauge falls to 0, and the pod logs a WARN; the key put back restores it.
func TestDeploymentHealth_PeriodicCheckActivatesBreaksAndRestores(t *testing.T) {
	f := newFixture(t)
	logs := captureLogs(t)
	h, gh, health, up, clock := f.deploymentHealth(t)
	ctx := context.Background()

	if _, ok := up(); ok {
		t.Error("the gauge reports before the pod has read the signer")
	}
	h.Tick(ctx)
	row := health.only(t)
	if row.Status != domain.CommitSignerActive || row.GitHubUserID != deploymentAccount || row.Name != "Triage Factory" {
		t.Fatalf("after the first tick the signer is %+v, want active on the account the login names", row)
	}
	if v, ok := up(); !ok || v != 1 {
		t.Errorf("gauge = %d (%v), want 1", v, ok)
	}

	// Renamed on GitHub: the check follows the account by its id now.
	gh.set(func(g *healthGitHub) { g.login, g.keys = "TriageFactoryBot", nil })
	h.Tick(ctx)
	if got := health.only(t); got.Status != domain.CommitSignerActive {
		t.Errorf("a tick inside the interval moved the signer to %s, want it left until a check is due", got.Status)
	}
	*clock = clock.Add(PeriodicCheckInterval + time.Minute)
	h.Tick(ctx)
	broken := health.only(t)
	if broken.Status != domain.CommitSignerBroken || broken.StatusReason != domain.CommitSignerReasonKeyMissing || broken.Login != "TriageFactoryBot" {
		t.Fatalf("with the key gone the signer is %+v, want broken (key_missing) under the renamed login", broken)
	}
	if v, ok := up(); !ok || v != 0 {
		t.Errorf("gauge = %d (%v), want 0", v, ok)
	}
	if !strings.Contains(logs.String(), "deployment commit signer is broken") {
		t.Errorf("logs = %s\nwant a WARN that the deployment commit signer is broken", logs)
	}

	gh.set(func(g *healthGitHub) { g.keys = []string{h.Signer.PublicKey} })
	*clock = clock.Add(PeriodicCheckInterval + time.Minute)
	h.Tick(ctx)
	if got := health.only(t); got.Status != domain.CommitSignerActive {
		t.Errorf("with the key back the signer is %s, want active", got.Status)
	}
}

// Every control pod reports the signer as it reads it, so a pod that did not
// make the check still logs the WARN and reports 0 once another pod has
// broken the signer.
func TestDeploymentHealth_EveryPodWarnsOnABrokenSigner(t *testing.T) {
	f := newFixture(t)
	logs := captureLogs(t)
	h, _, health, up, _ := f.deploymentHealth(t)
	ctx := context.Background()
	h.Tick(ctx)

	row := health.only(t)
	row.Status, row.StatusReason = domain.CommitSignerBroken, "unverified_email"
	health.set(row)
	h.Tick(ctx)
	if v, ok := up(); !ok || v != 0 {
		t.Errorf("gauge = %d (%v), want 0", v, ok)
	}
	if out := logs.String(); !strings.Contains(out, "deployment commit signer is broken") || !strings.Contains(out, "unverified_email") {
		t.Errorf("logs = %s\nwant a WARN naming the reason", out)
	}
}

// A login that names no account, or a bot, leaves the signer pending with no
// account, so no managed workspace commits as it.
func TestDeploymentHealth_LoginThatNamesNoUser(t *testing.T) {
	for name, stage := range map[string]func(g *healthGitHub){
		"no account": func(g *healthGitHub) { g.deleted = true },
		"a bot":      func(g *healthGitHub) { g.kind = "Bot" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			h, gh, health, up, _ := f.deploymentHealth(t)
			gh.set(stage)
			h.Tick(context.Background())
			row := health.only(t)
			if row.Status != domain.CommitSignerPending || row.GitHubUserID != 0 || row.CheckedAt == nil {
				t.Errorf("signer = %+v, want pending with no account, checked", row)
			}
			if v, ok := up(); !ok || v != 0 {
				t.Errorf("gauge = %d (%v), want 0", v, ok)
			}
		})
	}
}

// Verify activates a pending signer whose key is on the account, says which
// key to add when it is not, and, for a signer a pushed commit broke, holds it
// broken while GitHub still fails that commit and releases it once the fault
// is fixed.
func TestDeploymentHealth_Verify(t *testing.T) {
	f := newFixture(t)
	h, gh, health, _, _ := f.deploymentHealth(t)
	ctx := context.Background()

	gh.set(func(g *healthGitHub) { g.keys = nil })
	_, fault, err := h.Verify(ctx)
	if err != nil || fault == nil || fault.Kind != FaultKeyNotFound || !strings.Contains(fault.Message, "Signing key") {
		t.Fatalf("Verify with the key missing = %+v, %v; want a key-not-found fault saying how to add it", fault, err)
	}
	if got := health.only(t); got.Status != domain.CommitSignerPending {
		t.Errorf("after a failed Verify the signer is %s, want pending", got.Status)
	}

	gh.set(func(g *healthGitHub) { g.keys = []string{h.Signer.PublicKey} })
	row, fault, err := h.Verify(ctx)
	if err != nil || fault != nil || row.Status != domain.CommitSignerActive || row.GitHubUserID != deploymentAccount {
		t.Fatalf("Verify with the key on the account = %+v, %+v, %v; want active", row, fault, err)
	}

	// A commit the deployment signed, pushed and failed for unverified_email.
	email := h.Signer.CommitEmail(row)
	failed := f.pushedDeploymentSignature(t, h.Signer, shaA)
	health.failed = &failed
	broken := row
	broken.Status, broken.StatusReason = domain.CommitSignerBroken, "unverified_email"
	health.set(broken)
	gh.set(func(g *healthGitHub) {
		g.commits[shaA] = ghclient.CommitVerification{Reason: "unverified_email", CommitterEmail: email}
	})
	_, fault, err = h.Verify(ctx)
	if err != nil || fault == nil || fault.Kind != FaultCommitUnverified || !strings.Contains(fault.Message, email+" is not verified on TriageFactory") {
		t.Fatalf("Verify with the commit still failing = %+v, %v; want a commit-unverified fault naming the address", fault, err)
	}
	if got := health.only(t); got.Status != domain.CommitSignerBroken {
		t.Errorf("after the failed Verify the signer is %s, want broken", got.Status)
	}

	gh.set(func(g *healthGitHub) {
		g.commits[shaA] = ghclient.CommitVerification{Verified: true, Reason: domain.CommitVerificationValid, CommitterEmail: email}
	})
	if row, fault, err := h.Verify(ctx); err != nil || fault != nil || row.Status != domain.CommitSignerActive || row.StatusReason != "" {
		t.Errorf("Verify after the fix = %+v, %+v, %v; want active with no reason", row, fault, err)
	}
}

// pushedDeploymentSignature records a signature the deployment signer made for
// commit sha in the fixture's org and its push to acme/app.
func (f *fixture) pushedDeploymentSignature(t *testing.T, d *DeploymentSigner, sha string) domain.CommitSignature {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	row, err := f.stores.CommitSignatures.InsertSystem(ctx, domain.CommitSignature{
		OrgID: f.orgID, ConversationID: f.conversationID, ClaimID: f.claimID, BootEpoch: 1,
		Signer: domain.CommitSigningDeployment, SignerHost: d.Host, SignerUserID: deploymentAccount,
		PublicKey: d.PublicKey, Digest: "ab", State: domain.CommitSignatureSigned,
		Signature: "-----BEGIN SSH SIGNATURE-----", SignedAt: &at, CommitSHA: sha,
	})
	if err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}
	repo, err := f.stores.Repos.FindOrCreateByRefSystem(ctx, f.orgID, domain.RepoRefFromSlug("https://github.com", "acme/app"))
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	pushed, err := f.stores.CommitSignatures.MarkPushedSystem(ctx, f.orgID, row.ID, repo.ID)
	if err != nil || pushed == nil {
		t.Fatalf("MarkPushedSystem: %+v, %v", pushed, err)
	}
	return *pushed
}

// The org's poll cycle checks the commits its runs signed as the deployment
// signer, with the org's credential: one GitHub fails for the address the
// signer commits as breaks the deployment signer, and one committed under
// another address does not.
func TestHealth_DeploymentPushedCommitBreaksTheDeploymentSigner(t *testing.T) {
	for _, tc := range []struct {
		name       string
		committer  func(d *DeploymentSigner, row domain.DeploymentCommitSigner) string
		wantBroken bool
	}{
		{"committed as the signer", func(d *DeploymentSigner, row domain.DeploymentCommitSigner) string { return d.CommitEmail(row) }, true},
		{"committed under another address", func(*DeploymentSigner, domain.DeploymentCommitSigner) string { return "someone@example.com" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			checks := healthChecks(t)
			d, health := f.onDeploymentApp(t, domain.CommitSignerActive, "")
			h, gh, _ := f.healthChecker(t)
			h.Deployment = d
			ctx := context.Background()

			sig := f.pushedDeploymentSignature(t, d, shaA)
			row := health.only(t)
			gh.set(func(g *healthGitHub) {
				g.commits[shaA] = ghclient.CommitVerification{Reason: "unverified_email", CommitterEmail: tc.committer(d, row)}
			})
			h.Check(ctx, f.orgID)

			got := health.only(t)
			if broken := got.Status == domain.CommitSignerBroken && got.StatusReason == "unverified_email"; broken != tc.wantBroken {
				t.Errorf("deployment signer = %s (%q), want broken = %v", got.Status, got.StatusReason, tc.wantBroken)
			}
			if r := f.row(t, sig.ID); r.VerificationReason != "unverified_email" || r.CheckedAt == nil {
				t.Errorf("the pushed commit's row holds %q at %v, want GitHub's reason and a check time", r.VerificationReason, r.CheckedAt)
			}
			if got := checks()["post_push unverified_email"]; got != 1 {
				t.Errorf("post_push unverified_email checks = %d, want 1", got)
			}
		})
	}
}
