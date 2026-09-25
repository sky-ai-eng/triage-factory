package delegate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// takeoverBatch bounds how many expired claims one dispatch pass takes over.
// A pass runs every scan interval, so a backlog larger than this drains over
// a few passes rather than in one transaction that locks all of it.
const takeoverBatch = 100

// strandedRunGrace is how long a concluded step waits before its run counts
// as stranded. An ordinary reactor runs within milliseconds of the step's
// terminal commit; the grace keeps the replay off one that is merely running
// on another executor right now.
const strandedRunGrace = 60 * time.Second

// strandedRunLimit bounds how many stranded runs one dispatch pass replays.
const strandedRunLimit = 20

// ShutdownClaimReleaseTimeout bounds the clean-shutdown release. The process
// exits whether or not it finished: an unreleased claim is taken over after
// its lease, which costs a budget unit it did not need to but loses nothing.
const ShutdownClaimReleaseTimeout = 10 * time.Second

// DefaultMaxClaimLosses is TF_MAX_CLAIM_ATTEMPTS's default: how many
// engagements of one conversation may be lost in a row before its next claim
// fails it instead of running it. Distinct from maxClaimAttempts, which
// budgets setup failures — a host that cannot build a workspace says nothing
// about whether a conversation kills its executor.
const DefaultMaxClaimLosses = 2

// ParseMaxClaimLosses parses TF_MAX_CLAIM_ATTEMPTS. Empty maps to
// DefaultMaxClaimLosses; anything else must parse as a positive integer.
func ParseMaxClaimLosses(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultMaxClaimLosses, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid TF_MAX_CLAIM_ATTEMPTS=%q (want a positive integer)", raw)
	}
	return n, nil
}

// SetMaxClaimLosses installs the loss budget. Zero (the NewSpawner default)
// falls back to DefaultMaxClaimLosses at use time.
func (s *Spawner) SetMaxClaimLosses(n int) {
	s.mu.Lock()
	s.maxClaimLosses = n
	s.mu.Unlock()
}

func (s *Spawner) maxClaimLossesOrDefault() int {
	s.mu.Lock()
	n := s.maxClaimLosses
	s.mu.Unlock()
	if n <= 0 {
		return DefaultMaxClaimLosses
	}
	return n
}

// SetCellsConfirmedClean records whether this process confirmed, before its
// dispatcher started, that every cell a previous boot of this executor left
// behind is torn down. The boot reset releases that boot's claims at once,
// which is only safe when nothing of the old engagements can still be
// running; unconfirmed, the claims are left to lapse and be taken over after
// their lease, by which time the old cells have had the whole lease to die.
// False until set, so a caller that never confirms gets the slow path.
func (s *Spawner) SetCellsConfirmedClean(clean bool) {
	s.cellsConfirmedClean.Store(clean)
}

// takeOverExpiredClaims releases every expired claim another executor, or an
// earlier boot of this one, left behind. Its holder is gone — a live holder
// fences itself before its lease lapses — so the release is what returns the
// conversation to the queue, and the settlement pass after it settles a stop
// or a cancel pending on it. The count it adds to the loss budget is the one
// the next claim of the conversation reads.
func (s *Spawner) takeOverExpiredClaims(ctx context.Context) {
	if s.conversationQueue == nil {
		return
	}
	executorID, bootEpoch := s.executorIdentity()
	released, err := s.conversationQueue.TakeOverExpiredClaimsSystem(ctx, executorID, bootEpoch, takeoverBatch)
	if err != nil {
		dispatchLog.Warn("take over expired claims failed; retrying on the next scan", "error", err)
		return
	}
	for _, c := range released {
		dispatchLog.Warn("took over an expired claim; the conversation returns to the queue",
			"conversation", c.ConversationID, "claim", c.ClaimID, "org_id", c.OrgID)
	}
}

// replayStrandedRuns runs the reactor for every running blueprint run whose
// current step concluded without it: the executor that drove the step died
// between the step's terminal commit and the reactor's transition. Nothing
// else reads a concluded step, so without this the run holds its task's one
// active slot forever.
//
// A replay that races a late reactor is harmless: an advance is
// compare-and-swap guarded on the step it advances from, and a terminal's
// side effects run only for the write that actually ended the run.
func (s *Spawner) replayStrandedRuns(ctx context.Context) {
	if s.conversationQueue == nil || s.blueprints == nil || s.conversations == nil || s.tasks == nil {
		return
	}
	stranded, err := s.conversationQueue.StrandedBlueprintRunsSystem(ctx, strandedRunGrace, strandedRunLimit)
	if err != nil {
		dispatchLog.Warn("list stranded blueprint runs failed; retrying on the next scan", "error", err)
		return
	}
	for _, st := range stranded {
		if ctx.Err() != nil {
			return
		}
		s.replayStrandedRun(ctx, st)
	}
}

func (s *Spawner) replayStrandedRun(ctx context.Context, st db.StrandedRun) {
	// This process's own engagement on the step is still between its
	// terminal and its reactor, however long that is taking.
	if s.engagementFor(st.ConversationID) != nil {
		return
	}
	br, err := s.blueprints.GetRunSystem(ctx, st.OrgID, st.BlueprintRunID)
	if err != nil || br == nil {
		dispatchLog.Warn("load stranded blueprint run failed; retrying on the next scan",
			"blueprint_run", st.BlueprintRunID, "org_id", st.OrgID, "error", err)
		return
	}
	conv, err := s.conversations.GetSystem(ctx, st.OrgID, st.ConversationID)
	if err != nil || conv == nil {
		dispatchLog.Warn("load stranded run's step conversation failed; retrying on the next scan",
			"blueprint_run", st.BlueprintRunID, "conversation", st.ConversationID, "error", err)
		return
	}
	task, err := s.tasks.GetSystem(ctx, st.OrgID, br.TaskID)
	if err != nil || task == nil {
		dispatchLog.Warn("load stranded run's task failed; retrying on the next scan",
			"blueprint_run", st.BlueprintRunID, "task", br.TaskID, "error", err)
		return
	}
	conv.OrgID = st.OrgID
	dispatchLog.Warn("replaying the reactor for a blueprint run whose current step concluded without it",
		"blueprint_run", br.ID, "conversation", conv.ID, "step", conv.BlueprintStepIndex, "status", conv.Status, "org_id", st.OrgID)
	// The PR-branch cleanup a terminal would run needs the PR coordinates,
	// which no row carries; the worktree removal is best-effort, as it is for
	// the settlement's cancel.
	cfg := runConfig{
		orgID:  st.OrgID,
		teamID: conv.TeamID,
		wtPath: br.WorktreePath,
		hasWT:  br.WorktreePath != "" && task.EntitySource == "github",
	}
	s.reactToStepTerminal(ctx, st.OrgID, br, *conv, cfg, time.Now())
}

// disposeOfLostConversation fails a conversation whose engagements have been
// lost as many times in a row as the loss budget allows. The claim it holds
// is the one that would have run it next, and every write goes through that
// claim's fence, so a successor never has its conversation failed under it.
//
// This is what keeps a conversation that kills its executor from doing it to
// every executor in turn: a crash is recovered from, a crash loop is not.
func (s *Spawner) disposeOfLostConversation(ctx context.Context, conv *domain.Conversation) {
	orgID := conv.OrgID
	bgCtx := context.WithoutCancel(ctx)
	summary := fmt.Sprintf("Failed: the executor engagement was lost %d times in a row (its lease lapsed) and the loss budget (TF_MAX_CLAIM_ATTEMPTS) is spent", conv.LostEngagements)
	dispatchLog.Error("the loss budget is spent; failing the conversation without running it",
		"conversation", conv.ID, "lost_engagements", conv.LostEngagements, "org_id", orgID)
	s.endEngagement(conv.ID, engagementLost)
	if _, err := s.conversations.CompleteForClaimSystem(bgCtx, orgID, conv.ID, conv.ClaimID, "failed", 0, 0, 0,
		summary, "", "", string(domain.ConversationFailureExecutorLost)); err != nil {
		if errors.Is(err, db.ErrClaimReleased) {
			dispatchLog.Error("claim fence refused the executor-lost terminal — a successor owns this conversation; recording nothing",
				"conversation", conv.ID, "claim_id", conv.ClaimID, "org_id", orgID)
			return
		}
		dispatchLog.Warn("fail a conversation whose loss budget is spent failed", "conversation", conv.ID, "error", err)
		return
	}
	ended, err := s.conversations.EndConversationSystem(bgCtx, orgID, conv.ID, domain.EndedFailed)
	if err != nil {
		dispatchLog.Warn("stamp the failure boundary on a conversation whose loss budget is spent failed", "conversation", conv.ID, "error", err)
	} else if ended != nil {
		s.kickMemoryOwed(orgID, ended.ID)
	}
	s.broadcastConversationUpdate(orgID, conv.ID, "failed")
	if conv.BlueprintRunID != "" {
		s.terminateBlueprint(orgID, conv.BlueprintRunID, conv.TaskID, conv.TriggerType, conv.CreatorUserID, time.Now(),
			runConfig{orgID: orgID, teamID: conv.TeamID},
			domain.BlueprintRunStatusFailed, string(domain.ConversationFailureExecutorLost), conv.BlueprintStepIndex, false)
	}
	// Here as well as in the terminal: a follow-up on a run that had already
	// ended has no run to end, and its task's firings still wait on it.
	s.wakeFirings()
}

// ReleaseOwnClaimsOnShutdown hands back, as 'requeued_shutdown', every claim
// this executor boot still holds whose engagement has returned. It runs on the
// way out of a clean shutdown, after the dispatcher and its dispatches have
// been joined: a conversation whose engagement was cancelled by the shutdown
// is claimable at once, and spends neither budget, instead of waiting out its
// lease and counting as a loss. An engagement whose runtime came up has
// already handed its own claim back (handBackOnShutdown); what is left here
// is the engagements that stood down before that, at a gate or in bring-up.
// An engagement still registered keeps its claim — it may yet write — and is
// found later as the loss it then is.
//
// Only once the claim loop has provably stopped: a claim minted after the
// check would have no engagement registered yet, and releasing it would hand
// a successor a conversation this process is about to start.
func (s *Spawner) ReleaseOwnClaimsOnShutdown() {
	if s.conversationQueue == nil {
		return
	}
	if s.dispatcherRunning.Load() {
		dispatchLog.Warn("shutdown claim release skipped: the claim loop has not stopped, so a claim it mints could be released under its own engagement; its claims will be taken over after their leases")
		return
	}
	executorID, bootEpoch := s.executorIdentity()
	if executorID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownClaimReleaseTimeout)
	defer cancel()
	live, err := s.conversationQueue.LiveClaimsOfExecutorSystem(ctx, executorID, bootEpoch)
	if err != nil {
		dispatchLog.Warn("shutdown claim release: list this executor's claims failed; they will be taken over after their leases", "error", err)
		return
	}
	var idle []string
	for _, c := range live {
		if s.engagementFor(c.ConversationID) == nil {
			idle = append(idle, c.ConversationID)
		}
	}
	if len(idle) == 0 {
		return
	}
	n, err := s.conversationQueue.ReleaseOwnClaimsOnShutdownSystem(ctx, executorID, bootEpoch, idle)
	if err != nil {
		dispatchLog.Warn("shutdown claim release failed; the claims will be taken over after their leases", "error", err)
		return
	}
	dispatchLog.Info("released this executor's claims on shutdown; their conversations are claimable now",
		"released", n, "still_engaged", len(live)-len(idle))
}
