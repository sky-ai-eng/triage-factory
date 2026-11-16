package app

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// passConversations signals each time a DelegatorNotices pass reads the
// failure list, its last read, and answers both due lists empty. Any other
// store method panics through the nil interface.
type passConversations struct {
	db.ConversationStore
	passes chan struct{}
}

func (p *passConversations) ListPauseNoticesDueSystem(context.Context, int) ([]domain.PauseNoticeDue, error) {
	return nil, nil
}

func (p *passConversations) ListFailureNoticesDueSystem(context.Context, int) ([]domain.FailureNoticeDue, error) {
	p.passes <- struct{}{}
	return nil, nil
}

// A relayed delegator_notice reaches the brain's sweep as a pass. The kind is
// named in two switch statements, ctl.go's and relay.go's, and a payload either
// one drops is a doorbell nobody hears.
func TestApp_DispatchCtl_DelegatorNoticeRunsAPass(t *testing.T) {
	notifytest.Record(t)
	conv := &passConversations{passes: make(chan struct{}, 4)}
	a := &App{plan: subsystemPlan{role: runmode.RoleAll}}
	// An interval no test waits out, so a pass is the doorbell's.
	a.delegatorNotices = delegate.NewDelegatorNotices(conv, time.Hour)
	brainCtx, stopBrain := context.WithCancel(context.Background())
	t.Cleanup(stopBrain)
	go a.delegatorNotices.Run(brainCtx)

	a.dispatchCtl(`{"kind":"delegator_notice","org_id":"org-1"}`)
	select {
	case <-conv.passes:
	case <-time.After(2 * time.Second):
		t.Fatal("the relayed doorbell never ran a pass")
	}
}
