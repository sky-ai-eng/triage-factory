// The pre-complete outcome-validity gate: after an agent terminates, the
// completion path requires a parseable terminal envelope carrying a valid
// `outcome` (continue|finish|abort|yield). When that's missing, this gate
// re-prompts the agent for one before any conservative fallback is accepted.
//
// It is deliberately a SEPARATE gate from the entity-memory write gate
// (memory.go) even though both resume the session up to a fixed retry cap:
// the memory ticket later consolidates the two into one completion gate that
// re-prompts for whatever's missing (memory file and/or outcome). Keeping
// them apart here means that consolidation doesn't have to first disentangle
// a prematurely-merged gate.

package delegate

import (
	"context"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
)

// maxOutcomeRetries caps how many times the outcome gate resumes a run to
// ask for a valid terminal envelope. Three mirrors the figure the SKY-417
// design pinned: enough that a model that fumbled the envelope format on
// its first try gets real chances to correct, without spending unbounded
// resumes on a model that's ignoring the contract.
const maxOutcomeRetries = 3

// outcomeRetryMessage is the correction prompt the gate resumes a run with
// when its terminal envelope lacks a valid outcome.
const outcomeRetryMessage = "Your final message was not a valid completion envelope. It must be ONLY a " +
	"JSON object whose \"outcome\" is one of \"continue\", \"finish\", \"abort\", or " +
	"\"yield\" — with a \"summary\" (and a \"reason\" when the outcome is \"abort\"). " +
	"Re-read the completion contract and return your terminal JSON again now, with " +
	"no other text."

// completionHasValidOutcome reports whether the completion's terminal
// envelope parses and carries a recognized outcome. A summary-only envelope
// (parses via isValid, empty outcome) returns false — that's exactly the
// case the gate re-prompts for.
func completionHasValidOutcome(completion *agentproc.Result) bool {
	parsed := parseAgentResult(completion.Result)
	return parsed != nil && parsed.hasValidOutcome()
}

// gateResumeFunc performs one resume attempt and returns the resumed
// session's terminal completion (nil if none was observed). Abstracted so
// the retry loop can be exercised without spawning a real agent.
type gateResumeFunc func() (*agentproc.Result, error)

// runOutcomeRetryLoop drives the re-prompt loop. If initial already carries a
// valid outcome it's returned untouched (resume never called). Otherwise it
// resumes up to maxRetries times, merging each attempt's completion into the
// running result so cost/duration/turn accounting spans the full run, and
// stops the moment a valid outcome appears. A resume error halts the loop and
// returns whatever's accumulated so far — the caller applies the conservative
// fallback. Pure (no spawner / DB), so the retry mechanics are unit-testable.
func runOutcomeRetryLoop(runID string, initial *agentproc.Result, resume gateResumeFunc, maxRetries int) *agentproc.Result {
	if completionHasValidOutcome(initial) {
		return initial
	}

	current := initial
	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("[delegate] run %s: terminal envelope lacks a valid outcome after attempt %d, resuming", runID, attempt-1)
		completion, err := resume()
		if err != nil {
			log.Printf("[delegate] run %s: outcome-gate resume attempt %d failed: %v", runID, attempt, err)
			// Give up on further retries — the caller applies the
			// conservative fallback. Don't wipe out the accumulated
			// completion's accounting just because the retry subprocess
			// crashed.
			return current
		}
		if completion != nil {
			current = agentproc.MergeResult(current, completion)
		}
		if completionHasValidOutcome(current) {
			return current
		}
	}

	return current
}

// runOutcomeGate enforces that a terminating run produced a parseable
// envelope with a valid outcome, re-prompting via ResumeWithMessage when it
// didn't. The gate does not touch run status or persist the outcome — the
// caller (processCompletion) parses the returned completion, applies the
// single/terminal-vs-blueprint fallback, and writes runs.outcome. Structure
// mirrors runMemoryGate: model + repoEnv are passed in rather than read from
// live spawner state, so retries reuse the run's original context, and a
// missing session id logs and returns without retrying.
func (s *Spawner) runOutcomeGate(
	ctx context.Context,
	orgID, runID, cwd string,
	initial *agentproc.Result,
	sessionID, model, repoEnv, extraAllowedTools string,
	triggerType, creatorUserID string,
) *agentproc.Result {
	if completionHasValidOutcome(initial) {
		return initial
	}

	if sessionID == "" {
		log.Printf("[delegate] run %s: terminal envelope lacks a valid outcome and no session id available — cannot gate-retry", runID)
		return initial
	}

	resumeOpts := ResumeOptions{Model: model, RepoEnv: repoEnv, ExtraAllowedTools: extraAllowedTools}
	resume := func() (*agentproc.Result, error) {
		outcome, err := s.ResumeWithMessage(ctx, orgID, runID, sessionID, cwd, outcomeRetryMessage, resumeOpts, triggerType, creatorUserID)
		if err != nil {
			return nil, err
		}
		if outcome == nil {
			return nil, nil
		}
		return outcome.Completion, nil
	}

	return runOutcomeRetryLoop(runID, initial, resume, maxOutcomeRetries)
}
