package agenthost

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner/gitsig"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// sidecarSigning is a multi-mode signature's whole path in one process: the
// sidecar's relay runtime, the orchestrator's relay server, the executor's
// requester, and a control plane that signs with the org's key from its
// secret store. Only the control plane holds the key.
type sidecarSigning struct {
	stores         db.Stores
	conn           *sql.DB
	key            ed25519.PrivateKey
	signer         commitsigner.Identity
	conversationID string
	claimID        string
	srv            *RelayServer
	rt             *relayRuntime
	cp             *commitsigner.ControlPlane
}

func newSidecarSigning(t *testing.T, budgets commitsigner.Budgets) *sidecarSigning {
	t.Helper()
	stores, conn := newTestDB(t)
	key, err := gitsig.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, _ := gitsig.PublicKey(key)
	private, _ := gitsig.MarshalPrivateKey(key)
	ctx := context.Background()
	if _, err := stores.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
		OrgID: runmode.LocalDefaultOrgID, Host: testScope("github"), GitHubUserID: 583231, Login: "octocat", PublicKey: public,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.OrgCommitSigners.Activate(ctx, runmode.LocalDefaultOrgID, public); err != nil {
		t.Fatal(err)
	}
	signer, err := commitsigner.Resolver{Signers: stores.OrgCommitSigners}.Resolve(ctx, runmode.LocalDefaultOrgID)
	if err != nil || !signer.Mode.Signs() {
		t.Fatalf("Resolve = %+v, %v; want the active signer", signer, err)
	}

	conversationID := uuid.New().String()
	dbtest.SeedConversation(t, conn, domain.Conversation{ID: conversationID})
	claimID := dbtest.SeedActiveClaim(t, conn, conversationID, "executor-1", 1)

	// The control plane's stores hold the key; the relay server's do not.
	cpStores := stores
	cpStores.Secrets = keySecrets{key: string(private)}
	doorbells := commitsigner.NewDoorbells()
	var cp *commitsigner.ControlPlane
	publish := func(_ context.Context, msg ctlbus.Message) error {
		switch msg.Kind {
		case commitsigner.KindSignRequest:
			go cp.Handle(context.Background(), msg.OrgID, msg.SignatureID)
		case commitsigner.KindSignDone:
			doorbells.Ring(msg.SignatureID)
		}
		return nil
	}
	cp = commitsigner.NewControlPlane(cpStores, budgets, "pod-a", publish)

	info := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: conversationID}
	srv := NewRelayServer(stores, info, nil)
	srv.SetSealedCommitSigner(signer.Mode, signer.AccountID)
	srv.SetCommitSigning(CommitSigning{ClaimID: claimID, BootEpoch: 1, Requester: &commitsigner.Requester{
		Signatures:   stores.CommitSignatures,
		Publish:      publish,
		Doorbells:    doorbells,
		PollInterval: 10 * time.Millisecond,
		Timeout:      10 * time.Second,
	}})
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)
	rt.signer = &signer
	return &sidecarSigning{
		stores: stores, conn: conn, key: key, signer: signer,
		conversationID: conversationID, claimID: claimID,
		srv: srv, rt: rt, cp: cp,
	}
}

// commitBy is a commit object whose committer is name <email>.
func commitBy(name, email, subject string) []byte {
	return []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"author someone <someone@example.com> 1700000000 +0000\n" +
		"committer " + name + " <" + email + "> 1700000000 +0000\n\n" + subject + "\n")
}

// signatureRows counts the commit_signatures rows, in any state.
func (f *sidecarSigning) signatureRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.conn.QueryRow(`SELECT count(*) FROM commit_signatures`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *sidecarSigning) denials(t *testing.T) []domain.ExternalAction {
	t.Helper()
	var out []domain.ExternalAction
	for _, a := range listExternalActions(t, f.stores) {
		if a.Action == domain.ActionCommitSignDenied {
			out = append(out, a)
		}
	}
	return out
}

// TestRelayRuntime_SignCommitSignsOnTheControlPlane: the sidecar's daemon
// checks the commit, relays its digest, and hands git the signature the
// control plane made with the org's key; the commit id that signature gives
// the commit reaches the signature's row.
func TestRelayRuntime_SignCommitSignsOnTheControlPlane(t *testing.T) {
	f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 600, PerClaim: 5000})
	payload := commitBy(f.signer.Name, f.signer.Email, "signed in a jail")

	res, err := f.rt.SignCommit(context.Background(), payload)
	if err != nil || res.Refusal != "" {
		t.Fatalf("SignCommit = %+v, %v; want a signature", res, err)
	}
	if want, _ := gitsig.Sign(f.key, payload); res.Signature != string(want) {
		t.Fatal("the signature is not the org key's signature of the commit")
	}

	sha := gitsig.CommitID(gitsig.SignedCommit(payload, []byte(res.Signature)))
	row, err := f.stores.CommitSignatures.ForCommitSystem(context.Background(), runmode.LocalDefaultOrgID, sha)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatalf("no signature row carries the commit id %s", sha)
	}
	if row.ConversationID != f.conversationID || row.ClaimID != f.claimID || row.State != domain.CommitSignatureSigned {
		t.Errorf("signature row = %+v, want this run's, signed", row)
	}
	if row.Digest != gitsig.DigestOf(payload).String() {
		t.Errorf("row digest = %s, want the commit's", row.Digest)
	}
}

// TestRelayRuntime_SignCommitRefusesBeforeAskingTheControlPlane: a commit whose
// committer is not the run's signer, or that is not a commit, is refused in
// the sidecar and never queued. The committer mismatch is audited as
// commit_sign_denied, by the orchestrator, naming the committer.
func TestRelayRuntime_SignCommitRefusesBeforeAskingTheControlPlane(t *testing.T) {
	f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 600, PerClaim: 5000})
	ctx := context.Background()

	res, err := f.rt.SignCommit(ctx, commitBy("mallory", "mallory@example.com", "as someone else"))
	if err != nil || res.Signature != "" {
		t.Fatalf("SignCommit = %+v, %v; want a refusal", res, err)
	}
	if want := "this run signs commits only as octocat <583231+octocat@users.noreply.github.com>, and this commit's committer is mallory <mallory@example.com>"; !strings.Contains(res.Refusal, want) {
		t.Errorf("refusal = %q, want it to contain %q", res.Refusal, want)
	}
	denials := f.denials(t)
	if len(denials) != 1 {
		t.Fatalf("commit_sign_denied rows = %d, want 1", len(denials))
	}
	var detail map[string]string
	if err := json.Unmarshal([]byte(denials[0].DetailJSON), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["reason"] != "committer_mismatch" || detail["committer"] != "mallory <mallory@example.com>" {
		t.Errorf("denial detail = %v, want the committer mismatch and its committer", detail)
	}
	if denials[0].ConversationID != f.conversationID || denials[0].Target != "octocat" || denials[0].Credential != domain.CredentialNone {
		t.Errorf("denial = %+v, want this run's, about the signer, under no credential", denials[0])
	}

	tag := []byte("object 4b825dc642cb6eb9a060e54bf8d69288fbee4904\ntype commit\ntag v1\ntagger octocat <x@example.com> 1700000000 +0000\n\nv1\n")
	res, err = f.rt.SignCommit(ctx, tag)
	if err != nil || !strings.Contains(res.Refusal, "this run signs commits only, and this is a tag") {
		t.Errorf("SignCommit(tag) = %+v, %v; want the not-a-commit line", res, err)
	}
	if n := len(f.denials(t)); n != 1 {
		t.Errorf("commit_sign_denied rows after a tag = %d, want still 1: a tag is not a security signal", n)
	}

	if n := f.signatureRows(t); n != 0 {
		t.Errorf("signature rows = %d, want none: neither refusal reaches the control plane", n)
	}
}

// TestRelayRuntime_SignCommitPrintsTheControlPlanesRefusals: a refusal the
// control plane makes reaches the agent as its §4.2 line, with the limit the
// control pod refused at and, for the per-minute budget, the wait.
func TestRelayRuntime_SignCommitPrintsTheControlPlanesRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("claim budget", func(t *testing.T) {
		f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 600, PerClaim: 1})
		if res, _ := f.rt.SignCommit(ctx, commitBy(f.signer.Name, f.signer.Email, "first")); res.Signature == "" {
			t.Fatalf("first signature refused: %s", res.Refusal)
		}
		res, err := f.rt.SignCommit(ctx, commitBy(f.signer.Name, f.signer.Email, "second"))
		if err != nil || !strings.Contains(res.Refusal, "this run has made 1 signed commits, the most one run may make") {
			t.Errorf("SignCommit = %+v, %v; want the per-claim line with its limit", res, err)
		}
	})

	t.Run("org minute budget", func(t *testing.T) {
		f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 1, PerClaim: 5000})
		if res, _ := f.rt.SignCommit(ctx, commitBy(f.signer.Name, f.signer.Email, "first")); res.Signature == "" {
			t.Fatalf("first signature refused: %s", res.Refusal)
		}
		res, err := f.rt.SignCommit(ctx, commitBy(f.signer.Name, f.signer.Email, "second"))
		if err != nil || !strings.Contains(res.Refusal, "this org has made 1 signed commits in the last minute, its limit, so signing resumes in ") {
			t.Errorf("SignCommit = %+v, %v; want the per-minute line with its limit and wait", res, err)
		}
	})

	// The executor cannot read the signer, so why it stopped signing reaches
	// the agent on the refusal the control pod stored.
	t.Run("signer broken", func(t *testing.T) {
		f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 600, PerClaim: 5000})
		if _, err := f.conn.Exec(`UPDATE org_commit_signers SET status = 'broken', status_reason = 'key_missing' WHERE org_id = ?`, runmode.LocalDefaultOrgID); err != nil {
			t.Fatal(err)
		}
		res, err := f.rt.SignCommit(ctx, commitBy(f.signer.Name, f.signer.Email, "after the key was removed"))
		if err != nil || !strings.Contains(res.Refusal, "commit signing is off for the rest of this run: the signer's key is no longer on its GitHub account.") {
			t.Errorf("SignCommit = %+v, %v; want the signing-off line naming the missing key", res, err)
		}
	})

	t.Run("signer removed", func(t *testing.T) {
		f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 600, PerClaim: 5000})
		if err := f.stores.OrgCommitSigners.Delete(ctx, runmode.LocalDefaultOrgID); err != nil {
			t.Fatal(err)
		}
		res, err := f.rt.SignCommit(ctx, commitBy(f.signer.Name, f.signer.Email, "after removal"))
		if err != nil || !strings.Contains(res.Refusal, "commit signing is off for the rest of this run: an org admin removed the signer.") {
			t.Errorf("SignCommit = %+v, %v; want the signing-off line", res, err)
		}
	})
}

// TestRelayRuntime_SignCommitWithoutASigner: a run whose bundle named no
// signer is told it does not sign, and nothing is relayed.
func TestRelayRuntime_SignCommitWithoutASigner(t *testing.T) {
	rt := newRelayRuntime(recordingRelayConn{t: t}, ConversationInfo{OrgID: "org", ConversationID: "conv"}, nil)
	res, err := rt.SignCommit(context.Background(), commitBy("octocat", "x@example.com", "unsigned"))
	if err != nil || !strings.Contains(res.Refusal, "this run does not sign commits") {
		t.Errorf("SignCommit = %+v, %v; want the not-signing line", res, err)
	}
}

// TestRelayServer_RecordCommitSHAStampsOnlyThisRunsSignature: a relayed commit
// id lands on a signature of the run's own, and a sidecar naming another
// run's signature changes nothing.
func TestRelayServer_RecordCommitSHAStampsOnlyThisRunsSignature(t *testing.T) {
	f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 600, PerClaim: 5000})
	ctx := context.Background()
	at := time.Now()
	insert := func(conversationID, claimID string) domain.CommitSignature {
		row, err := f.stores.CommitSignatures.InsertSystem(ctx, domain.CommitSignature{
			OrgID: runmode.LocalDefaultOrgID, ConversationID: conversationID, ClaimID: claimID, BootEpoch: 1,
			Signer: domain.CommitSigningOrg, SignerHost: testScope("github"), SignerUserID: 583231,
			PublicKey: f.signer.PublicKey, Digest: gitsig.DigestOf([]byte(uuid.NewString())).String(),
			State: domain.CommitSignatureSigned, Signature: "sig", SignedAt: &at,
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	own := insert(f.conversationID, f.claimID)
	otherConversation := uuid.New().String()
	dbtest.SeedConversation(t, f.conn, domain.Conversation{ID: otherConversation})
	other := insert(otherConversation, dbtest.SeedActiveClaim(t, f.conn, otherConversation, "executor-1", 1))

	record := func(id, sha string) {
		raw, _ := json.Marshal(agentproc.RecordCommitSHAArgs{SignatureID: id, CommitSHA: sha})
		if err := f.srv.DispatchRecord(ctx, "core", agentproc.OpRecordCommitSHA, uuid.NewString(), raw); err != nil {
			t.Fatalf("DispatchRecord: %v", err)
		}
	}
	sha := strings.Repeat("ab", 20)
	record(own.ID, sha)
	record(own.ID, sha) // a resend writes nothing new
	if got, _ := f.stores.CommitSignatures.GetSystem(ctx, runmode.LocalDefaultOrgID, own.ID); got == nil || got.CommitSHA != sha {
		t.Errorf("own signature = %+v, want commit %s", got, sha)
	}

	record(other.ID, sha)
	if got, _ := f.stores.CommitSignatures.GetSystem(ctx, runmode.LocalDefaultOrgID, other.ID); got == nil || got.CommitSHA != "" {
		t.Errorf("another run's signature = %+v, want no commit id: a sidecar stamps only its own run's", got)
	}
}

// TestRelayServer_RecordSignRefusalNamesTheBringUpSigner: a sidecar's signing
// refusal is recorded against the signer it reported at bring-up, before the
// agent ran, and a refusal from a run that reported none writes nothing.
func TestRelayServer_RecordSignRefusalNamesTheBringUpSigner(t *testing.T) {
	f := newSidecarSigning(t, commitsigner.Budgets{PerOrgMinute: 600, PerClaim: 5000})
	ctx := context.Background()
	record := func() {
		raw, _ := json.Marshal(agentproc.RecordSignRefusalArgs{
			Reason: string(commitsigner.RefusalCommitterMismatch), CommitterName: "mallory", CommitterEmail: "mallory@example.com",
		})
		if err := f.srv.DispatchRecord(ctx, agentproc.RelayNamespaceCore, agentproc.OpRecordSignRefusal, uuid.NewString(), raw); err != nil {
			t.Fatalf("DispatchRecord: %v", err)
		}
	}

	f.srv.SetSealedCommitSigner("", "")
	record()
	if n := len(f.denials(t)); n != 0 {
		t.Fatalf("commit_sign_denied rows from a run sealed no signer = %d, want none", n)
	}

	f.srv.SetSealedCommitSigner(domain.CommitSigningOrg, "583231")
	record()
	denials := f.denials(t)
	if len(denials) != 1 || denials[0].ExternalID != "583231" || denials[0].Target != "octocat" {
		t.Fatalf("commit_sign_denied rows = %+v, want one about the bring-up signer's account", denials)
	}
}
