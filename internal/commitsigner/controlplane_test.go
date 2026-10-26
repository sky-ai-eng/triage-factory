package commitsigner

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const signerAccount int64 = 583231

// memSecrets is the secret store's system read over a map; nothing else is
// called.
type memSecrets struct {
	db.SecretStore
	mu     sync.Mutex
	values map[string]string
	// onGet, when set, runs before each read, and its error is the read's.
	onGet func() error
}

func (m *memSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.onGet != nil {
		if err := m.onGet(); err != nil {
			return "", err
		}
	}
	return m.values[key], nil
}

func (m *memSecrets) set(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
}

// fixture is one local database with an org whose signer is active, its
// private key in the secret store, and one live engagement at boot epoch 1.
type fixture struct {
	conn    *sql.DB
	stores  db.Stores
	secrets *memSecrets
	orgID   string
	key     ed25519.PrivateKey
	public  string

	conversationID string
	claimID        string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	f := &fixture{conn: conn, stores: sqlitestore.New(conn), orgID: runmode.LocalDefaultOrgID}
	f.secrets = &memSecrets{values: map[string]string{}}
	f.stores.Secrets = f.secrets

	f.key, f.public = newKey(t)
	f.secrets.set(integrations.KeyGitHubCommitSigningKey, marshalKey(t, f.key))
	f.registerSigner(t, signerAccount, "https://github.com", f.public, true)
	f.conversationID, f.claimID = f.engagement(t)
	return f
}

func newKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	public, err := commitsign.PublicKey(key)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	return key, public
}

func marshalKey(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	raw, err := commitsign.MarshalPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	return string(raw)
}

// registerSigner replaces the org's signer with account on host, active or
// left pending.
func (f *fixture) registerSigner(t *testing.T, account int64, host, public string, active bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.stores.OrgCommitSigners.Delete(ctx, f.orgID); err != nil && err != db.ErrNoSuchOrgCommitSigner {
		t.Fatalf("Delete signer: %v", err)
	}
	if _, err := f.stores.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
		OrgID: f.orgID, Host: host, GitHubUserID: account, Login: "octocat",
		EmailOverride: "signer@example.com", PublicKey: public,
	}); err != nil {
		t.Fatalf("Create signer: %v", err)
	}
	if active {
		if _, err := f.stores.OrgCommitSigners.Activate(ctx, f.orgID, public); err != nil {
			t.Fatalf("Activate signer: %v", err)
		}
	}
}

// engagement seeds a conversation and a live claim on it at boot epoch 1.
func (f *fixture) engagement(t *testing.T) (conversationID, claimID string) {
	t.Helper()
	conversationID = uuid.New().String()
	dbtest.SeedConversation(t, f.conn, domain.Conversation{ID: conversationID})
	return conversationID, dbtest.SeedActiveClaim(t, f.conn, conversationID, "executor-1", 1)
}

// request queues a signature request from the fixture's engagement, at
// bootEpoch, naming signer.
func (f *fixture) request(t *testing.T, signer domain.CommitSigningMode, bootEpoch int64, payload string) domain.CommitSignature {
	t.Helper()
	row, err := f.stores.CommitSignatures.InsertSystem(context.Background(), domain.CommitSignature{
		OrgID: f.orgID, ConversationID: f.conversationID, ClaimID: f.claimID, BootEpoch: bootEpoch,
		Signer: signer, SignerHost: "https://github.com", SignerUserID: signerAccount,
		Digest: commitsign.DigestOf([]byte(payload)).String(), State: domain.CommitSignatureRequested,
	})
	if err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}
	return row
}

// signedAt inserts a signature the org already made at, for the budgets.
func (f *fixture) signedAt(t *testing.T, at time.Time) {
	t.Helper()
	at = at.UTC()
	if _, err := f.stores.CommitSignatures.InsertSystem(context.Background(), domain.CommitSignature{
		OrgID: f.orgID, ConversationID: f.conversationID, ClaimID: f.claimID, BootEpoch: 1,
		Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: signerAccount,
		PublicKey: f.public, Digest: commitsign.DigestOf([]byte(at.String())).String(),
		State: domain.CommitSignatureSigned, Signature: "-----BEGIN SSH SIGNATURE-----", SignedAt: &at,
	}); err != nil {
		t.Fatalf("InsertSystem (signed): %v", err)
	}
}

func (f *fixture) row(t *testing.T, id string) domain.CommitSignature {
	t.Helper()
	got, err := f.stores.CommitSignatures.GetSystem(context.Background(), f.orgID, id)
	if err != nil || got == nil {
		t.Fatalf("GetSystem(%s) = %+v, %v", id, got, err)
	}
	return *got
}

// denials returns the org's commit_sign_denied rows.
func (f *fixture) denials(t *testing.T) []domain.ExternalAction {
	t.Helper()
	rows, _, err := f.stores.ExternalActions.ListByOrgSystem(context.Background(), f.orgID, domain.ExternalActionListOpts{Limit: 100})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	var out []domain.ExternalAction
	for _, r := range rows {
		if r.Action == domain.ActionCommitSignDenied {
			out = append(out, r)
		}
	}
	return out
}

// bell records what a signer publishes and, when wired, rings the doorbells
// an executor would hear sign_done on.
type bell struct {
	mu        sync.Mutex
	sent      []ctlbus.Message
	doorbells *Doorbells
	drop      bool
}

func (b *bell) publish(_ context.Context, msg ctlbus.Message) error {
	b.mu.Lock()
	b.sent = append(b.sent, msg)
	drop := b.drop
	b.mu.Unlock()
	if !drop && msg.Kind == KindSignDone {
		b.doorbells.Ring(msg.SignatureID)
	}
	return nil
}

func (b *bell) messages() []ctlbus.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ctlbus.Message(nil), b.sent...)
}

func (f *fixture) controlPlane(pod string, budgets Budgets, b *bell) *ControlPlane {
	return NewControlPlane(f.stores, budgets, pod, b.publish)
}

var roomyBudgets = Budgets{PerOrgMinute: DefaultMaxSignaturesPerOrgMinute, PerClaim: DefaultMaxSignaturesPerClaim}

func TestControlPlane_SignsARequestFromALiveClaim(t *testing.T) {
	f := newFixture(t)
	b := &bell{}
	req := f.request(t, domain.CommitSigningOrg, 1, "tree 4b825dc6\n")

	if !f.controlPlane("pod-a", roomyBudgets, b).Handle(context.Background(), f.orgID, req.ID) {
		t.Fatal("Handle = false, want this pod to claim the request")
	}
	got := f.row(t, req.ID)
	want, err := commitsign.SignDigest(f.key, commitsign.DigestOf([]byte("tree 4b825dc6\n")))
	if err != nil {
		t.Fatalf("SignDigest: %v", err)
	}
	if got.State != domain.CommitSignatureSigned || got.Signature != string(want) || got.PublicKey != f.public || got.ClaimedBy != "pod-a" {
		t.Errorf("row = state %q key %q by %q, want signed with the signer's key by pod-a", got.State, got.PublicKey, got.ClaimedBy)
	}
	if msgs := b.messages(); len(msgs) != 1 || msgs[0].Kind != KindSignDone || msgs[0].SignatureID != req.ID || msgs[0].OrgID != f.orgID {
		t.Errorf("published %+v, want one sign_done for the request", msgs)
	}
	if d := f.denials(t); len(d) != 0 {
		t.Errorf("a signature recorded %d commit_sign_denied rows, want none", len(d))
	}
}

func TestControlPlane_PodsRacingForOneRequestSignItOnce(t *testing.T) {
	f := newFixture(t)
	b := &bell{}
	req := f.request(t, domain.CommitSigningOrg, 1, "tree race\n")

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		took []string
	)
	start := make(chan struct{})
	for _, pod := range []string{"pod-a", "pod-b", "pod-c"} {
		cp := f.controlPlane(pod, roomyBudgets, b)
		wg.Go(func() {
			<-start
			if cp.Handle(context.Background(), f.orgID, req.ID) {
				mu.Lock()
				took = append(took, pod)
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	if len(took) != 1 {
		t.Fatalf("pods that handled the request = %v, want exactly one", took)
	}
	if got := f.row(t, req.ID); got.State != domain.CommitSignatureSigned || got.ClaimedBy != took[0] {
		t.Errorf("row = state %q by %q, want signed by %s", got.State, got.ClaimedBy, took[0])
	}
	if n, err := f.stores.CommitSignatures.ClaimSignaturesSystem(context.Background(), f.orgID, f.claimID); err != nil || n != 1 {
		t.Errorf("signatures for the claim = %d, %v; want one", n, err)
	}
	if msgs := b.messages(); len(msgs) != 1 {
		t.Errorf("published %d sign_done messages, want one", len(msgs))
	}
}

func TestControlPlane_RefusesAnEngagementThatNoLongerHoldsItsClaim(t *testing.T) {
	cases := map[string]struct {
		stage     func(t *testing.T, f *fixture)
		bootEpoch int64
	}{
		"a stale boot epoch": {bootEpoch: 2},
		"a released claim": {bootEpoch: 1, stage: func(t *testing.T, f *fixture) {
			if _, err := f.conn.Exec(`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'completed' WHERE id = ?`, f.claimID); err != nil {
				t.Fatalf("release: %v", err)
			}
		}},
		"a lapsed lease": {bootEpoch: 1, stage: func(t *testing.T, f *fixture) {
			dbtest.SeedExpiredClaimLease(t, f.conn, f.claimID)
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if c.stage != nil {
				c.stage(t, f)
			}
			req := f.request(t, domain.CommitSigningOrg, c.bootEpoch, "tree stale\n")
			if !f.controlPlane("pod-a", roomyBudgets, &bell{}).Handle(context.Background(), f.orgID, req.ID) {
				t.Fatal("Handle = false, want the request claimed")
			}
			got := f.row(t, req.ID)
			if got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != RefusalClaimInactive || got.Signature != "" {
				t.Errorf("row = state %q reason %q, want refused claim_inactive with nothing signed", got.State, got.RefusalReason)
			}
			denials := f.denials(t)
			if len(denials) != 1 {
				t.Fatalf("commit_sign_denied rows = %d, want one", len(denials))
			}
			d := denials[0]
			if d.Credential != domain.CredentialNone || d.ConversationID != f.conversationID || d.Target != "octocat" {
				t.Errorf("denial = credential %q conversation %q target %q, want none, the run's conversation, the signer's login",
					d.Credential, d.ConversationID, d.Target)
			}
			if reason := detailReason(t, d); reason != "claim_inactive" {
				t.Errorf("denial reason = %q, want claim_inactive", reason)
			}
		})
	}
}

func TestControlPlane_RefusesASignerThatChangedSinceTheSeal(t *testing.T) {
	other, otherPublic := newKey(t)
	cases := map[string]struct {
		stage  func(t *testing.T, f *fixture)
		signer domain.CommitSigningMode
		want   Refusal
		// status and reason are the signer's, stored on the refusal so the
		// run's line can say why signing stopped.
		status domain.CommitSignerStatus
		reason string
	}{
		"removed": {signer: domain.CommitSigningOrg, want: RefusalSignerRemoved, stage: func(t *testing.T, f *fixture) {
			if err := f.stores.OrgCommitSigners.Delete(context.Background(), f.orgID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		}},
		"another account": {signer: domain.CommitSigningOrg, want: RefusalSignerChanged, stage: func(t *testing.T, f *fixture) {
			f.secrets.set(integrations.KeyGitHubCommitSigningKey, marshalKey(t, other))
			f.registerSigner(t, 99, "https://github.com", otherPublic, true)
		}},
		"the same account on another host": {signer: domain.CommitSigningOrg, want: RefusalSignerChanged, stage: func(t *testing.T, f *fixture) {
			f.registerSigner(t, signerAccount, "https://ghe.example.com", f.public, true)
		}},
		"no longer active": {signer: domain.CommitSigningOrg, want: RefusalSignerInactive, status: domain.CommitSignerPending,
			stage: func(t *testing.T, f *fixture) {
				f.registerSigner(t, signerAccount, "https://github.com", f.public, false)
			}},
		"broken": {signer: domain.CommitSigningOrg, want: RefusalSignerInactive, status: domain.CommitSignerBroken, reason: "unverified_email",
			stage: func(t *testing.T, f *fixture) {
				if _, err := f.conn.Exec(`UPDATE org_commit_signers SET status = 'broken', status_reason = 'unverified_email' WHERE org_id = ?`, f.orgID); err != nil {
					t.Fatalf("break the signer: %v", err)
				}
			}},
		// A pod configured with no deployment signer cannot decide the
		// request, so the run tries again and may reach one that can.
		"a deployment request on a pod with no deployment signer": {signer: domain.CommitSigningDeployment, want: RefusalUnanswered},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if c.stage != nil {
				c.stage(t, f)
			}
			req := f.request(t, c.signer, 1, "tree signer\n")
			f.controlPlane("pod-a", roomyBudgets, &bell{}).Handle(context.Background(), f.orgID, req.ID)
			got := f.row(t, req.ID)
			if got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != c.want || got.Signature != "" {
				t.Errorf("row = state %q reason %q, want refused %s with nothing signed", got.State, got.RefusalReason, c.want)
			}
			if got.SignerStatus != c.status || got.SignerStatusReason != c.reason {
				t.Errorf("row signer status = (%q, %q), want (%q, %q)", got.SignerStatus, got.SignerStatusReason, c.status, c.reason)
			}
			if d := f.denials(t); len(d) != 0 {
				t.Errorf("a signer refusal recorded %d commit_sign_denied rows, want none: the signer's status says why", len(d))
			}
		})
	}
}

func TestControlPlane_EnforcesTheClaimBudget(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	f.signedAt(t, now.Add(-3*time.Hour))
	f.signedAt(t, now.Add(-2*time.Hour))
	cp := f.controlPlane("pod-a", Budgets{PerOrgMinute: 600, PerClaim: 2}, &bell{})

	for i := range 2 {
		req := f.request(t, domain.CommitSigningOrg, 1, "tree budget\n")
		cp.Handle(context.Background(), f.orgID, req.ID)
		got := f.row(t, req.ID)
		if got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != RefusalClaimBudget || got.RetryAfterSeconds != 0 || got.RefusalLimit != 2 {
			t.Errorf("request %d = state %q reason %q retry %d limit %d, want refused claim_budget at its limit of 2 with no retry",
				i, got.State, got.RefusalReason, got.RetryAfterSeconds, got.RefusalLimit)
		}
	}
	denials := f.denials(t)
	if len(denials) != 1 {
		t.Fatalf("commit_sign_denied rows after two refusals = %d, want one per conversation", len(denials))
	}
	if reason := detailReason(t, denials[0]); reason != "budget_exhausted" {
		t.Errorf("denial reason = %q, want budget_exhausted", reason)
	}

	// Another engagement has a budget of its own.
	f.conversationID, f.claimID = f.engagement(t)
	req := f.request(t, domain.CommitSigningOrg, 1, "tree fresh claim\n")
	cp.Handle(context.Background(), f.orgID, req.ID)
	if got := f.row(t, req.ID); got.State != domain.CommitSignatureSigned {
		t.Errorf("a fresh claim's request = state %q reason %q, want signed", got.State, got.RefusalReason)
	}
}

func TestControlPlane_EnforcesTheOrgMinuteBudget(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	f.signedAt(t, now.Add(-70*time.Second)) // outside the window
	f.signedAt(t, now.Add(-50*time.Second))
	f.signedAt(t, now.Add(-20*time.Second))
	f.signedAt(t, now.Add(-5*time.Second))
	cp := f.controlPlane("pod-a", Budgets{PerOrgMinute: 3, PerClaim: 5000}, &bell{})
	cp.now = func() time.Time { return now }

	req := f.request(t, domain.CommitSigningOrg, 1, "tree minute\n")
	cp.Handle(context.Background(), f.orgID, req.ID)
	got := f.row(t, req.ID)
	if got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != RefusalOrgMinuteBudget {
		t.Fatalf("row = state %q reason %q, want refused org_minute_budget", got.State, got.RefusalReason)
	}
	if got.RetryAfterSeconds != 10 {
		t.Errorf("retry after = %ds, want 10: the signature made 50s ago leaves the window in 10s", got.RetryAfterSeconds)
	}
	if got.RefusalLimit != 3 {
		t.Errorf("refusal limit = %d, want the per-minute budget of 3", got.RefusalLimit)
	}
	if d := f.denials(t); len(d) != 0 {
		t.Errorf("the per-minute budget recorded %d commit_sign_denied rows, want none", len(d))
	}

	// Once that signature has left the window, the next request signs.
	cp.now = func() time.Time { return now.Add(11 * time.Second) }
	next := f.request(t, domain.CommitSigningOrg, 1, "tree minute later\n")
	cp.Handle(context.Background(), f.orgID, next.ID)
	if got := f.row(t, next.ID); got.State != domain.CommitSignatureSigned {
		t.Errorf("after the retry-after, row = state %q reason %q, want signed", got.State, got.RefusalReason)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		oldest *time.Time
		want   int
	}{
		"a whole number of seconds":  {ptr(now.Add(-45 * time.Second)), 15},
		"rounded up":                 {ptr(now.Add(-45*time.Second - 500*time.Millisecond)), 15},
		"a part second is a second":  {ptr(now.Add(-59*time.Second - 999*time.Millisecond)), 1},
		"already out of the window":  {ptr(now.Add(-2 * time.Minute)), 1},
		"no oldest to wait on":       {nil, 1},
		"a moment ago is the window": {ptr(now), 60},
	} {
		if got := retryAfter(c.oldest, now); got != c.want {
			t.Errorf("%s: retryAfter = %d, want %d", name, got, c.want)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestControlPlane_ClosesARequestNobodyWaitsOnUnanswered(t *testing.T) {
	f := newFixture(t)
	req := f.request(t, domain.CommitSigningOrg, 1, "tree late\n")
	cp := f.controlPlane("pod-a", roomyBudgets, &bell{})
	cp.now = func() time.Time { return time.Now().Add(RequestTimeout + expiryMargin + time.Second) }

	cp.Handle(context.Background(), f.orgID, req.ID)
	if got := f.row(t, req.ID); got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != RefusalUnanswered {
		t.Errorf("row = state %q reason %q, want refused unanswered: the run stopped waiting", got.State, got.RefusalReason)
	}
}

// TestControlPlane_StoresADecisionWhoseContextEnded cancels the pod's context
// partway through a decision, as a shutdown does. The key read fails with it,
// and the request must still be closed and the run rung, not left signing for
// the run to wait out.
func TestControlPlane_StoresADecisionWhoseContextEnded(t *testing.T) {
	f := newFixture(t)
	b := &bell{}
	req := f.request(t, domain.CommitSigningOrg, 1, "tree shutdown\n")
	ctx, cancel := context.WithCancel(context.Background())
	f.secrets.onGet = func() error {
		cancel()
		return context.Canceled
	}

	if !f.controlPlane("pod-a", roomyBudgets, b).Handle(ctx, f.orgID, req.ID) {
		t.Fatal("Handle = false, want the request claimed")
	}
	if got := f.row(t, req.ID); got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != RefusalUnanswered {
		t.Errorf("row = state %q reason %q, want refused unanswered rather than left signing", got.State, got.RefusalReason)
	}
	if msgs := b.messages(); len(msgs) != 1 || msgs[0].Kind != KindSignDone {
		t.Errorf("published %+v, want sign_done for the closed request", msgs)
	}
}

func TestControlPlane_SignsWithTheKeyTheSignerNames(t *testing.T) {
	t.Run("from the pending secret mid-rotation", func(t *testing.T) {
		f := newFixture(t)
		stale, _ := newKey(t)
		f.secrets.set(integrations.KeyGitHubCommitSigningKey, marshalKey(t, stale))
		f.secrets.set(integrations.KeyGitHubCommitSigningKeyPending, marshalKey(t, f.key))
		req := f.request(t, domain.CommitSigningOrg, 1, "tree rotate\n")
		f.controlPlane("pod-a", roomyBudgets, &bell{}).Handle(context.Background(), f.orgID, req.ID)
		if got := f.row(t, req.ID); got.State != domain.CommitSignatureSigned || got.PublicKey != f.public {
			t.Errorf("row = state %q key %q, want signed with the signer's key", got.State, got.PublicKey)
		}
	})
	t.Run("unanswered with no key for it", func(t *testing.T) {
		f := newFixture(t)
		f.secrets.set(integrations.KeyGitHubCommitSigningKey, "")
		req := f.request(t, domain.CommitSigningOrg, 1, "tree keyless\n")
		f.controlPlane("pod-a", roomyBudgets, &bell{}).Handle(context.Background(), f.orgID, req.ID)
		if got := f.row(t, req.ID); got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != RefusalUnanswered {
			t.Errorf("row = state %q reason %q, want refused unanswered, which the run retries", got.State, got.RefusalReason)
		}
	})
}

// TestRequester_GetsItsAnswerByDoorbell runs both halves end to end. The poll
// is an hour, so only the sign_done doorbell can wake the waiting request.
func TestRequester_GetsItsAnswerByDoorbell(t *testing.T) {
	f := newFixture(t)
	doorbells := NewDoorbells()
	cp := f.controlPlane("pod-a", roomyBudgets, &bell{doorbells: doorbells})
	r := &Requester{
		Signatures: f.stores.CommitSignatures,
		Doorbells:  doorbells,
		Publish: func(_ context.Context, msg ctlbus.Message) error {
			if msg.Kind == KindSignRequest {
				go cp.Handle(context.Background(), msg.OrgID, msg.SignatureID)
			}
			return nil
		},
		PollInterval: time.Hour,
		Timeout:      10 * time.Second,
	}
	digest := commitsign.DigestOf([]byte("tree end to end\n"))
	res, err := r.Request(context.Background(), f.requestFor(digest))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	want, _ := commitsign.SignDigest(f.key, digest)
	if res.Outcome != OutcomeSigned || res.Signature != string(want) || res.SignatureID == "" {
		t.Errorf("result = %+v, want the signature over the digest", res)
	}
	if got := f.row(t, res.SignatureID); got.ConversationID != f.conversationID || got.ClaimID != f.claimID || got.BootEpoch != 1 {
		t.Errorf("row names conversation %q claim %q epoch %d, want the run's engagement", got.ConversationID, got.ClaimID, got.BootEpoch)
	}
}

func TestRequester_ReadsADecisionWhoseDoorbellWasDropped(t *testing.T) {
	f := newFixture(t)
	doorbells := NewDoorbells()
	cp := f.controlPlane("pod-a", Budgets{PerOrgMinute: 1, PerClaim: 5000}, &bell{doorbells: doorbells, drop: true})
	f.signedAt(t, time.Now().Add(-30*time.Second))
	r := &Requester{
		Signatures: f.stores.CommitSignatures,
		Doorbells:  doorbells,
		Publish: func(_ context.Context, msg ctlbus.Message) error {
			if msg.Kind == KindSignRequest {
				go cp.Handle(context.Background(), msg.OrgID, msg.SignatureID)
			}
			return nil
		},
		PollInterval: 20 * time.Millisecond,
		Timeout:      10 * time.Second,
	}
	res, err := r.Request(context.Background(), f.requestFor(commitsign.DigestOf([]byte("tree dropped done\n"))))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if res.Outcome != OutcomeRefused || res.Refusal != RefusalOrgMinuteBudget || res.RetryAfterSeconds < 29 || res.RetryAfterSeconds > 31 || res.Limit != 1 {
		t.Errorf("result = %+v, want refused org_minute_budget at its limit of 1 with about 30s to wait", res)
	}
}

// TestRequester_ASweepFindsADroppedRequest drops the sign_request doorbell, so
// only a control pod's sweep can reach the request.
func TestRequester_ASweepFindsADroppedRequest(t *testing.T) {
	reader := useTestInstruments(t)
	f := newFixture(t)
	doorbells := NewDoorbells()
	cp := f.controlPlane("pod-a", roomyBudgets, &bell{doorbells: doorbells})
	r := &Requester{
		Signatures:   f.stores.CommitSignatures,
		Doorbells:    doorbells,
		Publish:      func(context.Context, ctlbus.Message) error { return nil },
		PollInterval: time.Hour,
		Timeout:      10 * time.Second,
	}

	done := make(chan Result, 1)
	go func() {
		res, err := r.Request(context.Background(), f.requestFor(commitsign.DigestOf([]byte("tree swept\n"))))
		if err != nil {
			t.Errorf("Request: %v", err)
		}
		done <- res
	}()
	sweep := time.NewTicker(50 * time.Millisecond)
	defer sweep.Stop()
	for {
		select {
		case res := <-done:
			if res.Outcome != OutcomeSigned {
				t.Fatalf("result = %+v, want signed", res)
			}
			if n := counterTotal(t, reader, "commit_signing.swept"); n != 1 {
				t.Errorf("swept = %d, want the one request the sweep found", n)
			}
			return
		case <-sweep.C:
			cp.Sweep(context.Background())
		case <-time.After(5 * time.Second):
			t.Fatal("the sweep never answered the request")
		}
	}
}

func TestRequester_TimesOutUnanswered(t *testing.T) {
	reader := useTestInstruments(t)
	f := newFixture(t)
	r := &Requester{
		Signatures:   f.stores.CommitSignatures,
		Doorbells:    NewDoorbells(),
		Publish:      func(context.Context, ctlbus.Message) error { return nil },
		PollInterval: 10 * time.Millisecond,
		Timeout:      100 * time.Millisecond,
	}
	res, err := r.Request(context.Background(), f.requestFor(commitsign.DigestOf([]byte("tree nobody\n"))))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if res.Outcome != OutcomeUnanswered || res.Signature != "" {
		t.Errorf("result = %+v, want unanswered", res)
	}
	if got := f.row(t, res.SignatureID); got.State != domain.CommitSignatureRequested {
		t.Errorf("row = state %q, want still a request for a pod to close", got.State)
	}
	if n := counterTotal(t, reader, "commit_signatures"); n != 1 {
		t.Errorf("commit_signatures = %d, want the one unanswered request", n)
	}
	if RequestTimeout != 30*time.Second {
		t.Errorf("RequestTimeout = %s, want the 30s the run's message promises", RequestTimeout)
	}
}

func (f *fixture) requestFor(digest commitsign.Digest) Request {
	return Request{
		OrgID: f.orgID, ConversationID: f.conversationID, ClaimID: f.claimID, BootEpoch: 1,
		Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: signerAccount,
		Digest: digest,
	}
}

func detailReason(t *testing.T, a domain.ExternalAction) string {
	t.Helper()
	var detail struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(a.DetailJSON), &detail); err != nil {
		t.Fatalf("decode detail %q: %v", a.DetailJSON, err)
	}
	return detail.Reason
}

// useTestInstruments points the package's instruments at a manual reader for
// the rest of the test.
func useTestInstruments(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	current()
	prev := global.Load()
	global.Store(newInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { global.Store(prev) })
	return reader
}

// counterTotal sums every series of the named counter.
func counterTotal(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}
