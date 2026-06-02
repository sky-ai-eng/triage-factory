// The pre-complete memory write gate (SKY-141), the cross-run task-memory
// + project-knowledge materializers a fresh agent invocation reads as
// ambient context, and the entity → project lookup that decides whether
// project knowledge applies.

package delegate

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// maxCompletionRetries is the hard cap on how many times the completion
// gate will resume a run to ask the agent for whatever it still owes
// before the run is accepted — its namespaced memory file and/or a valid
// terminal outcome. Three gives a model that fumbled the contract real
// chances to correct without spending unbounded resumes on one that's
// ignoring it. Not a config knob because no one needs to tune it per-run.
const maxCompletionRetries = 3

// memoryNamespace is the folder under _scratch/entity-memory/ that groups a
// run's memory file. It's the blueprint_run_id when the run belongs to a
// blueprint run — so every step of one workflow shares a folder and step N+1
// reads step N's memory as its handoff — else the run's own id (the N=1 case,
// until blueprint_run becomes universal). Both the write path (the agent's own
// file) and the read path (materialized priors) resolve through this, so the
// tree is uniformly foldered with no top-level .md files.
func memoryNamespace(blueprintRunID, runID string) string {
	if blueprintRunID != "" {
		return blueprintRunID
	}
	return runID
}

// memoryFileExists returns true iff the agent wrote
// ./_scratch/entity-memory/<namespace>/<runID>.md during the run. Used by the
// completion gate both before retrying (is another attempt needed?) and
// after (did the retry succeed?).
func memoryFileExists(cwd, namespace, runID string) bool {
	_, err := os.Stat(filepath.Join(cwd, "_scratch", "entity-memory", namespace, runID+".md"))
	return err == nil
}

// memoryFileState distinguishes the three reasons readAgentMemoryFile
// returns no usable content. They all map to the same DB signal
// (UpsertAgentMemory normalizes empty/whitespace to NULL agent_content
// === "agent didn't comply with the gate"), but each carries different
// diagnostic value when something looks wrong post-run, so the gate
// teardown logs them distinctly.
type memoryFileState int

const (
	memoryFilePresent memoryFileState = iota // file exists, has non-whitespace content
	memoryFileMissing                        // file does not exist on disk
	memoryFileEmpty                          // file exists but is empty / whitespace-only
	memoryFileReadErr                        // file exists, read failed (permissions, race, etc.)
)

// readAgentMemoryFile returns the agent-written
// ./_scratch/entity-memory/<namespace>/<runID>.md content along with a state
// classification. The content string is empty for every non-Present
// state — callers pass it straight to UpsertAgentMemory either way,
// but inspect the state to log distinctly rather than collapsing every
// form of noncompliance to the same line. Read errors that aren't a
// missing file are logged at the read site so they aren't lost when
// the caller picks a higher-level message.
func readAgentMemoryFile(cwd, namespace, runID string) (string, memoryFileState) {
	path := filepath.Join(cwd, "_scratch", "entity-memory", namespace, runID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", memoryFileMissing
		}
		log.Printf("[delegate] warning: failed to read memory file %s: %v", path, err)
		return "", memoryFileReadErr
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		return "", memoryFileEmpty
	}
	return content, memoryFilePresent
}

// completionHasValidOutcome reports whether the completion's terminal envelope
// parses and carries a recognized outcome. A summary-only envelope (parses via
// isValid, empty outcome) returns false — that's exactly a case the gate
// re-prompts for.
func completionHasValidOutcome(completion *agentproc.Result) bool {
	parsed := parseAgentResult(completion.Result)
	return parsed != nil && parsed.hasValidOutcome()
}

// completionRetryMessage builds the correction the gate resumes a run with,
// naming exactly what's still missing: the namespaced memory file, a valid
// terminal outcome, or both. memOK/outcomeOK are the current pass/fail of each
// check — at least one is false whenever this is called.
func completionRetryMessage(namespace, runID string, memOK, outcomeOK bool) string {
	memPath := fmt.Sprintf("$TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/%s/%s.md", namespace, runID)
	memMsg := "write your memory file to " + memPath + " using the absolute path (the env var " +
		"resolves to the run-root regardless of which worktree you have cd'd into): what you did " +
		"and the state you're leaving, the key decisions and why, what you ruled out, and what the " +
		"next stage needs"
	outcomeMsg := "return your terminal completion as your final message — ONLY a JSON object whose " +
		"\"outcome\" is one of \"continue\", \"finish\", \"abort\", or \"yield\", with a \"summary\" " +
		"(and a \"reason\" when the outcome is \"abort\"), and no other text"
	switch {
	case !memOK && !outcomeOK:
		return "Before this run can be accepted, two things are missing. First, " + memMsg +
			". Then, " + outcomeMsg + "."
	case !memOK:
		return "You returned a completion but did not " + memMsg +
			". Write it now, then return your completion JSON again."
	default: // !outcomeOK
		return "Your final message was not a valid completion envelope. Re-read the completion " +
			"contract and " + outcomeMsg + "."
	}
}

// runCompletionRetryLoop drives the consolidated gate's re-prompt loop. On each
// pass it asks the agent for whatever's still missing — the namespaced memory
// file (checked via memoryPresent, which re-stats disk) and/or a valid terminal
// outcome (checked on the merged completion) — merging each resume's completion
// so cost/duration/num_turns accounting spans the whole run. It stops the
// moment both checks pass, the completion becomes a yield (a pause isn't a
// termination; the caller's post-gate routeYield parks it, so we don't keep
// demanding the memory write), a resume errors, or maxRetries is exhausted.
// Pure (no spawner / DB) so the mechanics are unit-testable; resume performs one
// re-invoke given the correction text and returns the resumed completion (nil if
// none was observed).
func runCompletionRetryLoop(
	runID, namespace string,
	initial *agentproc.Result,
	memoryPresent func() bool,
	resume func(message string) (*agentproc.Result, error),
	maxRetries int,
) *agentproc.Result {
	memOK := memoryPresent()
	outcomeOK := completionHasValidOutcome(initial)
	if memOK && outcomeOK {
		return initial
	}

	current := initial
	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("[delegate] run %s: completion gate resuming (attempt %d; memory_present=%v outcome_valid=%v)", runID, attempt, memOK, outcomeOK)
		completion, err := resume(completionRetryMessage(namespace, runID, memOK, outcomeOK))
		if err != nil {
			log.Printf("[delegate] run %s: completion-gate resume attempt %d failed: %v", runID, attempt, err)
			// Give up on further retries — the caller applies its conservative
			// fallback. Don't wipe the accumulated completion's accounting just
			// because the retry subprocess crashed.
			return current
		}
		if completion != nil {
			current = agentproc.MergeResult(current, completion)
		}
		// A gate resume can itself end in a yield — the agent decided it needs
		// the user before it can finish. Stop and return; processCompletion's
		// post-gate routeYield parks it. A pause isn't a termination, so we
		// don't keep demanding the memory write on it.
		if parsed := parseAgentResult(current.Result); parsed != nil && parsed.isYield() {
			return current
		}
		memOK = memoryPresent()
		outcomeOK = completionHasValidOutcome(current)
		if memOK && outcomeOK {
			return current
		}
	}

	return current
}

// completionGate consolidates the two pre-finalization checks a terminating run
// must pass: (a) the agent wrote its namespaced memory file, and (b) the
// terminal envelope carries a valid outcome. If either is missing, it resumes
// the session with a correction naming exactly what's missing (memory file
// and/or outcome), re-checks after each attempt, and repeats up to
// maxCompletionRetries. Completions from resumed sessions are merged so
// cost/duration/num_turns accounting reflects the full span of the run.
//
// The gate does not touch runs status or persist the outcome — that remains the
// caller's responsibility. The caller (processCompletion) reads the memory
// file, upserts run_memory, parses the returned completion, applies the
// single/terminal-vs-blueprint outcome fallback, and writes runs.outcome. Side
// effects: (a) spawns resume subprocesses via ResumeWithMessage, whose messages
// land in run_messages via the runSink, (b) logs progress for operator
// diagnosis.
//
// Model and repoEnv are passed in rather than read from live spawner state so
// the gate's retries use the same model and repo context as the initial
// invocation. If we read s.model at resume time, a concurrent UpdateCredentials
// could silently switch models mid-run.
//
// If no session id is available (shouldn't happen in practice because the
// runSink persists the init event, but defensive), the gate logs and returns
// without retrying. The caller will see the missing file / outcome and apply
// its fallbacks (memory_missing, the finish/no-outcome floor).
func (s *Spawner) completionGate(
	ctx context.Context,
	orgID, runID, cwd, namespace string,
	initial *agentproc.Result,
	sessionID, model, repoEnv, extraAllowedTools string,
	triggerType, creatorUserID string,
) *agentproc.Result {
	memoryPresent := func() bool { return memoryFileExists(cwd, namespace, runID) }
	if memoryPresent() && completionHasValidOutcome(initial) {
		return initial
	}

	if sessionID == "" {
		log.Printf("[delegate] run %s: completion gate needs a resume but no session id available — cannot gate-retry", runID)
		return initial
	}

	resumeOpts := ResumeOptions{Model: model, RepoEnv: repoEnv, ExtraAllowedTools: extraAllowedTools, Namespace: namespace}
	resume := func(message string) (*agentproc.Result, error) {
		outcome, err := s.ResumeWithMessage(ctx, orgID, runID, sessionID, cwd, message, resumeOpts, triggerType, creatorUserID)
		if err != nil {
			return nil, err
		}
		if outcome == nil {
			return nil, nil
		}
		return outcome.Completion, nil
	}

	return runCompletionRetryLoop(runID, namespace, initial, memoryPresent, resume, maxCompletionRetries)
}

// materializePriorMemories writes any existing run_memory rows for the
// entity into <cwd>/_scratch/entity-memory/<namespace>/<prior_run_id>.md as
// individual markdown files, so a fresh agent invocation sees what previous
// iterations on the same task have already tried — and so the sibling steps of
// one blueprint run land in a shared folder the later steps read as their
// handoff. Each prior's namespace is its own blueprint_run_id (else its run
// id); the tree is all <namespace>/ folders with no top-level .md files. The
// agent is taught to read this layout by the envelope.
//
// namespace is the CURRENT run's namespace — its folder is created
// unconditionally, even on the very first run when there are no priors. Two
// reasons: the prompt instructs the agent to `ls` its namespace folder early
// (fails noisily without the dir), and the completion-gate retry message tells
// the agent to write to
// `$TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<namespace>/<run>.md` (which
// fails on a missing parent dir unless the agent guesses to mkdir first).
//
// Pattern: DB is the source of truth, we materialize into the worktree
// at startup, and ingest back on completion. The worktree is destroyed
// after every run, so these files never outlive their run on disk —
// only the DB rows do.
//
// Degrades gracefully: database errors, mkdir failures, or per-file
// write failures are logged but do not fail the run. An agent running
// without materialized priors is still useful, just without the
// cross-run memory benefit. This "advisory" posture only holds for
// the read side — the write-before-finish gate is enforced separately
// for NEW memories produced during the run.
func materializePriorMemories(taskMemory db.TaskMemoryStore, orgID, cwd, entityID, namespace string) {
	// Create the current run's namespace folder up front so the agent's
	// pre-flight `ls` and its own memory write both have a parent dir, even
	// when this entity has no prior memories.
	ownDir := filepath.Join(cwd, "_scratch", "entity-memory", namespace)
	if err := os.MkdirAll(ownDir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create entity-memory namespace dir at %s: %v", ownDir, err)
		return
	}

	memories, err := taskMemory.GetMemoriesForEntitySystem(context.Background(), orgID, entityID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load prior memories for entity %s: %v", entityID, err)
		return
	}
	if len(memories) == 0 {
		return
	}

	written := 0
	for _, m := range memories {
		dir := filepath.Join(cwd, "_scratch", "entity-memory", memoryNamespace(m.BlueprintRunID, m.RunID))
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("[delegate] warning: failed to create entity-memory namespace dir at %s: %v", dir, err)
			continue
		}
		filename := filepath.Join(dir, m.RunID+".md")
		if err := os.WriteFile(filename, []byte(m.Content), 0644); err != nil {
			log.Printf("[delegate] warning: failed to materialize task memory %s: %v", filename, err)
			continue
		}
		written++
	}
	if written > 0 {
		log.Printf("[delegate] materialized %d prior memories for entity %s", written, entityID)
	}
}

// lookupEntityProjectID returns the entity's project_id (or nil if the
// entity is unassigned, missing, or the lookup fails). Failure is
// logged and treated as "not assigned" — the spawner degrades gracefully
// rather than blocking the run on a non-essential context lookup.
func lookupEntityProjectID(entities db.EntityStore, orgID, entityID string) *string {
	entity, err := entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load entity %s for project lookup: %v", entityID, err)
		return nil
	}
	if entity == nil {
		return nil
	}
	return entity.ProjectID
}

// projectKnowledgeWarnBytes is the soft cap on per-project knowledge-base
// total size. We log when crossed but still copy — curated KB content is
// the user's intent, and silently dropping it would be more surprising
// than a noisy log line.
const projectKnowledgeWarnBytes = 500 * 1024

// streamCopyFile copies src to dst via io.Copy so large knowledge-base
// files don't get buffered fully in the spawner's heap. Returns bytes
// written. Uses 0644 to mirror the previous os.WriteFile behavior.
func streamCopyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// materializeProjectKnowledge stages the entity's project knowledge-base
// into <cwd>/_scratch/project-knowledge/ so the agent can read it as
// ambient context. Mirrors materializePriorMemories' "create the dir
// unconditionally" pattern so the agent's pre-flight `ls` doesn't fail
// noisily on ENOENT when no project is assigned.
//
// Reads from the Curator's per-project knowledge base (the path the
// Curator writes to per SKY-216, resolved through internal/paths under
// the project-owning org's subtree) and copies each .md file flat into
// _scratch/project-knowledge/, preserving source filenames. orgID is the
// run's owning tenant — the same org the assigned project belongs to —
// so in multi mode this reads the org-scoped dir the Curator wrote
// rather than the org-stripped default (SKY-402).
//
// Degrades gracefully: a nil projectID, a missing knowledge-base dir,
// or per-file copy failures are logged but never fail the run.
func materializeProjectKnowledge(orgID, cwd string, projectID *string) {
	dir := filepath.Join(cwd, "_scratch", "project-knowledge")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create project-knowledge dir at %s: %v", dir, err)
		return
	}

	if projectID == nil || *projectID == "" {
		return
	}

	if _, err := paths.StateRootErr(); err != nil {
		log.Printf("[delegate] warning: resolve knowledge dir for project %s: %v", *projectID, err)
		return
	}
	kbRoot := paths.ProjectKBDir(orgID, *projectID)
	srcDir := filepath.Join(kbRoot, "knowledge-base")

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[delegate] warning: read project knowledge-base %s: %v", srcDir, err)
		}
		return
	}

	written := 0
	totalBytes := int64(0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(dir, e.Name())
		n, err := streamCopyFile(src, dst)
		if err != nil {
			log.Printf("[delegate] warning: copy project knowledge file %s -> %s: %v", src, dst, err)
			continue
		}
		written++
		totalBytes += n
	}

	if totalBytes > projectKnowledgeWarnBytes {
		log.Printf("[delegate] project %s knowledge-base is %d bytes — over the %d soft cap; consider trimming", *projectID, totalBytes, projectKnowledgeWarnBytes)
	}
	if written > 0 {
		log.Printf("[delegate] materialized %d project-knowledge files for project %s", written, *projectID)
	}
}
