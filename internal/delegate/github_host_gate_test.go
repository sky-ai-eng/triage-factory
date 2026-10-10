package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// countingClientResolver counts every call that would reach GitHub's
// credential path and answers each with err, so a run that gets past the host
// gate stops at its first GitHub step without a network call of its own.
type countingClientResolver struct {
	ghclient.Resolver
	calls *atomic.Int32
	err   error
}

func (r countingClientResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	r.calls.Add(1)
	return nil, r.err
}

func (r countingClientResolver) BaseURLFor(context.Context, string) (string, error) {
	r.calls.Add(1)
	return "", r.err
}

func dispatchAndWait(t *testing.T, fx *launchFixture) {
	t.Helper()
	conv := fx.conv
	conv.OrgID = runmode.LocalDefaultOrgID
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		fx.s.dispatchClaimedConversation(context.Background(), &conv, time.Now())
	}()
	select {
	case <-dispatched:
	case <-time.After(30 * time.Second):
		t.Fatal("the engagement never returned")
	}
}

// moveOrgGitHub points the fixture org's GitHub at host, leaving the task's
// pull request on the fixture's own host (dbtest.TestGitHubHost).
func moveOrgGitHub(t *testing.T, fx *launchFixture, host string) {
	t.Helper()
	if _, err := fx.stores.Orgs.SetSourceBaseURL(context.Background(), runmode.LocalDefaultOrgID, eventsource.KindGitHub, host); err != nil {
		t.Fatalf("SetSourceBaseURL: %v", err)
	}
}

// A step on a pull request whose host the org has left is refused at dispatch:
// nothing asks GitHub for a credential or a pull request, the step fails
// without a retry, and the reason names both hosts.
func TestDispatch_APullRequestOnAnotherGitHubHostIsRefusedBeforeGitHub(t *testing.T) {
	const newHost = "https://ghe.example.com"
	fx := newLaunchFixtureWithWorktree(t, "hostgate-other", "")
	moveOrgGitHub(t, fx, newHost)
	var calls atomic.Int32
	fx.s.SetRunCredentialResolvers(countingClientResolver{calls: &calls, err: errors.New("unreachable in this test")}, nil, nil)

	dispatchAndWait(t, fx)

	if n := calls.Load(); n != 0 {
		t.Errorf("GitHub credential path reached %d times; a refused run must make no GitHub call", n)
	}
	if st := fx.blueprintStatus(t); st != string(domain.BlueprintRunStatusFailed) {
		t.Errorf("blueprint_run status = %q, want failed", st)
	}
	if st := fx.storedStatus(t); st != domain.StatusFailed {
		t.Errorf("conversation status = %q, want failed", st)
	}
	br, err := fx.stores.Blueprints.GetRunSystem(context.Background(), runmode.LocalDefaultOrgID, fx.br.ID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	for _, host := range []string{dbtest.TestGitHubHost, newHost} {
		if !strings.Contains(br.AbortReason, host) {
			t.Errorf("abort reason %q does not name %s", br.AbortReason, host)
		}
	}
	for _, o := range fx.claimOutcomes(t) {
		if strings.HasPrefix(o, "requeued") {
			t.Errorf("claim outcomes = %v; a host refusal is settled and must not be handed back for a retry", fx.claimOutcomes(t))
		}
	}
}

// A conversation that has already worked is parked rather than failed, and its
// stop note names both hosts.
func TestDispatch_AStartedConversationOnAnotherGitHubHostParks(t *testing.T) {
	const newHost = "https://ghe.example.com"
	fx := newLaunchFixtureWithWorktree(t, "hostgate-started", "")
	fx.open(t)
	fx.speak(t, "assistant", "on it")
	moveOrgGitHub(t, fx, newHost)
	var calls atomic.Int32
	fx.s.SetRunCredentialResolvers(countingClientResolver{calls: &calls, err: errors.New("unreachable in this test")}, nil, nil)

	dispatchAndWait(t, fx)

	if n := calls.Load(); n != 0 {
		t.Errorf("GitHub credential path reached %d times; a refused run must make no GitHub call", n)
	}
	var status, reason string
	if err := fx.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, '') FROM conversations WHERE id = ?`, fx.conv.ID,
	).Scan(&status, &reason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != domain.StatusOpen || reason != string(domain.ParkReasonLaunchFailed) {
		t.Errorf("conversation = (%q, %q), want (open, %s)", status, reason, domain.ParkReasonLaunchFailed)
	}
	var note string
	if err := fx.database.QueryRow(
		`SELECT content FROM messages WHERE conversation_id = ? AND subtype = ? ORDER BY id DESC LIMIT 1`,
		fx.conv.ID, domain.MessageSubtypeStopNote,
	).Scan(&note); err != nil {
		t.Fatalf("read stop note: %v", err)
	}
	for _, host := range []string{dbtest.TestGitHubHost, newHost} {
		if !strings.Contains(note, host) {
			t.Errorf("stop note %q does not name %s", note, host)
		}
	}
	if st := fx.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running — a park moves no blueprint", st)
	}
}

// A step on a pull request on the org's current host passes the gate and goes
// on to resolve its GitHub client as before.
func TestDispatch_APullRequestOnTheCurrentGitHubHostPassesTheHostGate(t *testing.T) {
	fx := newLaunchFixtureWithWorktree(t, "hostgate-current", "")
	moveOrgGitHub(t, fx, dbtest.TestGitHubHost)
	var calls atomic.Int32
	noCreds := fmt.Errorf("%w: org=%s target=owner", ghclient.ErrNoGitHubCredentials, runmode.LocalDefaultOrgID)
	fx.s.SetRunCredentialResolvers(countingClientResolver{calls: &calls, err: noCreds}, nil, nil)

	dispatchAndWait(t, fx)

	if calls.Load() == 0 {
		t.Error("the run never reached its GitHub credential resolve; the host gate refused a pull request on the current host")
	}
	if got := fx.claimOutcomes(t); len(got) != 1 || got[0] != "requeued" {
		t.Errorf("claim outcomes = %v, want [requeued] — the missing credential, not a host refusal", got)
	}
	if st := fx.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", st)
	}
}

func TestGitHubHostMismatchError_NamesBothHosts(t *testing.T) {
	for _, tc := range []struct {
		err  gitHubHostMismatchError
		want []string
	}{
		{gitHubHostMismatchError{PRHost: "https://github.com", CurrentHost: "https://ghe.example.com"}, []string{"https://github.com", "https://ghe.example.com"}},
		{gitHubHostMismatchError{PRHost: "https://github.com"}, []string{"https://github.com", "no GitHub host configured"}},
		{gitHubHostMismatchError{CurrentHost: "https://ghe.example.com"}, []string{"records no GitHub host", "https://ghe.example.com"}},
	} {
		msg := tc.err.Error()
		for _, w := range tc.want {
			if !strings.Contains(msg, w) {
				t.Errorf("%+v.Error() = %q, want it to contain %q", tc.err, msg, w)
			}
		}
		if !errors.Is(&tc.err, errGitHubHostMismatch) {
			t.Errorf("%+v does not match errGitHubHostMismatch", tc.err)
		}
	}
}
