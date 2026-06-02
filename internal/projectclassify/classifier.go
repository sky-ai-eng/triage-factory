// Package projectclassify decides which project (if any) a newly-
// discovered entity belongs to. SKY-220.
//
// Two-stage design:
//
//	Stage 1 — broad pass, fast.
//	  Per project, parallel, single-shot Haiku. Prompt inlines name +
//	  description + KB content truncated at kbInlineMaxBytes. Returns
//	  {score, rationale, kb_truncated}. ~1 model turn per project.
//
//	Stage 2 — selective deepening, only when warranted.
//	  Trigger: no Stage 1 winner above ConfidenceThreshold AND ≥1
//	  project scored borderlineMin..ConfidenceThreshold with a
//	  truncated KB. Re-classify all such projects in agent-mode:
//	  cmd.Dir = curator.KnowledgeDir(orgID, project.ID), --max-turns 6,
//	  prompt directs at ./knowledge-base/ via Read/Glob/Grep. The
//	  agent may take 3-8 model turns to selectively read what's
//	  relevant.
//
// Single-entity-per-call rather than batched. Discoveries are rare
// (a few per poll cycle at most) and each call already inlines the
// per-project context, so batching across entities would just
// duplicate context.
package projectclassify

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:embed prompts/classify_stage1.txt
var stage1Prompt string

//go:embed prompts/classify_stage2.txt
var stage2Prompt string

// ConfidenceThreshold is the minimum Haiku score (0-100) required to
// auto-assign an entity to a project. Below this, project_id stays
// NULL and the entity resurfaces in future project-creation backfill
// popups. 60 is a launch default; tune from real votes.
const ConfidenceThreshold = 60

// borderlineMin is the lower bound on Stage 1 scores that qualify for
// Stage 2 escalation. A project that scored 40-59 with a truncated KB
// might have crossed threshold if Haiku had been able to see the
// whole KB; below 40, escalation is unlikely to flip the outcome and
// the agent loop isn't worth the cost.
const borderlineMin = 40

// classifyModel mirrors the scorer's model choice — fast and cheap, and
// we want consistent Haiku-version drift across the two AI surfaces.
const classifyModel = "haiku"

// maxConcurrentVotes caps the number of in-flight Haiku calls per
// stage. Most installs have <10 projects so the cap rarely fires;
// the bound exists to avoid swamping the local `claude` CLI on
// pathological setups.
const maxConcurrentVotes = 8

// stage2MaxTurns caps how many tool-using turns the Stage 2 agent
// gets. 6 is enough for "Glob to see the layout, Read 2-3 files,
// answer" — anything more is the model wandering, which we'd rather
// abort than pay for.
const stage2MaxTurns = 6

// entityDescriptionMaxLen mirrors the scorer's truncation policy
// (internal/ai/scorer.go:descriptionMaxLen). The classifier prompt
// already includes title; the description is supplementary context
// that doesn't need to be unbounded.
const entityDescriptionMaxLen = 1500

// kbInlineMaxBytes caps the per-project knowledge-base content sent
// inline to Stage 1. Curator-written KBs typically fit easily; the
// cap exists for the pathological case where a user dumps a large
// reference document. Above this we truncate with a sentinel and let
// Stage 2 escalate if the score lands borderline.
const kbInlineMaxBytes = 30 * 1024

// Vote is the per-project result of one classification call.
type Vote struct {
	ProjectID   string
	Score       int
	Rationale   string
	KBTruncated bool // Stage 1 only: signals Stage 2 escalation candidacy.
	Stage       int  // 1 or 2 — useful for logging which path produced a Vote.
	Err         error
}

// Classify runs the per-project quorum vote for one entity. Returns
// the winning project_id (or nil if all votes are below threshold)
// plus the merged per-project vote slice (Stage 2 vote replaces
// Stage 1 vote for any project that re-classified).
//
// Stage 2 fires only when Stage 1 has no winner AND ≥1 borderline-
// truncated vote exists. In the common case (clear winner OR clearly
// nobody fits), Stage 2 is skipped and the cost stays at
// 1 turn × N projects.
//
// orgID scopes the per-org credentials agentproc.Run resolves for
// each Haiku invocation. The Runner's per-org loop supplies this
// from ListActiveSystem; passing the wrong org would bill the wrong
// tenant in multi-mode. secrets is the per-org LLM-credential reader
// threaded alongside orgID (nil in local → ambient subscription;
// system-door reader in multi). SKY-389.
func Classify(ctx context.Context, orgID string, secrets agentproc.SecretsReader, projects []domain.Project, entity domain.Entity) (*string, []Vote) {
	if len(projects) == 0 {
		return nil, nil
	}

	stage1 := runVotes(ctx, orgID, secrets, projects, entity, voteStage1)

	if winner := pickWinner(stage1); winner != nil {
		return winner, stage1
	}

	// Find projects that are borderline AND ran into the inline KB cap.
	// Anything not truncated already had its full KB at Stage 1, so re-
	// running with filesystem access wouldn't change the answer.
	var escalated []domain.Project
	for i, v := range stage1 {
		if v.Err != nil {
			continue
		}
		if v.Score < borderlineMin || v.Score >= ConfidenceThreshold {
			continue
		}
		if !v.KBTruncated {
			continue
		}
		escalated = append(escalated, projects[i])
	}
	if len(escalated) == 0 {
		return nil, stage1
	}

	log.Printf("[classify] entity %s: escalating %d borderline+truncated project(s) to Stage 2", entity.ID, len(escalated))
	stage2 := runVotes(ctx, orgID, secrets, escalated, entity, voteStage2)

	merged := mergeStages(stage1, stage2)
	return pickWinner(merged), merged
}

// runVotes fans out one Haiku call per project using the provided
// vote function (Stage 1 or Stage 2). Concurrency is capped at
// maxConcurrentVotes.
func runVotes(ctx context.Context, orgID string, secrets agentproc.SecretsReader, projects []domain.Project, entity domain.Entity, vote func(context.Context, string, agentproc.SecretsReader, domain.Project, domain.Entity) Vote) []Vote {
	sem := make(chan struct{}, maxConcurrentVotes)
	votes := make([]Vote, len(projects))
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, project domain.Project) {
			defer wg.Done()
			defer func() { <-sem }()
			votes[idx] = vote(ctx, orgID, secrets, project, entity)
		}(i, p)
	}
	wg.Wait()
	return votes
}

// mergeStages produces the final per-project vote slice: for any
// project that re-classified in Stage 2, that result wins; for the
// rest, the Stage 1 result stands. The returned slice has one entry
// per project from the Stage 1 input, in original order.
func mergeStages(stage1, stage2 []Vote) []Vote {
	stage2ByID := make(map[string]Vote, len(stage2))
	for _, v := range stage2 {
		stage2ByID[v.ProjectID] = v
	}
	merged := make([]Vote, len(stage1))
	for i, v := range stage1 {
		if s2, ok := stage2ByID[v.ProjectID]; ok {
			merged[i] = s2
		} else {
			merged[i] = v
		}
	}
	return merged
}

// pickWinner returns the highest-scoring above-threshold project_id,
// or nil if none qualify. Ties resolve to first-returned (slice order
// == iteration order from db.ListProjects, which is alphabetical by
// name). Per the SKY-220 spec, a tie-breaking heuristic is out of
// scope for v1.
func pickWinner(votes []Vote) *string {
	bestScore := -1
	var winner string
	for _, v := range votes {
		if v.Err != nil {
			continue
		}
		if v.Score < ConfidenceThreshold {
			continue
		}
		if v.Score > bestScore {
			bestScore = v.Score
			winner = v.ProjectID
		}
	}
	if bestScore < 0 {
		return nil
	}
	return &winner
}

// voteStage1 is the broad-pass single-shot Haiku call. KB inlined
// up to kbInlineMaxBytes; flag KBTruncated when the cap was hit so
// the orchestrator knows whether Stage 2 might help.
func voteStage1(ctx context.Context, orgID string, secrets agentproc.SecretsReader, project domain.Project, entity domain.Entity) Vote {
	v := Vote{ProjectID: project.ID, Stage: 1}

	kb, truncated, err := readProjectKB(orgID, project.ID)
	if err != nil {
		log.Printf("[classify] project %s stage 1: KB read failed (%v) — voting with empty KB", project.ID, err)
		kb = ""
	}
	v.KBTruncated = truncated

	prompt := fmt.Sprintf(
		stage1Prompt,
		project.Name,
		project.Description,
		kb,
		entity.Source,
		entity.SourceID,
		entity.Kind,
		entity.Title,
		truncateDescription(entity.Description),
	)

	score, rationale, err := runStage1Haiku(ctx, orgID, prompt, secrets)
	if err != nil {
		v.Err = err
		return v
	}
	v.Score = score
	v.Rationale = rationale
	return v
}

// voteStage2 is the agent-mode call: Haiku in the project's working
// directory, free to Read/Glob/Grep `./knowledge-base/` selectively.
// Used only for borderline+truncated projects from Stage 1.
func voteStage2(ctx context.Context, orgID string, secrets agentproc.SecretsReader, project domain.Project, entity domain.Entity) Vote {
	v := Vote{ProjectID: project.ID, Stage: 2}

	kbRoot, err := curator.KnowledgeDir(orgID, project.ID)
	if err != nil {
		v.Err = fmt.Errorf("resolve project dir: %w", err)
		return v
	}
	if err := os.MkdirAll(kbRoot, 0o755); err != nil {
		v.Err = fmt.Errorf("ensure project dir: %w", err)
		return v
	}

	prompt := fmt.Sprintf(
		stage2Prompt,
		project.Name,
		project.Description,
		entity.Source,
		entity.SourceID,
		entity.Kind,
		entity.Title,
		truncateDescription(entity.Description),
	)

	score, rationale, err := runStage2Haiku(ctx, orgID, prompt, kbRoot, secrets)
	if err != nil {
		v.Err = err
		return v
	}
	v.Score = score
	v.Rationale = rationale
	return v
}

func truncateDescription(desc string) string {
	if len(desc) <= entityDescriptionMaxLen {
		return desc
	}
	return desc[:entityDescriptionMaxLen] + "\n…[truncated]"
}

// readProjectKB returns the concatenated content of every .md file
// under the project's knowledge-base/ directory that fits under the
// kbInlineMaxBytes budget. Files are read in lexical order so the
// same inputs produce the same prompt across runs.
//
// Files larger than the remaining budget are SKIPPED ENTIRELY rather
// than truncated mid-content — a half-paragraph fragment misleads the
// model more than a missing file does, and Stage 2 (which fires on
// truncated=true) will read the skipped files via filesystem tools.
// Smaller subsequent files in lex order are still inlined if they
// fit, so a project with one giant file + several small ones still
// gets the small ones in Stage 1.
//
// We Stat each file before reading so we never load oversized content
// into memory. truncated=true signals at least one file was skipped,
// which is the orchestrator's escalation trigger.
func readProjectKB(orgID, projectID string) (string, bool, error) {
	root, err := curator.KnowledgeDir(orgID, projectID)
	if err != nil {
		return "", false, err
	}
	kbDir := filepath.Join(root, "knowledge-base")
	entries, err := os.ReadDir(kbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var buf bytes.Buffer
	truncated := false
	for _, name := range names {
		full := filepath.Join(kbDir, name)
		info, err := os.Stat(full)
		if err != nil {
			log.Printf("[classify] project %s: stat KB file %s: %v", projectID, name, err)
			continue
		}
		headerOverhead := len("## ") + len(name) + len("\n\n") + len("\n\n")
		needed := buf.Len() + headerOverhead + int(info.Size())
		if needed > kbInlineMaxBytes {
			truncated = true
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			log.Printf("[classify] project %s: read KB file %s: %v", projectID, name, err)
			continue
		}
		fmt.Fprintf(&buf, "## %s\n\n%s\n\n", name, data)
	}
	if truncated {
		buf.WriteString("\n…[some knowledge-base files exceeded the inline cap and were skipped — Stage 2 will read selectively]")
	}
	return buf.String(), truncated, nil
}

// runStage1Haiku runs a single-shot Haiku classification through the
// shared agent runtime (agentproc.Run). Exposed as a var so tests can
// stub.
var runStage1Haiku = realRunStage1Haiku

func realRunStage1Haiku(ctx context.Context, orgID, prompt string, secrets agentproc.SecretsReader) (int, string, error) {
	return runHaiku(ctx, agentproc.RunOptions{
		Model:   classifyModel,
		Message: prompt,
		TraceID: "classify-stage1",
		OrgID:   orgID,
		Secrets: secrets,
	})
}

// runStage2Haiku runs in the project's KB directory so the model can
// use Read/Glob/Grep to inspect knowledge-base files selectively.
// MaxTurns is a hard cap on the tool loop. Exposed as a var so tests
// can stub.
var runStage2Haiku = realRunStage2Haiku

func realRunStage2Haiku(ctx context.Context, orgID, prompt, cwd string, secrets agentproc.SecretsReader) (int, string, error) {
	return runHaiku(ctx, agentproc.RunOptions{
		Cwd:      cwd,
		Model:    classifyModel,
		Message:  prompt,
		MaxTurns: stage2MaxTurns,
		TraceID:  "classify-stage2",
		OrgID:    orgID,
		Secrets:  secrets,
	})
}

// runHaiku drives one classification call through agentproc.Run with a
// NoopSink (no transcript persistence) and parses the {score, rationale}
// JSON the model emits. Stage 1 and Stage 2 share this path; they differ
// only in the RunOptions they pass. ctx propagates from the Runner's
// stop channel so server shutdown SIGKILLs in-flight calls instead of
// waiting for the model to time out.
func runHaiku(ctx context.Context, opts agentproc.RunOptions) (int, string, error) {
	outcome, err := agentproc.Run(ctx, opts, agentproc.NoopSink{})
	if err != nil {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		return 0, "", fmt.Errorf("classify agent failed: %w (stderr: %s)", err, stderr)
	}
	if outcome == nil || outcome.Result == nil {
		return 0, "", fmt.Errorf("classify agent: no terminal result event")
	}

	raw := ai.StripCodeFences([]byte(outcome.Result.Result))

	var resp struct {
		Score     int    `json:"score"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, "", fmt.Errorf("parse classify response: %w (raw: %s)", err, string(raw))
	}
	if resp.Score < 0 {
		resp.Score = 0
	}
	if resp.Score > 100 {
		resp.Score = 100
	}
	return resp.Score, resp.Rationale, nil
}
