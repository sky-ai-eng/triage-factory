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
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// maxMemoryRetries is the hard cap on how many times the write-gate
// will resume a run to ask the agent to write its memory file. Chosen
// in the SKY-141 design: 0 retries is too strict (one missed write
// shouldn't discard work), 3+ is overkill (if the agent ignored the
// first correction, a third attempt is almost never the one that
// works). Not a config knob because no one needs to tune it per-run.
const maxMemoryRetries = 2

// memoryFileExists returns true iff the agent wrote
// ./_scratch/entity-memory/<runID>.md during the run. Used by the
// write-gate both before retrying (is another attempt needed?) and
// after (did the retry succeed?).
func memoryFileExists(cwd, runID string) bool {
	_, err := os.Stat(filepath.Join(cwd, "_scratch", "entity-memory", runID+".md"))
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
// ./_scratch/entity-memory/<runID>.md content along with a state
// classification. The content string is empty for every non-Present
// state — callers pass it straight to UpsertAgentMemory either way,
// but inspect the state to log distinctly rather than collapsing every
// form of noncompliance to the same line. Read errors that aren't a
// missing file are logged at the read site so they aren't lost when
// the caller picks a higher-level message.
func readAgentMemoryFile(cwd, runID string) (string, memoryFileState) {
	path := filepath.Join(cwd, "_scratch", "entity-memory", runID+".md")
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

// runMemoryGate enforces the pre-complete entity-memory file requirement.
//
// If the agent wrote ./_scratch/entity-memory/<runID>.md during its initial
// invocation, returns the original completion unchanged. Otherwise
// resumes the session (up to maxMemoryRetries times) with a correction
// message and re-checks after each attempt. Completions from resumed
// sessions are merged into the returned completion so cost/duration/
// num_turns accounting reflects the full span of the run.
//
// The gate does not touch runs status — that remains the caller's
// responsibility. Side effects: (a) spawns resume subprocesses via
// ResumeWithMessage, whose messages land in run_messages via the
// runSink, (b) logs progress for operator diagnosis.
//
// Model and repoEnv are passed in rather than read from live spawner
// state so the gate's retries use the same model and repo context as
// the initial invocation. If we read s.model at resume time, a
// concurrent UpdateCredentials could silently switch models mid-run.
//
// If no session id is available (shouldn't happen in practice because
// the runSink persists the init event, but defensive), the gate
// logs and returns without retrying. The caller will see a missing
// memory file and flag memory_missing.
func (s *Spawner) runMemoryGate(
	ctx context.Context,
	orgID, runID, taskID, cwd string,
	initial *agentproc.Result,
	sessionID, model, repoEnv, extraAllowedTools string,
	triggerType, creatorUserID string,
) *agentproc.Result {
	if memoryFileExists(cwd, runID) {
		return initial
	}

	if sessionID == "" {
		log.Printf("[delegate] run %s: memory file missing and no session id available — cannot gate-retry", runID)
		return initial
	}

	resumeOpts := ResumeOptions{Model: model, RepoEnv: repoEnv, ExtraAllowedTools: extraAllowedTools}

	current := initial
	for attempt := 1; attempt <= maxMemoryRetries; attempt++ {
		log.Printf("[delegate] run %s: memory file missing after attempt %d, resuming", runID, attempt-1)
		msg := fmt.Sprintf(
			"You returned a completion JSON but did not write your memory file to "+
				"$TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/%s.md. Write it now using the "+
				"absolute path (the env var resolves to the run-root regardless of "+
				"which worktree you have cd'd into) — one paragraph of what you did, "+
				"one of why, one of what to try next if this recurs — then return "+
				"your completion JSON again.",
			runID,
		)
		outcome, err := s.ResumeWithMessage(ctx, orgID, runID, sessionID, cwd, msg, resumeOpts, triggerType, creatorUserID)
		if err != nil {
			log.Printf("[delegate] run %s: resume attempt %d failed: %v", runID, attempt, err)
			// Give up on further retries — the caller will mark
			// memory_missing. Don't wipe out the initial completion's
			// accounting just because the retry subprocess crashed.
			return current
		}
		if outcome.Completion != nil {
			current = agentproc.MergeResult(current, outcome.Completion)
		}
		if memoryFileExists(cwd, runID) {
			return current
		}
	}

	return current
}

// materializePriorMemories writes any existing run_memory rows for the
// task into <cwd>/_scratch/entity-memory/<prior_run_id>.md as individual
// markdown files, so a fresh agent invocation sees what previous
// iterations on the same task have already tried. The agent is taught
// to read this directory by the envelope.
//
// The directory is created unconditionally — even on the very first run
// when there are no priors. Two reasons: the prompt instructs the agent
// to `ls _scratch/entity-memory/` early (fails noisily without the dir),
// and the memory-gate retry message tells the agent to write to
// `$TRIAGE_FACTORY_RUN_ROOT/_scratch/entity-memory/<run>.md` (which fails
// on a missing parent dir unless the agent guesses to mkdir first).
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
func materializePriorMemories(taskMemory db.TaskMemoryStore, orgID, cwd, entityID string) {
	memDir := filepath.Join(cwd, "_scratch", "entity-memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create entity-memory dir at %s: %v", memDir, err)
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
		filename := filepath.Join(memDir, m.RunID+".md")
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
// Reads from ~/.triagefactory/projects/<projectID>/knowledge-base/*.md
// (the path the Curator writes to per SKY-216) and copies each .md file
// flat into _scratch/project-knowledge/, preserving source filenames.
//
// Degrades gracefully: a nil projectID, a missing knowledge-base dir,
// or per-file copy failures are logged but never fail the run.
func materializeProjectKnowledge(cwd string, projectID *string) {
	dir := filepath.Join(cwd, "_scratch", "project-knowledge")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create project-knowledge dir at %s: %v", dir, err)
		return
	}

	if projectID == nil || *projectID == "" {
		return
	}

	kbRoot := paths.ProjectKBDir(runmode.LocalDefaultOrg, *projectID)
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
