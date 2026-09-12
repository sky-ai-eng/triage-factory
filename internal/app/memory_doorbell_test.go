package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/memoryprovision"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// doorbellConversations records what the provisioner asked its store for, which
// is the only place a Nudge becomes observable from outside the package: the
// targeted form reads the conversation it was given, the org-wide form reads
// the org's owed list. Both answer empty, so nothing is generated and the
// assertion stays about the routing.
//
// The embedded nil interface is deliberate: any OTHER store method this reaches
// panics with the method name, so a dispatch that quietly took a different path
// fails loudly instead of passing.
type doorbellConversations struct {
	db.ConversationStore
	gets  chan [2]string
	lists chan listCall
}

type listCall struct {
	orgID   string
	backoff time.Duration
}

func (d *doorbellConversations) GetSystem(_ context.Context, orgID, conversationID string) (*domain.Conversation, error) {
	d.gets <- [2]string{orgID, conversationID}
	return nil, nil
}

func (d *doorbellConversations) ListMemoryOwedSystem(_ context.Context, orgID string, backoff time.Duration, _ int) ([]domain.MemoryOwed, error) {
	d.lists <- listCall{orgID: orgID, backoff: backoff}
	return nil, nil
}

// newDoorbellApp builds an App holding a real memory provisioner over the
// recording store, at the given role. A real Manager rather than a stub because
// the property under test is that the relay reaches Nudge with the right ids —
// which only means something against the code that consumes them.
func newDoorbellApp(t *testing.T, role runmode.DeployRole) (*App, *doorbellConversations) {
	t.Helper()
	conv := &doorbellConversations{
		gets:  make(chan [2]string, 4),
		lists: make(chan listCall, 4),
	}
	a := &App{plan: subsystemPlan{role: role}}
	// A live pool, because a non-holder's branch publishes through it. SQLite
	// has no pg_notify(), so every publish here FAILS — which is the second
	// thing this rig is for: a dropped doorbell must cost one sweep interval
	// and never take the boundary stamper down with it.
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	a.database = database
	a.memoryProvisioner = memoryprovision.NewManager(
		db.Stores{Conversations: conv}, nil, nil, nil, nil, nil,
	)
	return a, conv
}

func waitGet(t *testing.T, conv *doorbellConversations) [2]string {
	t.Helper()
	select {
	case got := <-conv.gets:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("the doorbell never reached the provisioner")
		return [2]string{}
	}
}

// A relayed memory_owed message reaches Nudge with the ids it carried. This is
// the far end of the doorbell: an executor or a standby control pod published
// it, every pod's shared listener heard it, and exactly the holder acts.
func TestApp_HandleCtlMessage_MemoryOwedReachesTheProvisioner(t *testing.T) {
	a, conv := newDoorbellApp(t, runmode.RoleAll) // role=all always self-holds
	a.handleCtlMessage(ctlbus.Message{Kind: "memory_owed", OrgID: "org-1", ConversationID: "conv-1"})
	if got := waitGet(t, conv); got != [2]string{"org-1", "conv-1"} {
		t.Errorf("provisioner asked about %v, want the ids the message carried", got)
	}
}

// The org-wide form re-sweeps the org ignoring the attempt backoff — that zero
// is the whole point of a configuration save's re-kick, because every owed
// conversation it could fix is one whose last attempt is recent enough to be
// inside the backoff.
func TestApp_HandleCtlMessage_MemoryOwedOrgWideIgnoresBackoff(t *testing.T) {
	a, conv := newDoorbellApp(t, runmode.RoleAll)
	a.handleCtlMessage(ctlbus.Message{Kind: "memory_owed", OrgID: "org-1"})
	select {
	case got := <-conv.lists:
		if got.orgID != "org-1" {
			t.Errorf("swept org %q, want org-1", got.orgID)
		}
		if got.backoff != 0 {
			t.Errorf("swept with backoff %v, want 0 — the re-kick must not skip a recently attempted conversation", got.backoff)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the org-wide doorbell never reached the provisioner")
	}
}

// The holder gate, for this kind specifically: NOTIFY fans out to every pod, so
// a non-holder hearing a memory_owed must drop it. Two of them would generate
// the same memory twice.
func TestApp_HandleCtlMessage_MemoryOwedIsHolderGated(t *testing.T) {
	for _, role := range []runmode.DeployRole{runmode.RoleControl, runmode.RoleExecutor} {
		a, conv := newDoorbellApp(t, role) // no leaseElector ⇒ never the holder
		a.handleCtlMessage(ctlbus.Message{Kind: "memory_owed", OrgID: "org-1", ConversationID: "conv-1"})
		a.handleCtlMessage(ctlbus.Message{Kind: "memory_owed", OrgID: "org-1"})
		select {
		case got := <-conv.gets:
			t.Errorf("role=%s acted on a relayed memory_owed (%v); only the holder may", role, got)
		case got := <-conv.lists:
			t.Errorf("role=%s swept on a relayed memory_owed (%v); only the holder may", role, got)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// The near end: a stamper on the pod that IS the brain calls Nudge directly
// rather than round-tripping through Postgres.
func TestApp_MemoryOwed_HolderNudgesInProcess(t *testing.T) {
	a, conv := newDoorbellApp(t, runmode.RoleAll)
	a.memoryOwed("org-1", "conv-1")
	if got := waitGet(t, conv); got != [2]string{"org-1", "conv-1"} {
		t.Errorf("provisioner asked about %v, want the ids the stamper rang with", got)
	}
}

// A stamper on a pod that is NOT the brain publishes instead of calling — an
// executor's failure boundary has no provisioner to reach. The publish itself
// needs a live Postgres pool, which a unit test has none of, so what is pinned
// here is the branch: the in-process provisioner is left untouched.
//
// It must also survive the publish failing. publishCtl logs and moves on by
// contract, and a boundary stamper that panicked on a dropped doorbell would
// turn one deferred sweep into a lost run.
func TestApp_MemoryOwed_NonHolderPublishesInsteadOfNudging(t *testing.T) {
	for _, role := range []runmode.DeployRole{runmode.RoleControl, runmode.RoleExecutor} {
		a, conv := newDoorbellApp(t, role)
		a.memoryOwed("org-1", "conv-1")
		select {
		case got := <-conv.gets:
			t.Errorf("role=%s nudged its own provisioner (%v); a non-holder must publish", role, got)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// An empty org names nothing to settle, and relaying it would widen a targeted
// nudge into a fleet-wide sweep that ignores every backoff. Refused at the
// publisher rather than absorbed downstream.
func TestApp_MemoryOwed_EmptyOrgIsRefused(t *testing.T) {
	a, conv := newDoorbellApp(t, runmode.RoleAll)
	a.memoryOwed("", "conv-1")
	a.memoryOwed("", "")
	select {
	case got := <-conv.gets:
		t.Errorf("an org-less doorbell reached the provisioner (%v)", got)
	case got := <-conv.lists:
		t.Errorf("an org-less doorbell swept (%v)", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// A message off the wire with no org_id is malformed, and dropping it is not
// mere hygiene: an empty org means EVERY org to the provisioner's sweep, so
// acting on one would amplify a single bad notification into a fleet-wide pass
// with the attempt backoff disabled — every tenant's backlog re-run because one
// payload was wrong.
//
// The dispatch hands on whatever arrived, so the refusal lives at the consumer
// (memoryprovision.Manager.Nudge), exactly as the trigger case's does in
// ai.Manager.Trigger. This pins the outcome through the real path rather than
// the placement: nothing scans, in either form.
func TestApp_HandleCtlMessage_MemoryOwedWithNoOrgIsDropped(t *testing.T) {
	a, conv := newDoorbellApp(t, runmode.RoleAll) // the holder, so the gate is not what refuses
	a.handleCtlMessage(ctlbus.Message{Kind: "memory_owed", ConversationID: "conv-1"})
	a.handleCtlMessage(ctlbus.Message{Kind: "memory_owed"})
	a.dispatchCtl(`{"kind":"memory_owed","conversation_id":"conv-1"}`)
	a.dispatchCtl(`{"kind":"memory_owed"}`)
	select {
	case got := <-conv.gets:
		t.Errorf("an org-less memory_owed read a conversation (%v)", got)
	case got := <-conv.lists:
		t.Errorf("an org-less memory_owed swept (%v) — an empty org is every org", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// The unified dispatcher routes the kind at all — a payload that never reaches
// handleCtlMessage is a doorbell nobody rings, and the kind lives in two switch
// statements (ctl.go's and relay.go's) that have to agree.
func TestApp_DispatchCtl_RoutesMemoryOwed(t *testing.T) {
	a, conv := newDoorbellApp(t, runmode.RoleAll)
	a.dispatchCtl(`{"kind":"memory_owed","org_id":"org-1","conversation_id":"conv-1"}`)
	if got := waitGet(t, conv); got != [2]string{"org-1", "conv-1"} {
		t.Errorf("dispatchCtl delivered %v, want the ids in the payload", got)
	}
}
