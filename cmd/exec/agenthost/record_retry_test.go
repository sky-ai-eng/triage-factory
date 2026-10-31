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
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
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

// flakyDeliverables is a DeliverableStore whose admin-pool upserts go through
// faults.
type flakyDeliverables struct {
	db.DeliverableStore
	f *faults
}

func (s flakyDeliverables) UpsertSystem(ctx context.Context, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
	if skip, err := s.f.next(); skip {
		return domain.Deliverable{}, err
	}
	return s.DeliverableStore.UpsertSystem(ctx, orgID, a)
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
		Provider:   domain.ProviderGitHub,
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

// flakyEntities is an EntityStore whose identity lookups go through faults:
// GetByExternalIDSystem is the first read a deliverable carrying a provider id
// resolves its entity through.
type flakyEntities struct {
	db.EntityStore
	f *faults
}

func (s flakyEntities) GetByExternalIDSystem(ctx context.Context, orgID, source, scope, externalID string) (*domain.Entity, error) {
	if skip, err := s.f.next(); skip {
		return nil, err
	}
	return s.EntityStore.GetByExternalIDSystem(ctx, orgID, source, scope, externalID)
}

// TestRecordExternalWrite_TransientEntityFailureStillLinks: resolving the
// entity a deliverable is about is retried like the write after it, so a lost
// connection on the first lookups still records the deliverable linked to its
// pull request's entity.
func TestRecordExternalWrite_TransientEntityFailureStillLinks(t *testing.T) {
	forEachTrigger(t, func(t *testing.T, eventTriggered bool) {
		fastRecordRetry(t)
		base, info := newCaptureStores(t, eventTriggered)
		lookups := &faults{fail: 2}
		stores := base
		stores.Entities = flakyEntities{EntityStore: base.Entities, f: lookups}

		pr := domain.NewPullRequestDeliverable("acme/widgets", 7, "PR_kw7", "feat", "main", "https://github.com/acme/widgets/pull/7", "t", "b", true)
		recordExternalWrite(context.Background(), stores, info, &pr, nil, domain.EntityContainer{},
			domain.GitHubIdentity{RepositoryID: "1", Repository: "acme/widgets"})

		arts := listConversationDeliverables(t, base, info.ConversationID)
		entity, err := base.Entities.GetByExternalIDSystem(context.Background(), info.OrgID, domain.ProviderGitHub, testScope("github"), "PR_kw7")
		if err != nil || entity == nil {
			t.Fatalf("PR entity = %+v, %v; want one created for PR_kw7", entity, err)
		}
		if len(arts) != 1 || arts[0].EntityID != entity.ID {
			t.Fatalf("deliverables = %+v; want one, linked to %s", arts, entity.ID)
		}
		if n := lookups.count(); n < 3 {
			t.Errorf("entity lookups = %d; want the two failures retried", n)
		}
	})
}

// TestRecordExternalWrite_PermanentEntityFailureRecordsUnlinked: a resolve
// failure another attempt cannot get past is tried once, and the deliverable is
// still recorded, unlinked.
func TestRecordExternalWrite_PermanentEntityFailureRecordsUnlinked(t *testing.T) {
	fastRecordRetry(t)
	base, info := newCaptureStores(t, true)
	lookups := &faults{fail: -1, err: errors.New("permission denied for table entities")}
	stores := base
	stores.Entities = flakyEntities{EntityStore: base.Entities, f: lookups}

	pr := domain.NewPullRequestDeliverable("acme/widgets", 7, "PR_kw7", "feat", "main", "https://github.com/acme/widgets/pull/7", "t", "b", true)
	RecordExternalWrite(context.Background(), stores, info, &pr, nil)

	if n := lookups.count(); n != 1 {
		t.Errorf("entity lookups = %d; want 1: the failure is not retryable", n)
	}
	arts := listConversationDeliverables(t, base, info.ConversationID)
	if len(arts) != 1 || arts[0].EntityID != "" {
		t.Fatalf("deliverables = %+v; want one, unlinked", arts)
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
// outlasts the write stage's retries. A draft pull request is recorded and
// then marked ready. The create's deliverable cannot be written, so its record
// is deferred and resent; the ready mark, sent after it, must not be applied
// until the create has been. Were it applied first, the create would then
// write the row back as a draft.
func TestRelayedRecords_RetriedRecordStaysAheadOfLaterOnes(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	deliverableFaults := &faults{fail: recordRetry.attempts + 1}
	stores := base
	stores.Deliverables = flakyDeliverables{DeliverableStore: base.Deliverables, f: deliverableFaults}
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
	rt := NewRelayRuntime(sidecarConn, agentproc.NewRecordSender(sidecarConn), info, nil, nil)

	key := domain.PullRequestDedupKey("PR_1")
	rt.Record(context.Background(),
		&domain.Deliverable{
			Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindPullRequest,
			Target: "octo/repo#1", ExternalID: "1", State: domain.DeliverableStatePRDraft, DedupKey: key,
		},
		&domain.ExternalAction{
			Provider: domain.ProviderGitHub, Action: domain.ActionPRCreated,
			Target: "octo/repo#1", ExternalID: "1", Credential: domain.CredentialGitHubApp,
		}, domain.EntityContainer{}, domain.GitHubIdentity{})
	rt.Record(context.Background(),
		&domain.Deliverable{
			Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindPullRequest,
			Target: "octo/repo#1", ExternalID: "1", State: domain.DeliverableStatePROpen, DedupKey: key,
		},
		&domain.ExternalAction{
			Provider: domain.ProviderGitHub, Action: domain.ActionPRMarkedReady,
			Target: "octo/repo#1", ExternalID: "1", Credential: domain.CredentialGitHubApp,
		}, domain.EntityContainer{}, domain.GitHubIdentity{})

	deadline := time.Now().Add(10 * time.Second)
	for {
		arts := listConversationDeliverables(t, base, info.ConversationID)
		acts := listExternalActions(t, base)
		if len(arts) == 1 && arts[0].State == domain.DeliverableStatePROpen && len(acts) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliverables = %+v, ledger rows = %d; want the pull request open and both rows written", arts, len(acts))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none: the outage ended and every record landed", got)
	}
}

// pushRelayServer is a RelayServer whose git gate records a relayed push the
// way the executor's does: through RecordBranchPushFor, with the repository id
// the push relays. The pushed repository, octo/repo, has its registry row, as a
// run's checkout does.
func pushRelayServer(t *testing.T, stores db.Stores, info ConversationInfo) *RelayServer {
	t.Helper()
	registerRepo(t, stores, "octo", "repo", "1")
	host := NewLocal(stores, info)
	git := &agentproc.GitProxyConfig{
		RecordPush: func(ctx context.Context, push gitproxy.PushedRef) {
			host.RecordBranchPushFor(ctx, push.Repo, push.Ref, push.NewSHA, push.Created, PushIdentity(push))
		},
	}
	return NewRelayServer(stores, info, git)
}

func pushArgs(t *testing.T) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(agentproc.RecordPushArgs{Repo: "octo/repo", Ref: "refs/heads/feat", NewSHA: "abc123", Created: true, Status: 200, RepositoryID: "1"})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	return args
}

// TestRelayedPush_TransientLedgerFailureDefersTheRecord: a relayed push whose
// branch_pushed row cannot be written past the write stage's retries is
// deferred like any other relayed record, so the sidecar's resend retries it.
// Nothing is counted lost, and the resend lands the one row.
func TestRelayedPush_TransientLedgerFailureDefersTheRecord(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	actionFaults := &faults{fail: -1}
	stores := base
	stores.ExternalActions = flakyActions{ExternalActionStore: base.ExternalActions, f: actionFaults}
	s := pushRelayServer(t, stores, info)

	key := uuid.NewString()
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordPush, key, pushArgs(t)); !errors.Is(err, errRecordDeferred) {
		t.Fatalf("push with an unwritable ledger row = %v; want %v", err, errRecordDeferred)
	}
	if acts := listExternalActions(t, base); len(acts) != 0 {
		t.Fatalf("ledger rows = %+v; want none during the outage", acts)
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; a deferred record is not lost, and must not be counted", got)
	}

	actionFaults.set(0, 0)
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordPush, key, pushArgs(t)); err != nil {
		t.Fatalf("resend = %v", err)
	}
	if acts := listExternalActions(t, base); len(acts) != 1 || acts[0].Action != domain.ActionBranchPushed {
		t.Fatalf("ledger rows = %+v; want the one branch_pushed row landed by the resend", acts)
	}
	if arts := listConversationDeliverables(t, base, info.ConversationID); len(arts) != 0 {
		t.Fatalf("deliverables = %+v; a push writes none", arts)
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none", got)
	}
}

// TestRelayedPush_RefusedLedgerRowIsCountedOnce: a ledger row refused for good
// is not resent. The record is acknowledged, and the row is counted lost once,
// at the write stage, under its action.
func TestRelayedPush_RefusedLedgerRowIsCountedOnce(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	base, info := newCaptureStores(t, true)
	stores := base
	stores.ExternalActions = flakyActions{ExternalActionStore: base.ExternalActions, f: &faults{fail: -1, err: errors.New("ledger write refused")}}
	s := pushRelayServer(t, stores, info)

	if err := relayRecord(t, s, agentproc.RelayNamespaceCore, agentproc.OpRecordPush, pushArgs(t)); err != nil {
		t.Fatalf("push with a refused ledger row = %v; want it acknowledged", err)
	}
	got := collectDrops(t, reader)
	if len(got[dropStageRecord]) != 1 || got[dropStageRecord][domain.ActionBranchPushed] != 1 {
		t.Fatalf("drops = %v; want exactly one %s drop under op=%s", got, dropStageRecord, domain.ActionBranchPushed)
	}
}

// TestUpsertDeliverable_DirectFailureIsTheCallersToHandle: outside a relayed
// record, a failed deliverable is returned to the caller waiting on it and is not
// counted here — the caller decides what the failure means.
func TestUpsertDeliverable_DirectFailureIsTheCallersToHandle(t *testing.T) {
	fastRecordRetry(t)
	reader := installTestAuditDrops(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	refuseDeliverableWrites(t, conn)

	review := domain.NewReviewDeliverable("octo/repo", 1, "PR_1", "head-sha", info.ConversationID)
	if _, err := newDirectRuntime(stores, info).UpsertDeliverable(context.Background(), review, domain.GitHubIdentity{}); err == nil {
		t.Fatal("UpsertDeliverable = nil error, want the refused deliverable's error")
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
		Provider: domain.ProviderGitHub, Action: domain.ActionGHChannelWrite,
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
