package delegate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// A run whose org's commit identity cannot be read does not start: runAgent
// hands the engagement back as a launch failure before it writes anything
// about the launch, and the dispatcher's hand-back requeues the claim on the
// budget its cause spends. A run that started would commit as whatever its git
// config says while the org's credential pushes the commits.
func TestRunAgent_FailedCommitIdentityReadHandsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		suffix  string
		readErr error
		want    db.RequeueOutcome
	}{
		"a failed read":           {"ident-read", errors.New("keychain: item could not be read"), db.RequeueSetupFailure},
		"an unreachable upstream": {"ident-upstream", &upstream.TransportError{Err: errors.New("dial tcp: connection refused")}, db.RequeueUpstreamUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			f := newLaunchFixture(t, tc.suffix)
			f.s.SetRunCredentialResolvers(&fakeResolver{orgLogin: "acme-bot", orgIdentityErr: tc.readErr}, nil, nil)
			org := runmode.LocalDefaultOrgID
			conv := f.conv

			disp := f.s.runAgent(context.Background(), conv.ID, loadTask(t, f.s, conv.TaskID), "mission",
				runConfig{orgID: org, claimID: conv.ClaimID, teamID: conv.TeamID, runRoot: f.worktree},
				time.Now(), conv.Model, conv.TriggerType, conv.CreatorUserID, "")

			if !errors.Is(disp.launchErr, tc.readErr) {
				t.Fatalf("disposition = %+v, want a launch failure carrying the identity read's error", disp)
			}
			if disp.fenced || disp.handedBack {
				t.Errorf("disposition = %+v, want neither fenced nor handed back", disp)
			}
			// Nothing about the launch was written, so no agent was started on it.
			if block := f.s.launchedSystemBlock(context.Background(), org, conv.ID); block != "" {
				t.Errorf("the launch's system block was stored (%d bytes); the run must stop before it", len(block))
			}
			if rows := f.transcript(t); len(rows) != 0 {
				t.Errorf("transcript = %+v, want nothing — no agent ran", rows)
			}

			f.s.handlePreAgentFailure(org, f.br, conv, disp.launchErr)
			if got := f.claimOutcomes(t); len(got) != 1 || got[0] != string(tc.want) {
				t.Errorf("claim outcomes = %v, want [%s]", got, tc.want)
			}
			if st := f.storedStatus(t); st != "" {
				t.Errorf("stored status = %q, want none — the conversation is still mid-flight", st)
			}
		})
	}
}

// A cold resume whose org's commit identity cannot be read hands its claim
// back as a setup failure before it takes the queued message, so the next
// claim delivers it. The resume would otherwise go on to the session check,
// which this fixture's worktree fails.
func TestDispatchResumeClaim_FailedCommitIdentityReadHandsBack(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const convID = "r-ident"
	seedConversation(t, database, convID, "sess-ident", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id=?`, convID); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	readErr := errors.New("keychain: item could not be read")
	s.SetRunCredentialResolvers(&fakeResolver{orgLogin: "acme-bot", orgIdentityErr: readErr}, nil, nil)

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, convID, runmode.LocalDefaultUserID, "the answer"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	claimAndDispatch(t, s, database)

	rows, err := database.Query(`SELECT COALESCE(outcome, '') FROM claims WHERE conversation_id = ? ORDER BY claimed_at`, convID)
	if err != nil {
		t.Fatalf("read claims: %v", err)
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatalf("scan claim: %v", err)
		}
		outcomes = append(outcomes, o)
	}
	if len(outcomes) != 1 || outcomes[0] != string(db.RequeueSetupFailure) {
		t.Errorf("claim outcomes = %v, want [requeued]", outcomes)
	}
	if st := storedStatus(t, database, convID); st == "failed" {
		t.Errorf("stored status = %q; the resume must not reach the session check", st)
	}
	msg, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, convID)
	if err != nil || !ok || msg != "the answer" {
		t.Errorf("pending input = (%q, %v, %v), want the queued message still undelivered", msg, ok, err)
	}
}
