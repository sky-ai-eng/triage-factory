// Run-memory file reading — mirrored into conversation_memory as the agent
// writes it and read once more at termination — and the cross-run task-memory
// materializer a fresh agent invocation reads as ambient context. (The
// completion envelope's bounded re-prompt-to-fix lives on the live driver —
// see live.go.)

package delegate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/memoryentities"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// maxCompletionRetries is the hard cap on how many times the live driver
// re-prompts a run to fix an invalid completion envelope (malformed JSON, an
// unrecognized outcome, or a recognized outcome missing its required
// companion field) before failing it. Three gives a model that fumbled the
// contract real chances to correct without spending unbounded turns on one
// that's ignoring it. Not a config knob because no one needs to tune it
// per-run. A turn that ends with NO envelope attempt is not retried — the run
// is left open (see driveLiveConversation). The live driver's results-channel buffer is
// sized off this (resultsBufferDepth in live.go); keep that in mind if you bump
// it.
const maxCompletionRetries = 3

// workspaceKey is the key that groups everything the conversations of one task
// share: the run tree on disk and its workspace snapshot blob. It is the task
// id, so a second delegation on a task continues in the tree the first one
// left rather than cloning a fresh checkout under a key nothing else looks
// under.
//
// It stays a named function over an identity return because the name is what
// tells a call site which of the several ids in scope keys the workspace: a
// conversation id or a blueprint run id reads as an equally plausible argument
// and silently keys a tree nothing else will look under.
func workspaceKey(taskID string) string {
	return taskID
}

// memoryFileState distinguishes the reasons readAgentMemoryFile returns no
// usable content. They all mean the same thing to the gate — no
// conversation_memory row is written, so the conversation reads as one whose
// agent never wrote — but each carries different diagnostic value when
// something looks wrong post-run, so the gate teardown logs them distinctly.
type memoryFileState int

const (
	memoryFilePresent memoryFileState = iota // file exists, has non-whitespace content
	memoryFileMissing                        // file does not exist on disk
	memoryFileEmpty                          // file exists but is empty / whitespace-only
	memoryFileReadErr                        // file exists, read failed (permissions, race, etc.)
	memoryFileStale                          // file exists, holding byte-for-byte what this run inherited
)

// Layout of the memory tree the orchestrator owns inside a run root.
//
// The agent writes exactly one file, at a fixed path it never has to assemble:
// _tfac/memory.md. The orchestrator reads it at termination, where it
// already knows which conversation and which workflow run it belongs to, and
// files it into conversation_memory under those ids.
//
// What the orchestrator materializes for the agent to READ lives under
// _tfac/entity-memory/, split by relevance and named for a human:
//
//	this-run/01-triage.md          earlier steps of the current workflow run
//	this-run/02-implement.md
//	history/2026-07-20-ci-fix.md   prior, separate runs on this entity
//
// That path is what the AGENT sees. Where those files physically live depends on
// who owns the run tree: local mode writes them in it, a sandboxed launch stages
// them outside and mounts them there read-only (entityMemoryTarget).
const (
	scratchDirName       = worktree.ScratchDir
	agentMemoryFileName  = "memory.md"
	entityMemoryDirName  = worktree.EntityMemoryDir
	currentRunDirName    = "this-run"
	taskContextFileName  = "task-context.md"
	priorRunsDirName     = "history"
	memorySlugMaxLen     = 32
	historyDateLayoutUTC = "2006-01-02"
)

// readAgentMemoryFile returns the agent-written ./_tfac/memory.md content
// along with a state classification. The content string is empty for every
// non-Present state — only Present is worth filing, and the state is what
// callers log so every form of noncompliance doesn't collapse to the same
// line. Read errors that aren't a missing file are logged at the read site so
// they aren't lost when the caller picks a higher-level message.
func readAgentMemoryFile(cwd string) (string, memoryFileState) {
	path := agentMemoryFilePath(cwd)
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

// agentMemoryFilePath is the one path the agent writes its memory to, and the
// one path the orchestrator reads it back from.
func agentMemoryFilePath(cwd string) string {
	return filepath.Join(cwd, scratchDirName, agentMemoryFileName)
}

// memoryFingerprint identifies the memory file a run INHERITED at the fixed
// write path, so its own termination can tell "the agent wrote this" from "the
// previous step left it there".
//
// It is a digest of the file's CONTENT, not its metadata. Metadata cannot answer
// this question: a rewrite to the same length inside one timestamp quantum — and
// mtime resolution is a filesystem property, not something this code can bound —
// is indistinguishable from an untouched file by (size, mtime), and the cost of
// getting it wrong falls on the wrong side. A step whose real memory is
// misclassified has its work silently discarded; a digest gets that case right
// by construction.
//
// The residual ambiguity is a step that writes content byte-identical to what it
// inherited, which reads as untouched. Nothing is lost when that happens: the
// narrative that would have been ingested is the one already recorded.
//
// Hashing is available here for the same reason this type has to exist at all:
// the orchestrator can still stat and READ inside a run tree handed to the
// sandbox uid, it just cannot write. Deleting the inherited file is the simpler
// answer and is what clearAgentMemoryFile does whenever it can — but a warm
// blueprint step cannot delete anything in its tree, and its predecessor's
// memory is sitting at the path it is about to be judged on.
type memoryFingerprint struct {
	sum [sha256.Size]byte
}

// fingerprintAgentMemoryFile digests the memory file a run is starting with.
// Returns nil when there is nothing at the path (the common case, and the only
// one on a cold tree), when it is not a regular file, or when it cannot be read
// — all of which mean "nothing to distrust", so the run's own file is taken at
// face value exactly as on the paths that carry no fingerprint at all.
//
// Streamed rather than read whole: the agent chooses this file's size, and a
// pre-launch bookkeeping step has no business holding an arbitrary amount of it
// in the orchestrator's heap.
func fingerprintAgentMemoryFile(cwd string) *memoryFingerprint {
	f, err := os.Open(agentMemoryFilePath(cwd))
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		delegateLog.Warn("fingerprint inherited memory file failed; this conversation's memory will be taken at face value", "cwd", cwd, "error", err)
		return nil
	}
	var fp memoryFingerprint
	h.Sum(fp.sum[:0])
	return &fp
}

// covers reports whether content is the very file this fingerprint recorded —
// i.e. the agent never wrote. A nil fingerprint covers nothing: there was no
// inherited file, so whatever is there now is this run's.
func (f *memoryFingerprint) covers(content string) bool {
	return f != nil && sha256.Sum256([]byte(content)) == f.sum
}

// readConversationMemory returns what THIS run wrote at the fixed path. Content identical
// to what the run inherited is not this run's work: it reads as "wrote nothing",
// so no conversation_memory row is written rather than one adopting a
// predecessor's narrative as this conversation's own.
//
// prior is nil on every path with no inherited file to distrust — a resume,
// whose own file is its work, and a fresh tree.
func readConversationMemory(cwd string, prior *memoryFingerprint) (string, memoryFileState) {
	content, state := readAgentMemoryFile(cwd)
	if state == memoryFilePresent && prior.covers(content) {
		return "", memoryFileStale
	}
	return content, state
}

// memoryMirror files the agent's memory file into conversation_memory as the
// agent writes it, instead of only at the engagement's own ending.
//
// Without it the file is durable at two moments: the completion gate ingests
// it, and each workspace snapshot carries it. Between those it exists on one
// executor's disk and nowhere else — so a conversation a handler ends
// mid-flight has whatever the agent last wrote on a machine the handler cannot
// read, and in multi mode it is not even the same pod. Mirroring makes the row
// track the file: one read per tool call, bounded by the file the agent chose
// to write.
//
// One per engagement, holding exactly what the engagement knows and a later
// check cannot re-derive: where the tree is, what the file held when this
// engagement inherited it, and the digest of the content last filed — which is
// what keeps a run that never touches the file from re-filing the same bytes
// on every tool call. That digest is an optimization for the mid-run look
// only; an ending settles unconditionally (see settle).
//
// Advisory throughout. A read error, a refused write, a spawner with no memory
// store: each logs and returns, and the run is untouched. The gates that call
// check last go on to write exactly what they wrote before.
type memoryMirror struct {
	memory         db.TaskMemoryStore
	orgID          string
	conversationID string
	blueprintRunID string
	// entityID is the task's primary entity — the join row every entity read
	// reaches this memory through. Empty where the conversation has no task
	// entity, which files the row and attaches nothing.
	entityID  string
	cwd       string
	inherited *memoryFingerprint

	// mu serializes the filings. Each hook point is single-threaded on its
	// own, but an ending's final check runs on the goroutine driving the
	// engagement while the SDK's sink still sits on the process reader's, so
	// the two can meet on the last tool row of a run.
	mu    sync.Mutex
	filed *[sha256.Size]byte
	// attached records that the primary join row landed. Tracked apart from
	// filed because the two writes fail independently and the join row does
	// not depend on the content: it is keyed (conversation, entity), so one
	// success covers every later filing and a failure has to be retried on a
	// later look the content digest would otherwise skip entirely.
	attached bool
}

// newMemoryMirror builds the mirror for one engagement.
//
// inherited is the fingerprint of the file the engagement started with — nil
// wherever there was nothing to distrust — and is the same value the
// completion gate has always judged the file against, so the mirror and the
// gate agree on whose work the file is by construction rather than by two
// readings of one rule.
func (s *Spawner) newMemoryMirror(orgID, conversationID, blueprintRunID, entityID, cwd string, inherited *memoryFingerprint) *memoryMirror {
	return &memoryMirror{
		memory:         s.taskMemory,
		orgID:          orgID,
		conversationID: conversationID,
		blueprintRunID: blueprintRunID,
		entityID:       entityID,
		cwd:            cwd,
		inherited:      inherited,
	}
}

// check is the mid-run look, one per tool call: it files the memory file when
// the agent has written something new and does nothing when it has not.
//
// Nothing is filed for a file that is absent, empty, unreadable, or still
// byte-for-byte what this engagement inherited — the four answers the
// completion gate has always refused to ingest — nor for content this mirror
// has already filed. A nil mirror answers memoryFileMissing, which is the
// honest reading: a caller with no tree yet has no file this conversation may
// claim.
func (m *memoryMirror) check(ctx context.Context) memoryFileState {
	return m.file(ctx, false)
}

// settle is the look an ENDING takes — a park, a conclusion, a failure — and
// it writes whatever the file says even when this mirror filed those exact
// bytes already.
//
// The skip check is in-memory state about what THIS mirror wrote, not a read
// of the row, so it cannot see a row someone else changed. That someone is
// real: the memory upsert is keyed by conversation and is not claim-fenced, so
// a zombie engagement's own ending files into a row its successor now owns.
// Under the mid-run rule the successor would then never correct it — its
// digest still matches its own unchanged file — and the conversation would
// end holding the loser's narrative.
//
// Writing unconditionally at the ending is what closes that, and it restores
// the property the completion gate had before this mirror existed and that is
// far easier to reason about than any digest rule: at an ending, the row is
// what the file says. The cost is one redundant upsert per ending.
func (m *memoryMirror) settle(ctx context.Context) memoryFileState {
	return m.file(ctx, true)
}

// file is check and settle's shared body; force is what settle adds.
func (m *memoryMirror) file(ctx context.Context, force bool) memoryFileState {
	if m == nil || m.memory == nil {
		return memoryFileMissing
	}
	content, state := readConversationMemory(m.cwd, m.inherited)
	if state != memoryFilePresent {
		return state
	}
	sum := sha256.Sum256([]byte(content))

	m.mu.Lock()
	defer m.mu.Unlock()

	// Detached from the caller's cancellation for the reason the gate's own
	// upsert is: a tool call that resolves as the engagement is stopped still
	// wrote what it wrote, and this row is the only durable copy of it.
	bgCtx := context.WithoutCancel(ctx)

	// The content, unless this mirror already filed these exact bytes. An
	// ending forces it (see settle).
	if force || m.filed == nil || *m.filed != sum {
		if _, err := m.memory.UpsertAgentMemorySystem(bgCtx, m.orgID, m.conversationID, m.blueprintRunID, content, domain.MemorySourceAgent); err != nil {
			delegateLog.Warn("mirror the agent's memory file failed", "conversation", m.conversationID, "error", err)
			return state
		}
		m.filed = &sum
	}

	// The primary join row, until it lands — an entity read reaches this
	// memory through it and nowhere else, so a row filed without one is
	// durable and invisible. Its own flag, not the content's: a failure here
	// has to be retried on a later look, and every later look sees the same
	// unchanged file the digest above just skipped. The gate's own
	// attachConversationMemoryEntities is no backstop for it either — that
	// runs at a conclusion, and a conversation ended at a boundary never
	// reaches one.
	if m.entityID == "" || m.attached {
		return state
	}
	if err := m.memory.RecordEntityTouchSystem(bgCtx, m.orgID, m.conversationID, m.entityID, domain.MemoryRolePrimary); err != nil {
		delegateLog.Warn("attach primary entity to mirrored conversation memory failed; retrying on this engagement's next look", "conversation", m.conversationID, "entity", m.entityID, "error", err)
		return state
	}
	m.attached = true
	return state
}

// afterToolCall is the native loop's AfterToolCall hook: mirror, then hand the
// outcome back exactly as dispatched. The hook's rewrite power is deliberately
// unused — the mirror observes the run, it never shapes what the model reads.
func (m *memoryMirror) afterToolCall(ctx context.Context, _ domain.ToolCall, out agentloop.ToolOutcome) agentloop.ToolOutcome {
	m.check(ctx)
	return out
}

// repoFiles is the set of paths under the scratch dir that belong to the REPO,
// not to TF — what git tracks there, slash-separated and repo-relative. For a
// GitHub PR run the run tree IS the repo checkout, and .git/info/exclude does
// nothing for an already-tracked path, so every infrastructure write and delete
// in the tree consults this first: a mutation TF makes to a tracked file rides
// the agent's next `git add -A` straight into its PR.
//
// The zero value permits everything, which is the honest answer for a run root
// that is no repo at all (Jira, taskless) and for the overwhelmingly common repo
// that tracks nothing under our directory.
type repoFiles map[string]bool

// owns reports whether the repo — not TF — owns the scratch-relative path parts.
func (r repoFiles) owns(parts ...string) bool {
	if len(r) == 0 {
		return false
	}
	return r[path.Join(append([]string{scratchDirName}, parts...)...)]
}

// scanRepoFiles asks git what the repo tracks under the scratch dir and warns
// when the answer isn't "nothing" — a repo that stores files there and an agent
// run that treats the directory as its own are on a collision course TF can
// only half-prevent: it can refuse to touch those paths, but the agent writes
// its own files and may still bury one.
func scanRepoFiles(ctx context.Context, cwd string) repoFiles {
	tracked := worktree.TrackedUnder(ctx, cwd, scratchDirName)
	if len(tracked) > 0 {
		names := make([]string, 0, len(tracked))
		for p := range tracked {
			names = append(names, p)
		}
		sort.Strings(names)
		delegateLog.Warn("repo tracks files under the agent scratch directory; leaving them untouched (an agent writing there may still dirty them)",
			"cwd", cwd, "paths", strings.Join(names, ", "))
	}
	return tracked
}

// clearAgentMemoryFile drops any memory file already sitting at the fixed write
// path when a fresh run starts in the tree. The steps of one blueprint run share
// a worktree and every step writes the same filename, so a step that terminates
// without writing must not have its predecessor's memory ingested as its own —
// a source='agent' row is a claim that THIS conversation wrote it. Called only
// when the run is starting a new conversation in the tree: a resumed run's own
// file is its work, not a leftover.
//
// A repo-owned path is left alone: the collision costs this run's memory
// attribution, which is worth strictly less than the user's committed file.
//
// Best-effort otherwise. A failure leaves a stale file, which is the state a run
// that skipped this would have had anyway.
func clearAgentMemoryFile(cwd string, owned repoFiles) {
	if owned.owns(agentMemoryFileName) {
		return
	}
	file := filepath.Join(cwd, scratchDirName, agentMemoryFileName)
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		delegateLog.Warn("clear stale memory file failed", "path", file, "error", err)
	}
}

// materializePriorMemories renders any existing conversation_memory rows for the
// entity as individual markdown files under the agent's _tfac/entity-memory/, so
// a fresh agent invocation sees what previous iterations on the same task have
// already tried — and so the later steps of one blueprint run read the earlier
// steps' memory as their handoff.
//
// root is the directory the layout is rendered into — the agent's
// _tfac/entity-memory in local mode, this launch's staging dir under a sandbox.
// The caller resolves it (entityMemoryTarget); this function is indifferent to
// which, and to whether the tree it will be read from is still writable.
//
// blueprintRunID is the CURRENT run's workflow run. Memory produced under it is
// this run's own handoff and lands in this-run/, numbered by step so the
// listing reads in execution order; everything else is history and lands in
// history/, dated. Both names are chosen here, from what the row already
// carries — no id an agent could mistype appears in either the tree or the
// prompt that describes it.
//
// Both folders are created unconditionally, even on the very first run when
// there are no priors: the prompt tells the agent to look in them early, and a
// missing directory turns that into noise.
//
// Pattern: DB is the source of truth, we materialize before each launch and
// ingest back on completion. Both the worktree and the staging dir are destroyed
// after their run, so these files never outlive it on disk — only the DB rows do.
//
// Degrades gracefully: database errors, mkdir failures, or per-file
// write failures are logged but do not fail the run. An agent running
// without materialized priors is still useful, just without the
// cross-run memory benefit. This "advisory" posture only holds for
// the read side — the write-before-finish gate is enforced separately
// for NEW memories produced during the run. It extends to a repo-owned
// target: a name that collides with a tracked file yields that file to the
// repo and skips the prior, rather than overwriting content the agent would
// then commit.
func materializePriorMemories(taskMemory db.TaskMemoryStore, orgID, teamID, root, entityID, blueprintRunID string, owned repoFiles) {
	thisRunDir := filepath.Join(root, currentRunDirName)
	historyDir := filepath.Join(root, priorRunsDirName)
	for _, dir := range []string{thisRunDir, historyDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			delegateLog.Warn("create entity-memory dir failed", "path", dir, "error", err)
			return
		}
	}

	// teamID is THIS conversation's owning team. The System read scopes the
	// prior memory to what that team can see, so a conversation never
	// materializes another team's conversation narratives on a shared entity
	// (TFAC-506).
	memories, err := taskMemory.GetMemoriesForEntitySystem(context.Background(), orgID, entityID, teamID)
	if err != nil {
		delegateLog.Warn("load prior memories for entity failed", "entity", entityID, "error", err)
		return
	}
	if len(memories) == 0 {
		return
	}

	var thisRun, history []domain.TaskMemory
	for _, m := range memories {
		if blueprintRunID != "" && m.BlueprintRunID == blueprintRunID {
			thisRun = append(thisRun, m)
			continue
		}
		history = append(history, m)
	}
	// The rows arrive oldest-first, which is already step order for a workflow
	// run that ran its steps in sequence. Sorting on the recorded step index
	// makes that explicit and survives a step whose memory landed out of
	// created_at order; a row with no index keeps its arrival position.
	sort.SliceStable(thisRun, func(i, j int) bool {
		a, b := thisRun[i].StepIndex, thisRun[j].StepIndex
		if a == nil || b == nil {
			return false
		}
		return *a < *b
	})

	written := 0
	used := map[string]bool{}
	for i, m := range thisRun {
		ordinal := i + 1
		if m.StepIndex != nil {
			ordinal = *m.StepIndex + 1
		}
		name := uniqueMemoryFileName(used, fmt.Sprintf("%02d", ordinal), memorySlug(m.PromptName))
		if owned.owns(entityMemoryDirName, currentRunDirName, name) {
			continue
		}
		if writeMemoryFile(filepath.Join(thisRunDir, name), m.Content) {
			written++
		}
	}
	used = map[string]bool{}
	for _, m := range history {
		name := uniqueMemoryFileName(used, m.CreatedAt.UTC().Format(historyDateLayoutUTC), memorySlug(m.PromptName))
		if owned.owns(entityMemoryDirName, priorRunsDirName, name) {
			continue
		}
		if writeMemoryFile(filepath.Join(historyDir, name), m.Content) {
			written++
		}
	}
	if written > 0 {
		delegateLog.Info("materialized prior memories for entity", "count", written, "entity", entityID)
	}
}

// writeTaskContextFile retains this launch's rendered <task_context> as a file
// the agent can re-read, at the fixed name every prompt block names it by.
//
// It is the task context's whole retention. Nothing is pinned through a
// compaction, so the row carrying these bytes is summarized like any other —
// and a summary is the model's restatement, which is the right posture for
// externally-authored text but loses the PR number the agent needs an hour
// later. The file is the original, addressed by a path rather than re-sent.
//
// root is entityMemoryTarget's answer, which is why the file sits one directory
// inside the memory tree rather than beside it in _tfac/: on a warm, handed-off
// step the run tree belongs to the sandbox identity and TF may not write there,
// while the staged memory dir is the one per-launch location it still owns. In
// local mode that same path is a real directory in the tree.
//
// Best-effort, like the prior memories rendered next to it: an agent that
// cannot re-read the original still has the summary, and a run must not fail
// for a file it may never open. A repo-owned name yields to the repo for the
// reason every other write here does — the collision costs a re-read, the
// user's committed file is worth more.
func writeTaskContextFile(root, taskContext string, owned repoFiles) {
	if root == "" || strings.TrimSpace(taskContext) == "" {
		return
	}
	if owned.owns(entityMemoryDirName, taskContextFileName) {
		delegateLog.Warn("repo tracks the task-context path; this run's task context is not retained as a file", "path", filepath.Join(root, taskContextFileName))
		return
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		delegateLog.Warn("create the task-context directory failed; this run's task context is not retained as a file", "path", root, "error", err)
		return
	}
	file := filepath.Join(root, taskContextFileName)
	if err := os.WriteFile(file, []byte(taskContext+"\n"), 0644); err != nil {
		delegateLog.Warn("write the task-context file failed; a compacted run cannot re-read its task context", "path", file, "error", err)
	}
}

// entityMemoryTarget resolves where THIS launch's prior-memory tree is
// rendered, and records the staging path on cfg when the launch will mount it.
// Returns the target directory plus the repo-owned set that applies to it.
//
// The branch is the sandbox gate, not the run mode: a jailed agent reads its
// handoff from a read-only bind mount of an orchestrator-owned staging dir, keyed
// by its own conversation id, because nothing TF writes may touch the run tree
// after its first launch — and every blueprint step but the first launches into a
// tree that was already handed off. An un-jailed run keeps writing the real
// directory inside the tree it owns.
//
// The repo-owned set only travels with the in-tree target. A staging dir is TF's
// outright, is no repo, and shares no path with one.
func entityMemoryTarget(cfg *runConfig, conversationID, cwd string, owned repoFiles) (string, repoFiles) {
	if !agentproc.WillSandbox() {
		return filepath.Join(cwd, scratchDirName, entityMemoryDirName), owned
	}
	dir := sandbox.TrustedMemorySourcePath(conversationID)
	cfg.memorySourcePath = dir
	return dir, nil
}

// stagedEntityMemorySource returns conversationID's memory staging dir when one is still
// on disk, else "". A resume re-invokes the agent in the same conversation and
// runs none of the per-launch setup, so it re-mounts whatever its original claim
// materialized rather than re-rendering it. Absent — a cold resume on an executor
// that never staged it, or after a startup sweep — the resumed agent continues
// from its transcript with nothing behind the symlink; the mount is ambient
// context, not the conversation's state.
func stagedEntityMemorySource(conversationID string) string {
	if conversationID == "" || !agentproc.WillSandbox() {
		return ""
	}
	dir := sandbox.TrustedMemorySourcePath(conversationID)
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// removeStagedMemory drops a launch's memory staging dir. A missing directory is
// success. Safe for the capability-less orchestrator: the staging dir is its own
// and is never chowned to a sandbox identity — that is the point of staging
// outside the run tree.
func removeStagedMemory(dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		delegateLog.Warn("remove staged entity memory failed", "path", dir, "error", err)
	}
}

// writeMemoryFile writes one materialized memory, reporting whether it landed.
// A failure is logged and skipped — the read side is advisory.
func writeMemoryFile(path, content string) bool {
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		delegateLog.Warn("materialize task memory failed", "path", path, "error", err)
		return false
	}
	return true
}

// uniqueMemoryFileName composes "<prefix>-<slug>.md" (or "<prefix>.md" when the
// producing prompt is unknown) and disambiguates against names already written
// to the same folder. Collisions are possible in both folders — two runs of one
// prompt on the same day, a re-run of a step — and a silently overwritten file
// would lose a prior narrative, so the loser gets a numeric suffix instead.
func uniqueMemoryFileName(used map[string]bool, prefix, slug string) string {
	base := prefix
	if slug != "" {
		base += "-" + slug
	}
	name := base + ".md"
	for n := 2; used[name]; n++ {
		name = fmt.Sprintf("%s-%d.md", base, n)
	}
	used[name] = true
	return name
}

// memorySlug renders a prompt name as a filename fragment: lowercase, ASCII
// alphanumerics only, single dashes between words, bounded in length. Returns
// "" for a name with nothing usable in it, which drops the suffix entirely
// rather than leaving a dangling dash.
func memorySlug(promptName string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(promptName) {
		if !isSlugChar(r) {
			dash = true
			continue
		}
		// Every byte this loop writes is ASCII, so the cost of taking this
		// character is one byte plus the separator it may need. Charge it
		// before writing: a check after the fact would let a word boundary
		// land the pair past the cap.
		cost := 1
		if dash && b.Len() > 0 {
			cost = 2
		}
		if b.Len()+cost > memorySlugMaxLen {
			break
		}
		if cost == 2 {
			b.WriteByte('-')
		}
		dash = false
		b.WriteByte(byte(r))
	}
	return strings.Trim(b.String(), "-")
}

// isSlugChar reports whether r survives into a slug as itself. Everything else
// — punctuation, whitespace, any non-ASCII letter — reads as a word boundary,
// which is what keeps a slug's byte count equal to its character count.
func isSlugChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// attachConversationMemoryEntities makes a terminated conversation's memory reachable
// from every entity it materially engaged. The rule itself lives in
// internal/memoryentities, where it is reachable by anything that writes a
// conversation's memory row; this is the spawner's stores bound to it.
func (s *Spawner) attachConversationMemoryEntities(ctx context.Context, orgID, conversationID, primaryEntityID string) {
	memoryentities.Attach(ctx, s.taskMemory, s.artifacts, s.entities, orgID, conversationID, primaryEntityID)
}
