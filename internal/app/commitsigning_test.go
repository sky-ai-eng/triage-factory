package app

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
)

// The unified tf_ctl dispatcher routes sign_done to the request an executor's
// run is waiting on, outside the brain's holder gate: an executor never holds
// the lease, so a gated doorbell would never ring.
func TestApp_DispatchCtl_RingsTheWaitingSignatureRequest(t *testing.T) {
	a := &App{commitSignDoorbells: commitsigner.NewDoorbells()}
	ring, stop := a.commitSignDoorbells.Listen("sig-1")
	defer stop()

	a.dispatchCtl(`{"kind":"sign_done","org_id":"org-1","signature_id":"sig-1"}`)
	select {
	case <-ring:
	case <-time.After(time.Second):
		t.Fatal("sign_done did not ring the request waiting on sig-1")
	}
}

// Each doorbell reaches every pod, and each role holds only one receiver: a
// control pod has no requests to ring, an executor no signer to nudge.
func TestApp_DispatchCtl_SigningDoorbellsIgnoredWhereNothingListens(t *testing.T) {
	a := &App{}
	a.dispatchCtl(`{"kind":"sign_request","org_id":"org-1","signature_id":"sig-1"}`)
	a.dispatchCtl(`{"kind":"sign_done","org_id":"org-1","signature_id":"sig-1"}`)
}
