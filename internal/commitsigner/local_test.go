package commitsigner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner/gitsig"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

const testTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// commitPayload is the object git hands its signing program for a root
// commit by committer.
func commitPayload(committer string) []byte {
	return []byte("tree " + testTree + "\n" +
		"author Someone Else <else@example.com> 1600000000 -0700\n" +
		"committer " + committer + " 1700000000 +0100\n" +
		"\n" +
		"subject\n")
}

// localSigner is a signer over the fixture's stores with the default budgets.
func (f *fixture) localSigner() *LocalSigner {
	return &LocalSigner{Stores: f.stores, Budgets: Budgets{PerOrgMinute: DefaultMaxSignaturesPerOrgMinute, PerClaim: DefaultMaxSignaturesPerClaim}}
}

// runSigner is the identity a run started now would sign as.
func (f *fixture) runSigner(t *testing.T) Identity {
	t.Helper()
	id, err := Resolver{Signers: f.stores.OrgCommitSigners}.Resolve(context.Background(), f.orgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !id.Mode.Signs() {
		t.Fatalf("the fixture's signer does not sign: %+v", id)
	}
	return id
}

func (f *fixture) localRequest(signer Identity, payload []byte) LocalRequest {
	return LocalRequest{
		OrgID: f.orgID, ConversationID: f.conversationID, ClaimID: f.claimID, BootEpoch: 1,
		Signer: signer, Payload: payload,
	}
}

func (f *fixture) signerPayload(signer Identity) []byte {
	return commitPayload(signer.Name + " <" + signer.Email + ">")
}

// signatureCount counts the org's commit_signatures rows.
func (f *fixture) signatureCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.conn.QueryRow(`SELECT COUNT(*) FROM commit_signatures`).Scan(&n); err != nil {
		t.Fatalf("count signatures: %v", err)
	}
	return n
}

func TestLocalSign_SignsAndRecords(t *testing.T) {
	reader := useTestInstruments(t)
	f := newFixture(t)
	signer := f.runSigner(t)
	payload := f.signerPayload(signer)

	res := f.localSigner().Sign(context.Background(), f.localRequest(signer, payload))
	if res.Outcome != OutcomeSigned {
		t.Fatalf("Sign = %+v, want signed", res)
	}
	want, err := gitsig.Sign(f.key, payload)
	if err != nil {
		t.Fatalf("gitsig.Sign: %v", err)
	}
	if res.Signature != string(want) {
		t.Errorf("signature is not the org key's:\n%s\nwant\n%s", res.Signature, want)
	}
	sha := gitsig.CommitID(gitsig.SignedCommit(payload, want))
	if res.CommitSHA != sha {
		t.Errorf("CommitSHA = %s, want %s", res.CommitSHA, sha)
	}

	row := f.row(t, res.SignatureID)
	if row.State != domain.CommitSignatureSigned || row.CommitSHA != sha || row.PublicKey != f.public ||
		row.Signature != string(want) || row.SignedAt == nil {
		t.Errorf("stored row = %+v", row)
	}
	if row.Signer != domain.CommitSigningOrg || row.SignerHost != "https://github.com" || row.SignerUserID != signerAccount {
		t.Errorf("stored signer = %s %s %d", row.Signer, row.SignerHost, row.SignerUserID)
	}
	if row.ConversationID != f.conversationID || row.ClaimID != f.claimID || row.BootEpoch != 1 {
		t.Errorf("stored engagement = %s %s %d", row.ConversationID, row.ClaimID, row.BootEpoch)
	}
	if row.Digest != gitsig.DigestOf(payload).String() {
		t.Errorf("stored digest = %s", row.Digest)
	}

	if got := signatureSeries(t, reader); got["signed/"] != 1 || len(got) != 1 {
		t.Errorf("tf_commit_signatures_total = %v, want one signed", got)
	}
	if modes := durationModes(t, reader); len(modes) != 1 || modes[0] != modeLocal {
		t.Errorf("duration observed under modes %v, want [%s]", modes, modeLocal)
	}
}

// A signature reads the signer's key each time, so a run that started before
// a rotation signs with the new key once verify promotes it.
func TestLocalSign_AcrossARotation(t *testing.T) {
	f := newFixture(t)
	signer := f.runSigner(t)
	payload := f.signerPayload(signer)

	before := f.localSigner().Sign(context.Background(), f.localRequest(signer, payload))
	if before.Outcome != OutcomeSigned {
		t.Fatalf("Sign before the rotation = %+v", before)
	}

	newKey, newPublic := newKey(t)
	f.secrets.set(integrations.KeyGitHubCommitSigningKeyPending, marshalKey(t, newKey))
	ctx := context.Background()
	if _, err := f.stores.OrgCommitSigners.StagePendingKey(ctx, f.orgID, newPublic); err != nil {
		t.Fatalf("StagePendingKey: %v", err)
	}
	mid := f.localSigner().Sign(ctx, f.localRequest(signer, payload))
	if mid.Outcome != OutcomeSigned || f.row(t, mid.SignatureID).PublicKey != f.public {
		t.Fatalf("a staged key signed before verify promoted it: %+v", mid)
	}
	if _, err := f.stores.OrgCommitSigners.Activate(ctx, f.orgID, newPublic); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	after := f.localSigner().Sign(ctx, f.localRequest(signer, payload))
	if after.Outcome != OutcomeSigned {
		t.Fatalf("Sign after the rotation = %+v", after)
	}
	want, _ := gitsig.Sign(newKey, payload)
	if after.Signature != string(want) || f.row(t, after.SignatureID).PublicKey != newPublic {
		t.Errorf("after the rotation the signature is not the new key's")
	}
}

func TestLocalSign_RefusesBeforeSigning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, f *fixture, req *LocalRequest)
		check   func(t *testing.T, f *fixture, res Result)
		want    Refusal
		metric  string
	}{
		{
			name: "a tag",
			arrange: func(_ *testing.T, _ *fixture, req *LocalRequest) {
				req.Payload = []byte("object " + testTree + "\ntype commit\ntag v1\ntagger " +
					req.Signer.Name + " <" + req.Signer.Email + "> 1700000000 +0000\n\nv1\n")
			},
			check: func(t *testing.T, _ *fixture, res Result) {
				if res.Object != gitsig.ObjectTag {
					t.Errorf("Object = %q, want tag", res.Object)
				}
			},
			want: RefusalNotACommit, metric: "not_a_commit",
		},
		{
			name: "another committer",
			arrange: func(_ *testing.T, _ *fixture, req *LocalRequest) {
				req.Payload = commitPayload("Mallory <mallory@example.com>")
			},
			check: func(t *testing.T, f *fixture, res Result) {
				if res.Committer != (gitsig.Identity{Name: "Mallory", Email: "mallory@example.com"}) {
					t.Errorf("Committer = %+v", res.Committer)
				}
				d := f.denials(t)
				if len(d) != 1 {
					t.Fatalf("commit_sign_denied rows = %d, want 1", len(d))
				}
				var detail map[string]string
				_ = json.Unmarshal([]byte(d[0].DetailJSON), &detail)
				if detail["reason"] != "committer_mismatch" || detail["committer"] != "Mallory <mallory@example.com>" {
					t.Errorf("denial detail = %s", d[0].DetailJSON)
				}
				if d[0].Target != "octocat" || d[0].ConversationID != f.conversationID || d[0].Credential != domain.CredentialNone {
					t.Errorf("denial = %+v", d[0])
				}
			},
			want: RefusalCommitterMismatch, metric: "committer_mismatch",
		},
		{
			name: "the signer's own name, another address",
			arrange: func(_ *testing.T, _ *fixture, req *LocalRequest) {
				req.Payload = commitPayload(req.Signer.Name + " <someone@example.com>")
			},
			want: RefusalCommitterMismatch, metric: "committer_mismatch",
		},
		{
			name: "an engagement that lost its claim",
			arrange: func(_ *testing.T, _ *fixture, req *LocalRequest) {
				req.BootEpoch = 2
			},
			check: func(t *testing.T, f *fixture, _ Result) {
				if d := f.denials(t); len(d) != 1 || !strings.Contains(d[0].DetailJSON, `"claim_inactive"`) {
					t.Errorf("commit_sign_denied rows = %+v, want one claim_inactive", d)
				}
			},
			want: RefusalClaimInactive, metric: "claim_inactive",
		},
		{
			name: "a removed signer",
			arrange: func(t *testing.T, f *fixture, _ *LocalRequest) {
				if err := f.stores.OrgCommitSigners.Delete(context.Background(), f.orgID); err != nil {
					t.Fatalf("Delete: %v", err)
				}
			},
			want: RefusalSignerRemoved, metric: "signer_inactive",
		},
		{
			name: "a different account",
			arrange: func(t *testing.T, f *fixture, _ *LocalRequest) {
				f.registerSigner(t, signerAccount+1, "https://github.com", f.public, true)
			},
			want: RefusalSignerChanged, metric: "signer_inactive",
		},
		{
			name: "a broken signer",
			arrange: func(t *testing.T, f *fixture, _ *LocalRequest) {
				if _, err := f.conn.Exec(`UPDATE org_commit_signers SET status = 'broken', status_reason = 'key_missing'`); err != nil {
					t.Fatalf("break the signer: %v", err)
				}
			},
			check: func(t *testing.T, _ *fixture, res Result) {
				if res.SignerStatus != domain.CommitSignerBroken || res.SignerReason != domain.CommitSignerReasonKeyMissing {
					t.Errorf("signer status = %s (%s)", res.SignerStatus, res.SignerReason)
				}
			},
			want: RefusalSignerInactive, metric: "signer_inactive",
		},
		{
			name: "a run started without a signer",
			arrange: func(_ *testing.T, _ *fixture, req *LocalRequest) {
				req.Signer = req.Signer.Unsigned()
			},
			want: RefusalNotSigning, metric: "signer_inactive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := useTestInstruments(t)
			f := newFixture(t)
			signer := f.runSigner(t)
			req := f.localRequest(signer, f.signerPayload(signer))
			tc.arrange(t, f, &req)

			res := f.localSigner().Sign(context.Background(), req)
			if res.Outcome != OutcomeRefused || res.Refusal != tc.want {
				t.Fatalf("Sign = %+v, want refused %s", res, tc.want)
			}
			if res.Signature != "" || res.SignatureID != "" {
				t.Errorf("a refusal carried a signature: %+v", res)
			}
			if n := f.signatureCount(t); n != 0 {
				t.Errorf("a refusal stored %d signature rows", n)
			}
			if tc.check != nil {
				tc.check(t, f, res)
			}
			if got := signatureSeries(t, reader); got["refused/"+tc.metric] != 1 || len(got) != 1 {
				t.Errorf("tf_commit_signatures_total = %v, want one refused/%s", got, tc.metric)
			}
		})
	}
}

func TestLocalSign_Budgets(t *testing.T) {
	t.Run("the claim's", func(t *testing.T) {
		f := newFixture(t)
		signer := f.runSigner(t)
		l := f.localSigner()
		l.Budgets.PerClaim = 2
		req := f.localRequest(signer, f.signerPayload(signer))
		for range 2 {
			if res := l.Sign(context.Background(), req); res.Outcome != OutcomeSigned {
				t.Fatalf("Sign within the budget = %+v", res)
			}
		}
		for range 2 {
			res := l.Sign(context.Background(), req)
			if res.Refusal != RefusalClaimBudget || res.Limit != 2 {
				t.Fatalf("Sign past the budget = %+v, want claim_budget with limit 2", res)
			}
		}
		if d := f.denials(t); len(d) != 1 || !strings.Contains(d[0].DetailJSON, `"budget_exhausted"`) {
			t.Errorf("commit_sign_denied rows = %+v, want one budget_exhausted for the conversation", d)
		}
	})
	t.Run("the org's per minute", func(t *testing.T) {
		f := newFixture(t)
		signer := f.runSigner(t)
		now := time.Now().UTC().Truncate(time.Second)
		l := f.localSigner()
		l.Budgets.PerOrgMinute = 2
		l.now = func() time.Time { return now }
		f.signedAt(t, now.Add(-50*time.Second))
		f.signedAt(t, now.Add(-10*time.Second))

		res := l.Sign(context.Background(), f.localRequest(signer, f.signerPayload(signer)))
		if res.Refusal != RefusalOrgMinuteBudget || res.Limit != 2 || res.RetryAfterSeconds != 10 {
			t.Fatalf("Sign = %+v, want org_minute_budget, limit 2, retry after 10s", res)
		}
		if d := f.denials(t); len(d) != 0 {
			t.Errorf("an org budget refusal wrote %d commit_sign_denied rows", len(d))
		}
		l.now = func() time.Time { return now.Add(11 * time.Second) }
		if res := l.Sign(context.Background(), f.localRequest(signer, f.signerPayload(signer))); res.Outcome != OutcomeSigned {
			t.Fatalf("Sign once the window passed = %+v", res)
		}
	})
}

// A signature that cannot be made, or cannot be recorded, is unanswered: the
// run tries again rather than getting a signature the budgets never counted.
func TestLocalSign_UnansweredWithoutTheKey(t *testing.T) {
	reader := useTestInstruments(t)
	f := newFixture(t)
	signer := f.runSigner(t)
	f.secrets.set(integrations.KeyGitHubCommitSigningKey, "")

	res := f.localSigner().Sign(context.Background(), f.localRequest(signer, f.signerPayload(signer)))
	if res.Outcome != OutcomeUnanswered || res.Signature != "" {
		t.Fatalf("Sign = %+v, want unanswered", res)
	}
	if n := f.signatureCount(t); n != 0 {
		t.Errorf("stored %d rows for a signature never made", n)
	}
	if got := signatureSeries(t, reader); got["unanswered/"] != 1 {
		t.Errorf("tf_commit_signatures_total = %v, want one unanswered", got)
	}
}

// signatureSeries reads tf_commit_signatures_total as "outcome/reason" →
// count.
func signatureSeries(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, m := range collect(t, reader) {
		if m.Name != "commit_signatures" {
			continue
		}
		for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
			outcome, _ := dp.Attributes.Value("outcome")
			reason, _ := dp.Attributes.Value("reason")
			out[outcome.AsString()+"/"+reason.AsString()] += dp.Value
		}
	}
	return out
}

// durationModes lists the mode label of every duration series.
func durationModes(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()
	var out []string
	for _, m := range collect(t, reader) {
		if m.Name != "commit_signing.duration" {
			continue
		}
		for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
			mode, _ := dp.Attributes.Value("mode")
			out = append(out, mode.AsString())
		}
	}
	return out
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}
