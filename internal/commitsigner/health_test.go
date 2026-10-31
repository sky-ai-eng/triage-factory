package commitsigner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
)

// healthGitHub is a fake GitHub serving what the health checks read: the
// signer's account by id, its signing-key listing, and the verification of
// commits in acme/app. Tests mutate it between checks to play the admin
// removing a key or GitHub renaming the account.
type healthGitHub struct {
	mu sync.Mutex
	// id is the account's id, signerAccount when zero; kind is its type,
	// User when empty.
	id      int64
	kind    string
	login   string
	name    string
	deleted bool
	keys    []string
	// commits is each known commit's verification in acme/app; a commit not
	// in it answers 404. failing makes every commit read a 403, as for a
	// repository the credential lost access to.
	commits map[string]ghclient.CommitVerification
	failing bool
	// limited makes every account read a 403, as a rate limit does, and
	// accountReads counts the account reads made.
	limited      bool
	accountReads int
}

func newHealthGitHub(t *testing.T, key string) (*healthGitHub, string) {
	t.Helper()
	gh := &healthGitHub{login: "octocat", keys: []string{key}, commits: map[string]ghclient.CommitVerification{}}
	ts := httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(ts.Close)
	return gh, ts.URL
}

func (g *healthGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	notFound := func() { http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) }
	id, kind := g.id, g.kind
	if id == 0 {
		id = signerAccount
	}
	if kind == "" {
		kind = ghclient.AccountTypeUser
	}
	switch {
	case g.limited && (strings.HasPrefix(path, "/user/") || strings.HasPrefix(path, "/users/")):
		g.accountReads++
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	case path == fmt.Sprintf("/user/%d", id), path == "/users/"+g.login:
		g.accountReads++
		if g.deleted {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "login": g.login, "type": kind, "name": g.name})
	case path == "/users/"+g.login+"/ssh_signing_keys" && !g.deleted:
		items := []map[string]any{}
		for i, k := range g.keys {
			items = append(items, map[string]any{"id": i + 1, "key": k})
		}
		_ = json.NewEncoder(w).Encode(items)
	case strings.HasPrefix(path, "/repos/acme/app/commits/"):
		if g.failing {
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			return
		}
		v, ok := g.commits[strings.TrimPrefix(path, "/repos/acme/app/commits/")]
		if !ok {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{
			"committer":    map[string]any{"email": v.CommitterEmail},
			"verification": map[string]any{"verified": v.Verified, "reason": v.Reason},
		}})
	default:
		notFound()
	}
}

func (g *healthGitHub) set(mutate func(g *healthGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	mutate(g)
}

// healthResolver routes every GitHub client at the fake.
type healthResolver struct{ url string }

func (r healthResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, ""), nil
}

func (r healthResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, ""), nil
}

func (r healthResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return ghclient.NewClient(r.url, ""), nil
}

func (healthResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("no token in this test")
}

func (r healthResolver) BaseURLFor(context.Context, string) (string, error) { return r.url, nil }

func (healthResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}

// healthChecker is a checker over the fixture's stores and a fake GitHub
// holding the signer's key. Its clock runs ahead of the database's by the
// interval, so every Check is due a periodic check (registration and every
// check stamp checked_at on the database's clock); notDue sets it back, for
// the Checks that come before the interval has passed.
func (f *fixture) healthChecker(t *testing.T) (h *HealthChecker, gh *healthGitHub, notDue func(bool)) {
	t.Helper()
	gh, url := newHealthGitHub(t, f.public)
	ahead := true
	h = &HealthChecker{Stores: f.stores, GitHub: healthResolver{url: url}}
	h.now = func() time.Time {
		if ahead {
			return time.Now().Add(PeriodicCheckInterval + time.Minute)
		}
		return time.Now()
	}
	return h, gh, func(not bool) { ahead = !not }
}

func (f *fixture) signer(t *testing.T) domain.OrgCommitSigner {
	t.Helper()
	s, err := f.stores.OrgCommitSigners.GetSystem(context.Background(), f.orgID)
	if err != nil || s == nil {
		t.Fatalf("GetSystem = %+v, %v", s, err)
	}
	return *s
}

// pushedSignature records a signature the signer made for commit sha and its
// push to acme/app, as the run's push recorder stamps it.
func (f *fixture) pushedSignature(t *testing.T, sha string) domain.CommitSignature {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	row, err := f.stores.CommitSignatures.InsertSystem(ctx, domain.CommitSignature{
		OrgID: f.orgID, ConversationID: f.conversationID, ClaimID: f.claimID, BootEpoch: 1,
		Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: signerAccount,
		PublicKey: f.public, Digest: "ab", State: domain.CommitSignatureSigned,
		Signature: "-----BEGIN SSH SIGNATURE-----", SignedAt: &at, CommitSHA: sha,
	})
	if err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}
	repo, err := f.stores.Repos.FindOrCreateByRefSystem(ctx, f.orgID, domain.RepoRefFromSlug("https://github.com", "acme/app"))
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if _, err := f.stores.CommitSignatures.MarkPushedSystem(ctx, f.orgID, row.ID, repo.ID); err != nil {
		t.Fatalf("MarkPushedSystem: %v", err)
	}
	return row
}

// healthChecks points the checks counter at a manual reader and returns a
// reader of its series, "check result" → count.
func healthChecks(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "commit_signer.checks" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					check, _ := dp.Attributes.Value(attribute.Key("check"))
					result, _ := dp.Attributes.Value(attribute.Key("result"))
					out[check.AsString()+" "+result.AsString()] += dp.Value
				}
			}
		}
		return out
	}
}

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
	shaC = "3333333333333333333333333333333333333333"
)

// A key removed from the account breaks the signer within one check, the next
// signature is refused with signing turned off, and the same check restores
// the signer once the key is back — but not before the interval has passed.
func TestHealth_KeyRemovedBreaksAndRecovers(t *testing.T) {
	f := newFixture(t)
	checks := healthChecks(t)
	h, gh, notDue := f.healthChecker(t)
	ctx := context.Background()
	runSigner := f.runSigner(t)

	h.Check(ctx, f.orgID)
	if got := f.signer(t); got.Status != domain.CommitSignerActive {
		t.Fatalf("with the key on the account the signer is %q, want active", got.Status)
	}

	gh.set(func(g *healthGitHub) { g.keys = nil })
	h.Check(ctx, f.orgID)
	broken := f.signer(t)
	if broken.Status != domain.CommitSignerBroken || broken.StatusReason != domain.CommitSignerReasonKeyMissing {
		t.Fatalf("after the key was removed the signer is %q (%q), want broken for key_missing", broken.Status, broken.StatusReason)
	}

	res := f.localSigner().Sign(ctx, f.localRequest(runSigner, f.signerPayload(runSigner)))
	line := RefusalLine(res, runSigner)
	if res.Outcome != OutcomeRefused ||
		!strings.Contains(line, "commit signing is off for the rest of this run: the signer's key is no longer on its GitHub account.") {
		t.Errorf("a signature after the break = %+v, line %q; want the signing-is-off refusal naming the missing key", res, line)
	}

	// Back on the account, but the check is not due again yet.
	gh.set(func(g *healthGitHub) { g.keys = []string{f.public} })
	notDue(true)
	h.Check(ctx, f.orgID)
	if got := f.signer(t); got.Status != domain.CommitSignerBroken {
		t.Errorf("a check inside the interval moved the signer to %q, want it left broken until the next check is due", got.Status)
	}
	notDue(false)
	h.Check(ctx, f.orgID)
	if got := f.signer(t); got.Status != domain.CommitSignerActive || got.StatusReason != "" {
		t.Errorf("after the key came back the signer is %q (%q), want active", got.Status, got.StatusReason)
	}
	if res := f.localSigner().Sign(ctx, f.localRequest(runSigner, f.signerPayload(runSigner))); res.Outcome != OutcomeSigned {
		t.Errorf("a signature after the recovery = %+v, want signed", res)
	}
	if got, want := checks(), map[string]int64{"periodic valid": 2, "periodic key_missing": 1}; !maps.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
}

// The periodic check reads the account by its id, so a renamed login and a
// changed profile name are picked up, and an account GitHub no longer has is
// one whose key is gone.
func TestHealth_RefreshesTheAccountByID(t *testing.T) {
	f := newFixture(t)
	h, gh, _ := f.healthChecker(t)
	ctx := context.Background()

	gh.set(func(g *healthGitHub) { g.login, g.name = "octocat-renamed", "The Octocat" })
	h.Check(ctx, f.orgID)
	got := f.signer(t)
	if got.Login != "octocat-renamed" || got.Name != "The Octocat" || got.Status != domain.CommitSignerActive {
		t.Errorf("after a rename the signer is %q (%q) %q, want the new login and name, still active", got.Login, got.Name, got.Status)
	}

	gh.set(func(g *healthGitHub) { g.deleted = true })
	h.Check(ctx, f.orgID)
	got = f.signer(t)
	if got.Status != domain.CommitSignerBroken || got.StatusReason != domain.CommitSignerReasonKeyMissing || got.Login != "octocat-renamed" {
		t.Errorf("with the account gone the signer is %q (%q) %q, want broken for key_missing, login kept", got.Status, got.StatusReason, got.Login)
	}
}

// A pushed commit GitHub reads as unverified_email breaks the signer with that
// reason, which the periodic check does not clear, and the next signature is
// refused saying why.
func TestHealth_PushedCommitWithUnverifiedEmailBreaksTheSigner(t *testing.T) {
	f := newFixture(t)
	checks := healthChecks(t)
	h, gh, _ := f.healthChecker(t)
	ctx := context.Background()
	runSigner := f.runSigner(t)

	sig := f.pushedSignature(t, shaA)
	gh.set(func(g *healthGitHub) {
		g.commits[shaA] = ghclient.CommitVerification{Reason: "unverified_email", CommitterEmail: "signer@example.com"}
	})
	h.Check(ctx, f.orgID)

	got := f.signer(t)
	if got.Status != domain.CommitSignerBroken || got.StatusReason != "unverified_email" {
		t.Fatalf("after the post-push check the signer is %q (%q), want broken for unverified_email", got.Status, got.StatusReason)
	}
	if row := f.row(t, sig.ID); row.VerificationReason != "unverified_email" || row.CheckedAt == nil {
		t.Errorf("the pushed commit's row holds %q at %v, want GitHub's reason and a check time", row.VerificationReason, row.CheckedAt)
	}

	res := f.localSigner().Sign(ctx, f.localRequest(runSigner, f.signerPayload(runSigner)))
	line := RefusalLine(res, runSigner)
	if res.Outcome != OutcomeRefused || !strings.Contains(line,
		"commit signing is off for the rest of this run: GitHub reported a recent commit's email as not verified on the signer's account.") {
		t.Errorf("a signature after the break = %+v, line %q; want the signing-is-off refusal naming the email", res, line)
	}

	// The key is on the account, but only a Verify clears a commit's failure.
	h.Check(ctx, f.orgID)
	if got := f.signer(t); got.Status != domain.CommitSignerBroken || got.StatusReason != "unverified_email" {
		t.Errorf("after a periodic check the signer is %q (%q), want it still broken for unverified_email", got.Status, got.StatusReason)
	}
	if got, want := checks(), map[string]int64{"periodic valid": 2, "post_push unverified_email": 1}; !maps.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
}

// A pushed commit that verifies, one GitHub no longer has, and one that fails
// under an address the signer no longer commits as are each recorded and none
// breaks the signer; one GitHub could not be read for stays unchecked for the
// next cycle.
func TestHealth_PushedCommitsThatDoNotBreakTheSigner(t *testing.T) {
	f := newFixture(t)
	checks := healthChecks(t)
	h, gh, notDue := f.healthChecker(t)
	ctx := context.Background()

	valid := f.pushedSignature(t, shaA)
	gone := f.pushedSignature(t, shaB)
	stale := f.pushedSignature(t, shaC)
	gh.set(func(g *healthGitHub) {
		g.commits[shaA] = ghclient.CommitVerification{Verified: true, Reason: "valid", CommitterEmail: "signer@example.com"}
		g.commits[shaC] = ghclient.CommitVerification{Reason: "no_user", CommitterEmail: "old-address@example.com"}
	})
	h.Check(ctx, f.orgID)

	if got := f.signer(t); got.Status != domain.CommitSignerActive {
		t.Errorf("the signer is %q (%q), want active", got.Status, got.StatusReason)
	}
	for _, tc := range []struct {
		name   string
		id     string
		reason string
	}{{"verified", valid.ID, "valid"}, {"gone", gone.ID, ""}, {"stale address", stale.ID, "no_user"}} {
		if row := f.row(t, tc.id); row.VerificationReason != tc.reason || row.CheckedAt == nil {
			t.Errorf("%s commit holds %q at %v, want %q with a check time", tc.name, row.VerificationReason, row.CheckedAt, tc.reason)
		}
	}

	unread := f.pushedSignature(t, "4444444444444444444444444444444444444444")
	gh.set(func(g *healthGitHub) { g.failing = true })
	notDue(true)
	h.Check(ctx, f.orgID)
	if row := f.row(t, unread.ID); row.CheckedAt != nil {
		t.Errorf("a commit GitHub could not be read for was marked checked at %v, want it left for the next cycle", row.CheckedAt)
	}
	if got, want := checks(), map[string]int64{
		"periodic valid": 1, "post_push valid": 1, "post_push no_user": 1, "post_push error": 1,
	}; !maps.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
}

// A signer broken because its key went missing does not take unknown_key from
// a commit pushed while the key was gone: that is the same fault, and the
// periodic check still restores the signer when the key is back.
func TestHealth_MissingKeyIsNotReplacedByItsUnknownKeyCommit(t *testing.T) {
	f := newFixture(t)
	h, gh, notDue := f.healthChecker(t)
	ctx := context.Background()

	gh.set(func(g *healthGitHub) { g.keys = nil })
	h.Check(ctx, f.orgID)
	f.pushedSignature(t, shaA)
	gh.set(func(g *healthGitHub) {
		g.commits[shaA] = ghclient.CommitVerification{Reason: "unknown_key", CommitterEmail: "signer@example.com"}
	})
	notDue(true)
	h.Check(ctx, f.orgID)
	if got := f.signer(t); got.StatusReason != domain.CommitSignerReasonKeyMissing {
		t.Fatalf("the signer's reason is %q, want key_missing kept", got.StatusReason)
	}

	gh.set(func(g *healthGitHub) { g.keys = []string{f.public} })
	notDue(false)
	h.Check(ctx, f.orgID)
	if got := f.signer(t); got.Status != domain.CommitSignerActive {
		t.Errorf("after the key came back the signer is %q (%q), want active", got.Status, got.StatusReason)
	}
}

// The status gauges count every signer by status, zero included, and report
// each broken one with its reason.
func TestSignerGauges(t *testing.T) {
	f := newFixture(t)
	reader := sdkmetric.NewManualReader()
	g := NewSignerGauges(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), f.stores.OrgCommitSigners)
	t.Cleanup(g.Close)
	ctx := context.Background()

	collect := func() (map[string]int64, map[string]int64) {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		statuses, broken := map[string]int64{}, map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
					switch m.Name {
					case "commit_signers":
						status, _ := dp.Attributes.Value(attribute.Key("status"))
						statuses[status.AsString()] = dp.Value
					case "commit_signer.broken":
						org, _ := dp.Attributes.Value(attribute.Key("org.id"))
						reason, _ := dp.Attributes.Value(attribute.Key("reason"))
						broken[org.AsString()+" "+reason.AsString()] = dp.Value
					}
				}
			}
		}
		return statuses, broken
	}

	if statuses, _ := collect(); len(statuses) != 0 {
		t.Errorf("before the first sample the gauges report %v, want nothing", statuses)
	}
	g.Tick(ctx)
	statuses, broken := collect()
	if want := map[string]int64{"pending": 0, "active": 1, "broken": 0}; !maps.Equal(statuses, want) || len(broken) != 0 {
		t.Errorf("an active signer reports %v and broken %v, want %v and none broken", statuses, broken, want)
	}

	if _, err := f.stores.OrgCommitSigners.BreakSystem(ctx, f.orgID, f.public, "expired_key"); err != nil {
		t.Fatalf("BreakSystem: %v", err)
	}
	g.Tick(ctx)
	statuses, broken = collect()
	if want := map[string]int64{"pending": 0, "active": 0, "broken": 1}; !maps.Equal(statuses, want) {
		t.Errorf("a broken signer reports %v, want %v", statuses, want)
	}
	if want := map[string]int64{f.orgID + " other": 1}; !maps.Equal(broken, want) {
		t.Errorf("tf_commit_signer_broken = %v, want %v: a reason the metrics do not name folds into other", broken, want)
	}
}

// A read that ends in GitHub's verification service failing is no verdict on
// the commit: the signer is left as it is and the commit is read again next
// cycle, when a real answer counts.
func TestHealth_VerificationUnavailableIsReadAgain(t *testing.T) {
	f := newFixture(t)
	checks := healthChecks(t)
	h, gh, notDue := f.healthChecker(t)
	ctx := context.Background()

	sig := f.pushedSignature(t, shaA)
	for _, reason := range []string{"gpgverify_unavailable", "gpgverify_error"} {
		gh.set(func(g *healthGitHub) {
			g.commits[shaA] = ghclient.CommitVerification{Reason: reason, CommitterEmail: "signer@example.com"}
		})
		h.Check(ctx, f.orgID)
		notDue(true)
		if got := f.signer(t); got.Status != domain.CommitSignerActive {
			t.Errorf("after %s the signer is %q (%q), want active", reason, got.Status, got.StatusReason)
		}
		if row := f.row(t, sig.ID); row.CheckedAt != nil || row.VerificationReason != "" {
			t.Errorf("after %s the commit holds %q at %v, want it left unchecked", reason, row.VerificationReason, row.CheckedAt)
		}
	}

	gh.set(func(g *healthGitHub) {
		g.commits[shaA] = ghclient.CommitVerification{Reason: "unverified_email", CommitterEmail: "signer@example.com"}
	})
	h.Check(ctx, f.orgID)
	if got := f.signer(t); got.Status != domain.CommitSignerBroken || got.StatusReason != "unverified_email" {
		t.Errorf("once GitHub answered, the signer is %q (%q), want broken for unverified_email", got.Status, got.StatusReason)
	}
	if got, want := checks(), map[string]int64{"periodic valid": 1, "post_push error": 2, "post_push unverified_email": 1}; !maps.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
}
