// Run-memory file reading at termination, the cross-run task-memory +
// project-knowledge materializers a fresh agent invocation reads as ambient
// context, and the entity → project lookup that decides whether project
// knowledge applies. (The completion envelope's bounded re-prompt-to-fix lives
// on the live driver — see live.go.)

package delegate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// maxCompletionRetries is the hard cap on how many times the live driver
// re-prompts a run to fix an invalid completion envelope (malformed JSON, an
// unrecognized outcome, or a recognized outcome missing its required
// companion field) before failing it. Three gives a model that fumbled the
// contract real chances to correct without spending unbounded turns on one
// that's ignoring it. Not a config knob because no one needs to tune it
// per-run. A turn that ends with NO envelope attempt is not retried — the run
// is left open (see driveLiveRun). The live driver's results-channel buffer is
// sized off this (resultsBufferDepth in live.go); keep that in mind if you bump
// it.
const maxCompletionRetries = 3

// memoryNamespace is the folder under _scratch/entity-memory/ that groups a
// run's memory file: the blueprint_run_id its run belongs to, so every step of
// one blueprint shares a folder and step N+1 reads step N's memory as its
// handoff. Every run is a blueprint step now (a single prompt is a 1-step
// blueprint), so there is no run-id fallback — the value is always the
// blueprint_run_id. The runID arg is retained so call sites read uniformly and a
// future change can't silently mis-key. Both the write path (the agent's own
// file) and the read path (materialized priors) resolve through this, so the
// tree is uniformly foldered with no top-level .md files.
func memoryNamespace(blueprintRunID, runID string) string {
	_ = runID
	return blueprintRunID
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
		delegateLog.Warn("read memory file failed", "path", path, "error", err)
		return "", memoryFileReadErr
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		return "", memoryFileEmpty
	}
	return content, memoryFilePresent
}

// materializePriorMemories writes any existing conversation_memory rows for the
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
// `$TRIAGE_FACTORY_CONVERSATION_ROOT/_scratch/entity-memory/<namespace>/<run>.md` (which
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
func materializePriorMemories(taskMemory db.TaskMemoryStore, orgID, teamID, cwd, entityID, namespace string) {
	// Create the current run's namespace folder up front so the agent's
	// pre-flight `ls` and its own memory write both have a parent dir, even
	// when this entity has no prior memories.
	ownDir := filepath.Join(cwd, "_scratch", "entity-memory", namespace)
	if err := os.MkdirAll(ownDir, 0755); err != nil {
		delegateLog.Warn("create entity-memory namespace dir failed", "path", ownDir, "error", err)
		return
	}

	// teamID is THIS run's owning team. The System read scopes the prior
	// memory to what that team can see, so a run never materializes another
	// team's run narratives on a shared entity (TFAC-506).
	memories, err := taskMemory.GetMemoriesForEntitySystem(context.Background(), orgID, entityID, teamID)
	if err != nil {
		delegateLog.Warn("load prior memories for entity failed", "entity", entityID, "error", err)
		return
	}
	if len(memories) == 0 {
		return
	}

	written := 0
	for _, m := range memories {
		dir := filepath.Join(cwd, "_scratch", "entity-memory", memoryNamespace(m.BlueprintRunID, m.RunID))
		if err := os.MkdirAll(dir, 0755); err != nil {
			delegateLog.Warn("create entity-memory namespace dir failed", "path", dir, "error", err)
			continue
		}
		filename := filepath.Join(dir, m.RunID+".md")
		if err := os.WriteFile(filename, []byte(m.Content), 0644); err != nil {
			delegateLog.Warn("materialize task memory failed", "path", filename, "error", err)
			continue
		}
		written++
	}
	if written > 0 {
		delegateLog.Info("materialized prior memories for entity", "count", written, "entity", entityID)
	}
}

// lookupEntityProjectID returns the entity's project_id (or nil if the
// entity is unassigned, missing, or the lookup fails). Failure is
// logged and treated as "not assigned" — the spawner degrades gracefully
// rather than blocking the run on a non-essential context lookup.
func lookupEntityProjectID(entities db.EntityStore, orgID, entityID string) *string {
	entity, err := entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil {
		delegateLog.Warn("load entity for project lookup failed", "entity", entityID, "error", err)
		return nil
	}
	if entity == nil {
		return nil
	}
	return entity.ProjectID
}

// attachRunMemoryEntities makes a terminated run's memory reachable from every
// entity the run materially engaged: the primary (task) entity, plus every
// entity it produced (derived from the run's artifacts). Touched entities are
// recorded durably at verb time by the exec funnel; role precedence in
// RecordEntityTouchSystem (primary > produced > touched) converges all three
// sets, so this needs no ordering care against those mid-run writes — a
// touched row upgrades to produced here, and the primary entity ends primary
// even if it was also touched or produced.
//
// Best-effort and non-fatal throughout: it runs after the conversation_memory upsert
// has already landed, so a join-row failure is logged and skipped, never
// aborts completion.
//
// The primary attach is unconditional, exactly like the upsert — the task's
// entity carries the run's memory even when the agent wrote no file
// (agent_content NULL). On the produced side, FindOrCreate (not lookup-only)
// is deliberate: a PR the run just opened may not have been polled yet, so the
// attach mints the create-minimal stub the poller/enrichment path later fills
// (the artifact URL links it out). A repo-level artifact target (a branch
// push, or owner/repo with no '#N') maps to no entity and is skipped.
func (s *Spawner) attachRunMemoryEntities(ctx context.Context, orgID, runID, primaryEntityID string) {
	if s.taskMemory == nil {
		return
	}
	// primary — the task's entity always carries the run's memory.
	if err := s.taskMemory.RecordEntityTouchSystem(ctx, orgID, runID, primaryEntityID, domain.MemoryRolePrimary); err != nil {
		delegateLog.Warn("attach primary entity to run memory failed", "run", runID, "entity", primaryEntityID, "error", err)
	}

	if s.artifacts == nil || s.entities == nil {
		return
	}
	// produced — every external object the run created/mutated, resolved from
	// its artifacts. A listing failure leaves the upsert + primary row intact.
	arts, err := s.artifacts.ListByRunSystem(ctx, orgID, runID)
	if err != nil {
		delegateLog.Warn("list artifacts for produced-entity attach failed", "run", runID, "error", err)
		return
	}
	for _, a := range arts {
		source, sourceID, kind, ok := domain.EntityRefForExternal(a.Provider, a.Target)
		if !ok {
			continue
		}
		ent, _, err := s.entities.FindOrCreateSystem(ctx, orgID, source, sourceID, kind, "", a.URL)
		if err != nil || ent == nil {
			delegateLog.Warn("resolve produced entity for run memory failed",
				"run", runID, "provider", a.Provider, "target", a.Target, "error", err)
			continue
		}
		if err := s.taskMemory.RecordEntityTouchSystem(ctx, orgID, runID, ent.ID, domain.MemoryRoleProduced); err != nil {
			delegateLog.Warn("attach produced entity to run memory failed", "run", runID, "entity", ent.ID, "error", err)
		}
	}
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
// Curator writes to, resolved through internal/paths under
// the project-owning org's subtree) and copies each .md file flat into
// _scratch/project-knowledge/, preserving source filenames. orgID is the
// run's owning tenant — the same org the assigned project belongs to —
// so in multi mode this reads the org-scoped dir the Curator wrote
// rather than the org-stripped default (the curator's on-disk path
// was moved to be org-scoped).
//
// Degrades gracefully: a nil projectID, a missing knowledge-base dir,
// or per-file copy failures are logged but never fail the run.
func materializeProjectKnowledge(orgID, cwd string, projectID *string) {
	dir := filepath.Join(cwd, "_scratch", "project-knowledge")
	if err := os.MkdirAll(dir, 0755); err != nil {
		delegateLog.Warn("create project-knowledge dir failed", "path", dir, "error", err)
		return
	}

	if projectID == nil || *projectID == "" {
		return
	}

	if _, err := paths.StateRootErr(); err != nil {
		delegateLog.Warn("resolve knowledge dir for project failed", "project", *projectID, "error", err)
		return
	}
	kbRoot := paths.ProjectKBDir(orgID, *projectID)
	srcDir := filepath.Join(kbRoot, "knowledge-base")

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if !os.IsNotExist(err) {
			delegateLog.Warn("read project knowledge-base failed", "path", srcDir, "error", err)
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
			delegateLog.Warn("copy project knowledge file failed", "src", src, "dst", dst, "error", err)
			continue
		}
		written++
		totalBytes += n
	}

	if totalBytes > projectKnowledgeWarnBytes {
		delegateLog.Warn("project knowledge-base over the soft cap; consider trimming", "project", *projectID, "bytes", totalBytes, "soft_cap", projectKnowledgeWarnBytes)
	}
	if written > 0 {
		delegateLog.Info("materialized project-knowledge files for project", "count", written, "project", *projectID)
	}
}
