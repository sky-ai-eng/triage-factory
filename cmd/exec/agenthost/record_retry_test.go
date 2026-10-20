package agenthost

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// relayRecord applies one relayed record the way the supervisor hands it over,
// under a fresh key, and returns the answer the sidecar would get.
func relayRecord(t *testing.T, s *RelayServer, namespace, op string, args json.RawMessage) error {
	t.Helper()
	return s.DispatchRecord(context.Background(), namespace, op, uuid.NewString(), args)
}

// fastRecordRetry shrinks the write stage's backoff for one test, so a test
// that exhausts the retries takes milliseconds.
func fastRecordRetry(t *testing.T) {
	t.Helper()
	prev := recordRetry
	recordRetry = recordRetryPolicy{attempts: prev.attempts, initial: time.Millisecond, max: 2 * time.Millisecond}
	t.Cleanup(func() { recordRetry = prev })
}

// errTransient is a failure IsRetryable accepts: the connection was lost.
var errTransient = driver.ErrBadConn

// faults decides, write by write, what a flaky store does. Shared by the
// wrappers below so one counter covers every write path a test takes.
type faults struct {
	mu sync.Mutex
	// fail is how many writes, from the next one, fail before reaching the
	// database. Negative fails every write.
	fail int
	// lostAcks is how many writes, from the next one, land in the database
	// and then report a lost connection anyway — a commit whose
	// acknowledgement never arrived.
	lostAcks int
	// err is what a failing write returns; errTransient when nil.
	err error
	// attempts counts every write the wrappers saw.
	attempts int
}

// next reports the fault for one write: whether to skip the database, and
// the error to return after it (nil for a clean write).
func (f *faults) next() (skip bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	failErr := f.err
	if failErr == nil {
		failErr = errTransient
	}
	switch {
	case f.fail != 0:
		if f.fail > 0 {
			f.fail--
		}
		return true, failErr
	case f.lostAcks > 0:
		f.lostAcks--
		return false, errTransient
	}
	return false, nil
}

func (f *faults) set(fail, lostAcks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail, f.lostAcks = fail, lostAcks
}

func (f *faults) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// flakyActions is an ExternalActionStore whose appends go through faults.
type flakyActions struct {
	db.ExternalActionStore
	f *faults
}

func (s flakyActions) Record(ctx context.Context, orgID string, e domain.ExternalAction) error {
	return s.write(func() error { return s.ExternalActionStore.Record(ctx, orgID, e) })
}

func (s flakyActions) RecordSystem(ctx context.Context, orgID string, e domain.ExternalAction) error {
	return s.write(func() error { return s.ExternalActionStore.RecordSystem(ctx, orgID, e) })
}

func (s flakyActions) write(do func() error) error {
	skip, err := s.f.next()
	if skip {
		return err
	}
	if derr := do(); derr != nil {
		return derr
	}
	return err
}

// flakyArtifacts is an ArtifactStore whose admin-pool upserts go through
// faults.
type flakyArtifacts struct {
	db.ArtifactStore
	f *faults
}

func (s flakyArtifacts) UpsertSystem(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error) {
	if skip, err := s.f.next(); skip {
		return domain.Artifact{}, err
	}
	return s.ArtifactStore.UpsertSystem(ctx, orgID, a)
}

// flakyTx is a TxRunner whose claims transactions see the flaky ledger store,
// and whose commits can lose their acknowledgement: a transaction that
// committed and reports a lost connection anyway.
type flakyTx struct {
	db.TxRunner
	actions  *faults
	lostAcks *faults
}

func (r flakyTx) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	err := r.TxRunner.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		ts.ExternalActions = flakyActions{ExternalActionStore: ts.ExternalActions, f: r.actions}
		return fn(ts)
	})
	if err != nil {
		return err
	}
	if _, lost := r.lostAcks.next(); lost != nil {
		return lost
	}
	return nil
}

// flakyLedger wires stores so every ledger append — the admin-pool one of an
// event-triggered run and the claims-transaction one of a manual run — goes
// through actions, and a manual run's commit through commits.
func flakyLedger(stores db.Stores, actions, commits *faults) db.Stores {
	stores.ExternalActions = flakyActions{ExternalActionStore: stores.ExternalActions, f: actions}
	stores.Tx = flakyTx{TxRunner: stores.Tx, actions: actions, lostAcks: commits}
	return stores
}

func ghWriteAction() *domain.ExternalAction {
	return &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
		Action:     domain.ActionGHChannelWrite,
		Target:     "acme/widgets",
		Credential: domain.CredentialGitHubApp,
	}
}

func forEachTrigger(t *testing.T, run func(t *testing.T, eventTriggered bool)) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) { run(t, eventTriggered) })
	}
}

// TestRecordExternalWrite_TransientFailureKeepsTheRow is the write-stage
// acceptance: the first writes fail on a lost connection, and the row still
// lands, exactly once, with nothing counted lost.
func TestRecordExternalWrite_TransientFailureKeepsTheRow(t *testing.T) {
	forEachTrigger(t, func(t *testing.T, eventTriggered bool) {
		fastRecordRetry(t)
		reader := installTestAuditDrops(t)
		base, info := newCaptureStores(t, eventTriggered)
		actions := &faults{fail: 3}
		stores := flakyLedger(base, actions, &faults{})

		RecordExternalWrite(context.Background(), stores, info, nil, ghWriteAction())

		if acts := listExternalActions(t, base); len(acts) != 1 {
			t.Fatalf("ledger rows = %d; want exactly one after three failed writes", len(acts))
		}
		if n := actions.count(); n != 4 {
			t.Errorf("write attempts = %d; want 4 (three failures, then the row)", n)
		}
		if got := collectDrops(t, reader); len(got) != 0 {
			t.Fatalf("drops = %v; want none: the row landed", got)
		}
	})
}

// TestRecordExternalWrite_LostAcknowledgementWritesOneRow: a write that landed
// but whose acknowledgement was lost is retried, and the retry conflicts with
// the row it already wrote instead of appending a second one — the dedup key
// was fixed before the first attempt.
func TestRecordExternalWrite_LostAcknowledgementWritesOneRow(t *testing.T) {
	forEachTrigger(t, func(t *testing.T, eventTriggered bool) {
		fastRecordRetry(t)
		base, info := newCaptureStores(t, eventTriggered)
		actions, commits := &faults{}, &faults{}
		if eventTriggered {
			actions.lostAcks = 1
		} else {
			commits.lostAcks = 1
		}
		stores := flakyLedger(base, actions, commits)

		RecordExternalWrite(context.Background(), stores, info, nil, ghWriteAction())

		if acts := listExternalActions(t, base); len(acts) != 1 {
			t.Fatalf("ledger rows = %d; want one: the retry must land on the row the lost acknowledgement already wrote", len(acts))
		}
		if n := actions.count(); n != 2 {
			t.Errorf("write attempts = %d; want 2 (the landed write, then its retry)", n)
		}
	})
}

// TestRecordExternalWrite_ExhaustedRetriesAreCounted: a failure that outlasts
// the retries is still counted at the write stage, once.
func TestRecordExternalWrite_ExhaustedRetriesAreCounted(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	actions := &faults{fail: -1}
	stores := flakyLedger(base, actions, &faults{})

	RecordExternalWrite(context.Background(), stores, info, nil, ghWriteAction())

	if n := actions.count(); n != recordRetry.attempts {
		t.Errorf("write attempts = %d; want the policy's %d", n, recordRetry.attempts)
	}
	if n := collectDrops(t, reader)[dropStageRecord][domain.ActionGHChannelWrite]; n != 1 {
		t.Fatalf("want one %s drop once the retries ran out", dropStageRecord)
	}
}

// TestRecordExternalWrite_PermanentFailureIsNotRetried: a failure another
// attempt cannot get past is counted at once, without retrying.
func TestRecordExternalWrite_PermanentFailureIsNotRetried(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	actions := &faults{fail: -1, err: errors.New("new row violates row-level security policy")}
	stores := flakyLedger(base, actions, &faults{})

	RecordExternalWrite(context.Background(), stores, info, nil, ghWriteAction())

	if n := actions.count(); n != 1 {
		t.Errorf("write attempts = %d; want 1: the failure is not retryable", n)
	}
	if n := collectDrops(t, reader)[dropStageRecord][domain.ActionGHChannelWrite]; n != 1 {
		t.Fatalf("want one %s drop", dropStageRecord)
	}
}

// TestRecordExternalWrite_OutlivesItsCaller: the funnel writes on a context of
// its own, so a caller whose context is already cancelled — a run that ended
// while the write was on its way — does not cost the row.
func TestRecordExternalWrite_OutlivesItsCaller(t *testing.T) {
	forEachTrigger(t, func(t *testing.T, eventTriggered bool) {
		stores, info := newCaptureStores(t, eventTriggered)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		RecordExternalWrite(ctx, stores, info, nil, ghWriteAction())

		if acts := listExternalActions(t, stores); len(acts) != 1 {
			t.Fatalf("ledger rows = %d; want the row written despite the cancelled caller", len(acts))
		}
	})
}

// TestRelayServer_TransientFailureDefersTheRecord: a relayed record whose write
// fails past the write stage's retries is not counted lost — it is answered
// "not applied" so the sidecar sends it again — and the resend, under the same
// key, lands it once the database is back.
func TestRelayServer_TransientFailureDefersTheRecord(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	actions := &faults{fail: -1}
	s := NewRelayServer(flakyLedger(base, actions, &faults{}), info, nil)

	key := uuid.NewString()
	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{Method: "PUT", Path: "/repos/acme/widgets/pulls/7/merge", Status: 404})
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, key, args); !errors.Is(err, errRecordDeferred) {
		t.Fatalf("DispatchRecord during the outage = %v; want %v", err, errRecordDeferred)
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; a deferred record is not lost, and must not be counted", got)
	}

	actions.set(0, 0)
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, key, args); err != nil {
		t.Fatalf("DispatchRecord after the outage = %v", err)
	}
	if acts := listExternalActions(t, base); len(acts) != 1 {
		t.Fatalf("ledger rows = %d; want one", len(acts))
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none", got)
	}
}

// TestRelayServer_ResentRecordWritesOneRow: a record whose first application
// landed its row but never learned so — every attempt lost its acknowledgement
// — is sent again by the sidecar, and the second application lands on the
// same row. The ledger key comes from the record's key, which is the same on
// every resend, not from anything minted per application.
func TestRelayServer_ResentRecordWritesOneRow(t *testing.T) {
	fastRecordRetry(t)
	base, info := newCaptureStores(t, true)
	actions := &faults{lostAcks: recordRetry.attempts}
	s := NewRelayServer(flakyLedger(base, actions, &faults{}), info, nil)

	key := uuid.NewString()
	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{Method: "PUT", Path: "/repos/acme/widgets/pulls/7/merge", Status: 404})
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, key, args); !errors.Is(err, errRecordDeferred) {
		t.Fatalf("first application = %v; want %v", err, errRecordDeferred)
	}
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, key, args); err != nil {
		t.Fatalf("resend = %v", err)
	}
	acts := listExternalActions(t, base)
	if len(acts) != 1 {
		t.Fatalf("ledger rows = %d; want one across both applications", len(acts))
	}
	if want := domain.RelayedRecordDedupKey(info.ConversationID, key, 0); acts[0].DedupKey != want {
		t.Errorf("dedup key = %q; want %q", acts[0].DedupKey, want)
	}
}

// TestRelayedRecords_RetriedRecordStaysAheadOfLaterOnes runs the whole relayed
// path — the sidecar's record sender, the supervision channel, the
// orchestrator's record lane and the write funnel — through an outage that
// outlasts the write stage's retries. A comment is posted and then deleted.
// The post's artifact cannot be written, so its record is deferred and resent;
// the delete, sent after it, must not be applied until the post has been. Were
// it applied first it would find no comment row and write nothing, and the
// post would then create the row as posted.
func TestRelayedRecords_RetriedRecordStaysAheadOfLaterOnes(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	artifactFaults := &faults{fail: recordRetry.attempts + 1}
	stores := base
	stores.Artifacts = flakyArtifacts{ArtifactStore: base.Artifacts, f: artifactFaults}
	srv := NewRelayServer(stores, info, nil)

	orchEnd, sidecarEnd := net.Pipe()
	orchConn := sidecarproto.New(orchEnd, sidecarproto.HandlerFunc(func(ctx context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
		if kind != sidecarproto.KindRelayRecord {
			return nil, nil
		}
		var env sidecarproto.RelayRecordBody
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		return nil, srv.DispatchRecord(ctx, env.Namespace, env.Op, env.Key, env.Args)
	}))
	sidecarConn := sidecarproto.New(sidecarEnd, nil)
	t.Cleanup(func() {
		_ = orchConn.Close()
		_ = sidecarConn.Close()
		_ = orchEnd.Close()
		_ = sidecarEnd.Close()
	})
	rt := NewRelayRuntime(sidecarConn, agentproc.NewRecordSender(sidecarConn), info, nil)

	key := domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 7)
	rt.Record(context.Background(),
		&domain.Artifact{
			Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			Target: "octo/repo#1", ExternalID: "7", State: domain.ArtifactStateCommentPosted, DedupKey: key,
		},
		&domain.ExternalAction{
			Provider: domain.ArtifactProviderGitHub, Action: domain.ActionCommentPosted,
			Target: "octo/repo#1", ExternalID: "7", Credential: domain.CredentialGitHubApp,
		}, "")
	rt.Record(context.Background(),
		&domain.Artifact{
			Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			ExternalID: "7", State: domain.ArtifactStateCommentDeleted, DedupKey: key,
		},
		&domain.ExternalAction{
			Provider: domain.ArtifactProviderGitHub, Action: domain.ActionCommentDeleted,
			Target: "octo/repo", ExternalID: "7", Credential: domain.CredentialGitHubApp,
		}, "")

	deadline := time.Now().Add(10 * time.Second)
	for {
		arts := listConversationArtifacts(t, base, info.ConversationID)
		acts := listExternalActions(t, base)
		if len(arts) == 1 && arts[0].State == domain.ArtifactStateCommentDeleted && len(acts) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("artifacts = %+v, ledger rows = %d; want the comment deleted and both rows written", arts, len(acts))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none: the outage ended and every record landed", got)
	}
}

// pushRelayServer is a RelayServer whose git gate records a relayed push the
// way the executor's does: through UpsertArtifact, as a branch artifact with
// its ledger row composed in.
func pushRelayServer(stores db.Stores, info ConversationInfo) *RelayServer {
	host := NewLocal(stores, info)
	git := &agentproc.GitProxyConfig{
		RecordPush: func(ctx context.Context, push gitproxy.PushedRef) {
			art, ok := domain.NewBranchArtifact(push.Repo, push.Ref, push.NewSHA, push.Created)
			if !ok {
				return
			}
			_, _ = host.UpsertArtifact(ctx, art)
		},
	}
	return NewRelayServer(stores, info, git)
}

func pushArgs(t *testing.T) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(agentproc.RecordPushArgs{Repo: "octo/repo", Ref: "refs/heads/feat", NewSHA: "abc123", Created: true, Status: 200})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	return args
}

// TestRelayedPush_TransientArtifactFailureDefersTheRecord: a relayed push
// whose branch artifact cannot be written past the write stage's retries is
// deferred like any other relayed record, so the sidecar's resend retries the
// artifact — rather than the record being acknowledged with only its ledger
// row written. Nothing is counted lost, and the resend lands the artifact
// beside the one ledger row.
func TestRelayedPush_TransientArtifactFailureDefersTheRecord(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	artifactFaults := &faults{fail: -1}
	stores := base
	stores.Artifacts = flakyArtifacts{ArtifactStore: base.Artifacts, f: artifactFaults}
	s := pushRelayServer(stores, info)

	key := uuid.NewString()
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordPush, key, pushArgs(t)); !errors.Is(err, errRecordDeferred) {
		t.Fatalf("push with an unwritable artifact = %v; want %v", err, errRecordDeferred)
	}
	if arts := listConversationArtifacts(t, base, info.ConversationID); len(arts) != 0 {
		t.Fatalf("artifacts = %+v; want none during the outage", arts)
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; a deferred record is not lost, and must not be counted", got)
	}

	artifactFaults.set(0, 0)
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordPush, key, pushArgs(t)); err != nil {
		t.Fatalf("resend = %v", err)
	}
	arts := listConversationArtifacts(t, base, info.ConversationID)
	if len(arts) != 1 || arts[0].Kind != domain.ArtifactKindBranch {
		t.Fatalf("artifacts = %+v; want the branch artifact landed by the resend", arts)
	}
	if acts := listExternalActions(t, base); len(acts) != 1 || acts[0].Action != domain.ActionBranchPushed {
		t.Fatalf("ledger rows = %+v; want one branch_pushed row across both applications", acts)
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none", got)
	}
}

// TestRelayedPush_RefusedArtifactIsCountedOnce: an artifact refused for good
// is not resent. The record is acknowledged with its ledger row written, and
// the artifact is counted lost once, at the write stage, under its kind.
func TestRelayedPush_RefusedArtifactIsCountedOnce(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	refuseArtifactWrites(t, conn)
	s := pushRelayServer(stores, info)

	if err := relayRecord(t, s, agentproc.RelayNamespaceCore, agentproc.OpRecordPush, pushArgs(t)); err != nil {
		t.Fatalf("push with a refused artifact = %v; want it acknowledged", err)
	}
	if acts := listExternalActions(t, stores); len(acts) != 1 || acts[0].Action != domain.ActionBranchPushed {
		t.Fatalf("ledger rows = %+v; want the branch_pushed row despite the refused artifact", acts)
	}
	got := collectDrops(t, reader)
	if len(got[dropStageRecord]) != 1 || got[dropStageRecord][domain.ArtifactKindBranch] != 1 {
		t.Fatalf("drops = %v; want exactly one %s drop under op=%s", got, dropStageRecord, domain.ArtifactKindBranch)
	}
}

// TestUpsertArtifact_DirectFailureIsTheCallersToHandle: outside a relayed
// record, a failed artifact is returned to the caller waiting on it and is not
// counted here — the caller decides what the failure means.
func TestUpsertArtifact_DirectFailureIsTheCallersToHandle(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	refuseArtifactWrites(t, conn)

	branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact refused the fixture branch")
	}
	if _, err := newDirectRuntime(stores, info).UpsertArtifact(context.Background(), branch); err == nil {
		t.Fatal("UpsertArtifact = nil error, want the refused artifact's error")
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none: the caller got the error", got)
	}
}

// TestRelayServer_WireDedupKeyIsNotTrusted: a relayed external write's ledger
// key is derived on this side, whatever key the sidecar put on the action. A
// key the sidecar chose could equal another run's deterministic one — here an
// egress denial's — and suppress that run's real row.
func TestRelayServer_WireDedupKeyIsNotTrusted(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	forged := domain.EgressDenialDedupKey("another-conversation", "api.github.com:443")
	args, _ := json.Marshal(recordExternalWriteArgs{Action: &domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub, Action: domain.ActionGHChannelWrite,
		Target: "acme/widgets", Credential: domain.CredentialGitHubApp, DedupKey: forged,
	}})
	key := uuid.NewString()
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, opRecordExternalWrite, key, args); err != nil {
		t.Fatalf("DispatchRecord: %v", err)
	}
	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("ledger rows = %d; want 1", len(acts))
	}
	if want := domain.RelayedRecordDedupKey(info.ConversationID, key, 0); acts[0].DedupKey != want {
		t.Fatalf("dedup key = %q; want the derived %q, not the sidecar's %q", acts[0].DedupKey, want, forged)
	}
}

// TestRelayServer_EveryRecordApplicationHasADeadline: the record lane serves
// one record at a time, so every application runs under a deadline — the ops
// that set no timeout of their own included, like a git denial or a provider's
// record.
func TestRelayServer_EveryRecordApplicationHasADeadline(t *testing.T) {
	t.Cleanup(ResetProviders)
	deadlines := make(chan bool, 2)
	RegisterProviderOp("acmeprov", "record_thing",
		func(ctx context.Context, _ db.Stores, _ ConversationInfo, _ json.RawMessage) (any, error) {
			_, ok := ctx.Deadline()
			deadlines <- ok
			return nil, nil
		})
	git := &agentproc.GitProxyConfig{
		RecordDenial: func(ctx context.Context, _ gitproxy.DeniedGitOp) {
			_, ok := ctx.Deadline()
			deadlines <- ok
		},
	}
	s := NewRelayServer(db.Stores{}, ConversationInfo{}, git)

	denial, _ := json.Marshal(agentproc.RecordDenialArgs{Owner: "acme", Repo: "widgets", Op: "push", Reason: "off-ref"})
	if err := relayRecord(t, s, agentproc.RelayNamespaceCore, agentproc.OpRecordDenial, denial); err != nil {
		t.Fatalf("denial: %v", err)
	}
	if err := relayRecord(t, s, "acmeprov", "record_thing", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("provider record: %v", err)
	}
	for _, name := range []string{"git denial", "provider record"} {
		if !<-deadlines {
			t.Errorf("%s ran with no deadline", name)
		}
	}
}
