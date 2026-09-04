package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// bringUpResolver hands every GitHub read to one client and nothing else. It
// deliberately does not implement ghclient.ScopedResolver, so the local git
// channel stands down (as it does for every narrow test resolver) and the
// claim's bring-up reaches the PR fetch with nothing else in the way.
type bringUpResolver struct {
	ghclient.Resolver
	client *ghclient.Client
}

func (r bringUpResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

// TestDispatch_StopDuringBringUpCancelsTheSetupAndParks pins the window this
// ticket closes. The claim's cancel handle is registered before bring-up, so a
// stop that lands while the PR fetch is in flight cancels the fetch, and the
// engagement reads the cancelled setup as the stop it is: the conversation
// ends parked with its claim released, the stop's note is the only thing on
// the transcript, the blueprint stays running (a plain stop freezes it), and
// no agent ever ran under the released claim.
//
// The fetch is a real HTTP request to a server that holds the connection open
// until the request's context is cancelled. Registered late, the stop would
// find no handle and take the DB-only path, the fetch would run to its
// server-side deadline, and the assertion on how it ended would fail.
func TestDispatch_StopDuringBringUpCancelsTheSetupAndParks(t *testing.T) {
	fx := newLaunchFixtureWithWorktree(t, "938", "")

	fetchEntered := make(chan struct{})
	fetchEnded := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(fetchEntered)
		select {
		case <-r.Context().Done():
			fetchEnded <- true
		case <-time.After(10 * time.Second):
			fetchEnded <- false
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	fx.s.SetRunCredentialResolvers(bringUpResolver{client: ghclient.NewClient(server.URL, "test-token")}, nil, nil)

	conv := fx.conv
	conv.OrgID = runmode.LocalDefaultOrgID
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		fx.s.dispatchClaimedConversation(context.Background(), &conv)
	}()

	select {
	case <-fetchEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("bring-up never reached the PR fetch")
	}
	if err := fx.s.Stop(runmode.LocalDefaultOrgID, conv.ID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop during bring-up: %v", err)
	}

	select {
	case cancelled := <-fetchEnded:
		if !cancelled {
			t.Fatal("the stop did not cancel the in-flight fetch; the setup goroutine was never told")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the fetch never ended")
	}
	select {
	case <-dispatched:
	case <-time.After(10 * time.Second):
		t.Fatal("the engagement did not return after its setup was cancelled")
	}

	if got := fx.storedStatus(t); got != "open" {
		t.Errorf("status = %q, want open (a stop is a park)", got)
	}
	if got := fx.claimOutcomes(t); len(got) != 1 || got[0] != "cancelled" {
		t.Errorf("claim outcomes = %v, want the one claim released as cancelled", got)
	}
	if got := fx.blueprintStatus(t); got != "running" {
		t.Errorf("blueprint status = %q, want running (a plain stop freezes the blueprint)", got)
	}
	rows := fx.transcript(t)
	if len(rows) != 1 || rows[0].Subtype != domain.MessageSubtypeStopNote {
		t.Errorf("transcript = %+v, want exactly the stop's own note — nothing ran under the released claim", rows)
	}
	fx.s.mu.Lock()
	_, registered := fx.s.cancels[conv.ID]
	fx.s.mu.Unlock()
	if registered {
		t.Error("the cancel handle outlived the engagement")
	}
}
