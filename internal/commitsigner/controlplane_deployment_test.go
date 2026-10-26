package commitsigner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
)

// onDeploymentApp moves the fixture's org onto the deployment App with no
// signer of its own, and gives it a deployment signer whose health is status.
func (f *fixture) onDeploymentApp(t *testing.T, status domain.CommitSignerStatus, reason string) (*DeploymentSigner, *memDeploymentSigners) {
	t.Helper()
	ctx := context.Background()
	if err := f.stores.OrgCommitSigners.Delete(ctx, f.orgID); err != nil {
		t.Fatalf("Delete the org's signer: %v", err)
	}
	if _, err := f.stores.Orgs.SetSourceBaseURL(ctx, f.orgID, eventsource.KindGitHub, "https://github.com"); err != nil {
		t.Fatalf("SetSourceBaseURL: %v", err)
	}
	if _, err := f.stores.Orgs.SetGitHubCredentialClass(ctx, f.orgID, domain.GitHubCredentialClassManagedApp); err != nil {
		t.Fatalf("SetGitHubCredentialClass: %v", err)
	}
	d := newDeploymentSigner(t)
	health := newMemDeploymentSigners()
	health.set(deploymentRow(d, status, reason))
	f.stores.DeploymentCommitSigners = health
	return d, health
}

// requestAs queues a signature request from the fixture's engagement as
// signer on account.
func (f *fixture) requestAs(t *testing.T, signer domain.CommitSigningMode, account int64, payload string) domain.CommitSignature {
	t.Helper()
	row, err := f.stores.CommitSignatures.InsertSystem(context.Background(), domain.CommitSignature{
		OrgID: f.orgID, ConversationID: f.conversationID, ClaimID: f.claimID, BootEpoch: 1,
		Signer: signer, SignerHost: "https://github.com", SignerUserID: account,
		Digest: commitsign.DigestOf([]byte(payload)).String(), State: domain.CommitSignatureRequested,
	})
	if err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}
	return row
}

// A managed workspace with no signer of its own signs as the deployment signer,
// with the key from the control pod's config: the secret store holds no key
// for it, and the signature is the configured key's.
func TestControlPlane_SignsAsTheDeploymentSigner(t *testing.T) {
	f := newFixture(t)
	d, _ := f.onDeploymentApp(t, domain.CommitSignerActive, "")
	f.secrets.values = map[string]string{}
	cp := f.controlPlane("pod-a", roomyBudgets, &bell{})
	cp.SetDeploymentSigner(d)

	payload := "tree deployment\n"
	req := f.requestAs(t, domain.CommitSigningDeployment, deploymentAccount, payload)
	if !cp.Handle(context.Background(), f.orgID, req.ID) {
		t.Fatal("Handle = false, want this pod to claim the request")
	}
	got := f.row(t, req.ID)
	want, err := commitsign.SignDigest(d.key, commitsign.DigestOf([]byte(payload)))
	if err != nil {
		t.Fatalf("SignDigest: %v", err)
	}
	if got.State != domain.CommitSignatureSigned || got.Signature != string(want) || got.PublicKey != d.PublicKey {
		t.Errorf("row = state %q reason %q key %q, want signed with the deployment signer's key", got.State, got.RefusalReason, got.PublicKey)
	}
}

// A deployment signer that is not active refuses with the line that turns
// signing off for the rest of the run, naming why from its health; an org that
// has registered a signer of its own since the run started is refused as a
// change of signer; and the budgets still count.
func TestControlPlane_RefusesADeploymentSignerThatCannotSign(t *testing.T) {
	runSigner := Identity{Name: "Triage Factory", Email: "90001+TriageFactory@users.noreply.github.com", AccountID: "90001", Mode: domain.CommitSigningDeployment}
	for _, tc := range []struct {
		name     string
		status   domain.CommitSignerStatus
		reason   string
		stage    func(t *testing.T, f *fixture)
		budgets  Budgets
		want     Refusal
		wantLine string
	}{
		{name: "broken, its key gone", status: domain.CommitSignerBroken, reason: domain.CommitSignerReasonKeyMissing, want: RefusalSignerInactive,
			wantLine: "commit signing is off for the rest of this run: the signer's key is no longer on its GitHub account. Nothing was committed."},
		{name: "broken by a pushed commit", status: domain.CommitSignerBroken, reason: "unverified_email", want: RefusalSignerInactive,
			wantLine: "commit signing is off for the rest of this run: GitHub reported a recent commit's email as not verified on the signer's account."},
		{name: "the org registered its own signer", status: domain.CommitSignerActive, want: RefusalSignerChanged,
			stage: func(t *testing.T, f *fixture) {
				f.registerSigner(t, signerAccount, "https://github.com", f.public, true)
			},
			wantLine: "commit signing is off for the rest of this run: the org now signs as a different account."},
		{name: "the org's per-minute budget", status: domain.CommitSignerActive, budgets: Budgets{PerOrgMinute: 1, PerClaim: 10},
			stage: func(t *testing.T, f *fixture) {
				f.signedAt(t, time.Now())
			},
			want: RefusalOrgMinuteBudget, wantLine: "this org has made 1 signed commits in the last minute, its limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			d, _ := f.onDeploymentApp(t, tc.status, tc.reason)
			if tc.stage != nil {
				tc.stage(t, f)
			}
			budgets := tc.budgets
			if budgets == (Budgets{}) {
				budgets = roomyBudgets
			}
			cp := f.controlPlane("pod-a", budgets, &bell{})
			cp.SetDeploymentSigner(d)

			req := f.requestAs(t, domain.CommitSigningDeployment, deploymentAccount, "tree refused\n")
			cp.Handle(context.Background(), f.orgID, req.ID)
			got := f.row(t, req.ID)
			if got.State != domain.CommitSignatureRefused || Refusal(got.RefusalReason) != tc.want || got.Signature != "" {
				t.Fatalf("row = state %q reason %q, want refused %s with nothing signed", got.State, got.RefusalReason, tc.want)
			}
			line := RefusalLine(Result{
				Outcome: OutcomeRefused, Refusal: Refusal(got.RefusalReason), RetryAfterSeconds: got.RetryAfterSeconds,
				Limit: got.RefusalLimit, SignerStatus: got.SignerStatus, SignerReason: got.SignerStatusReason,
			}, runSigner)
			if !strings.Contains(line, tc.wantLine) {
				t.Errorf("the run's line = %q, want it to say %q", line, tc.wantLine)
			}
		})
	}
}

// A request a sandbox makes as the deployment signer for an org that does not
// resolve to it gains nothing: its own App's workspace, or one whose
// deployment signer has no account yet, is refused, never signed.
func TestControlPlane_DeploymentRequestForAnOrgNotOnIt(t *testing.T) {
	for name, stage := range map[string]func(t *testing.T, f *fixture, health *memDeploymentSigners, d *DeploymentSigner){
		"its own App": func(t *testing.T, f *fixture, _ *memDeploymentSigners, _ *DeploymentSigner) {
			if _, err := f.stores.Orgs.SetGitHubCredentialClass(context.Background(), f.orgID, domain.GitHubCredentialClassBYOApp); err != nil {
				t.Fatal(err)
			}
		},
		"no account read yet": func(t *testing.T, _ *fixture, health *memDeploymentSigners, d *DeploymentSigner) {
			health.set(domain.DeploymentCommitSigner{Host: d.Host, PublicKey: d.PublicKey, Login: d.Login, Status: domain.CommitSignerPending})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			d, health := f.onDeploymentApp(t, domain.CommitSignerActive, "")
			stage(t, f, health, d)
			cp := f.controlPlane("pod-a", roomyBudgets, &bell{})
			cp.SetDeploymentSigner(d)
			req := f.requestAs(t, domain.CommitSigningDeployment, deploymentAccount, "tree stray\n")
			cp.Handle(context.Background(), f.orgID, req.ID)
			if got := f.row(t, req.ID); got.State != domain.CommitSignatureRefused || got.Signature != "" ||
				(Refusal(got.RefusalReason) != RefusalSignerRemoved && Refusal(got.RefusalReason) != RefusalSignerChanged) {
				t.Errorf("row = state %q reason %q, want refused for the signer with nothing signed", got.State, got.RefusalReason)
			}
		})
	}
}
