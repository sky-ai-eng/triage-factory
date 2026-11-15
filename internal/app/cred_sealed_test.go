package app

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/pgnotify/ctlbus"
)

// The unified tf_ctl dispatcher routes cred_sealed to the credential wait of
// the conversation it names, outside the brain's holder gate: the executor
// waiting never holds the lease.
func TestApp_DispatchCtl_RingsTheWaitingCredentialWait(t *testing.T) {
	a := &App{credDoorbells: ctlbus.NewDoorbells()}
	ring, stop := a.credDoorbells.Listen("conv-1")
	defer stop()

	a.dispatchCtl(`{"kind":"cred_sealed","org_id":"org-1","conversation_id":"conv-1"}`)
	select {
	case <-ring:
	case <-time.After(time.Second):
		t.Fatal("cred_sealed did not ring the credential wait of conv-1")
	}
}

// A control pod runs no engagement and has no credential doorbells; the
// cred_sealed its own brain publishes reaches it too, and is ignored.
func TestApp_DispatchCtl_CredSealedIgnoredWhereNothingWaits(t *testing.T) {
	a := &App{}
	a.dispatchCtl(`{"kind":"cred_sealed","org_id":"org-1","conversation_id":"conv-1"}`)
}
