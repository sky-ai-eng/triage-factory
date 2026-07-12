// Package projectclassify decides which project (if any) a newly-
// discovered entity belongs to.
//
// Single stage, toolless: per project, parallel, single-shot Haiku.
// Prompt inlines name + description + KB content truncated at
// kbInlineMaxBytes. Returns {score, rationale}. ~1 model turn per
// project. An exact tie for the top above-threshold score resolves to
// unassigned rather than guessing.
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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

//go:embed prompts/classify_stage1.txt
var stage1Prompt string

// ConfidenceThreshold is the minimum Haiku score (0-100) required to
// auto-assign an entity to a project. Below this, project_id stays
// NULL and the entity resurfaces in future project-creation backfill
// popups. 60 is a launch default; tune from real votes.
const ConfidenceThreshold = 60

// maxConcurrentVotes caps the number of in-flight Haiku calls per
// classification cycle. Most installs have <10 projects so the cap
// rarely fires; the bound exists to avoid swamping the local `claude`
// CLI on pathological setups.
const maxConcurrentVotes = 8

// entityDescriptionMaxLen mirrors the scorer's truncation policy
// (internal/ai/scorer.go:descriptionMaxLen). The classifier prompt
// already includes title; the description is supplementary context
// that doesn't need to be unbounded.
const entityDescriptionMaxLen = 1500

// kbInlineMaxBytes caps the per-project knowledge-base content sent
// inline to the classification prompt. Curator-written KBs typically
// fit easily; the cap exists for the pathological case where a user
// dumps a large reference document. Above this we truncate with a
// sentinel — the entity may still be classified from name +
// description + whatever KB content fit.
const kbInlineMaxBytes = 30 * 1024

// Vote is the per-project result of one classification call.
type Vote struct {
	ProjectID string
	Score     int
	Rationale string
	Err       error
}

// classify runs the per-project quorum vote for one entity. Returns
// the winning project_id (or nil if no vote clears ConfidenceThreshold,
// or if the top qualifying score is an exact tie between two or more
// projects) plus the per-project vote slice.
//
// The org context is r.orgID — it scopes the per-org credentials
// agentproc.Run resolves for each Haiku invocation. Billing the wrong
// tenant in multi-mode would mean a mis-routed Runner; the Manager keys
// runners by org so that can't happen. r.secrets is the per-org
// LLM-credential reader read off the receiver (nil in local → ambient
// subscription; system-door reader in multi).
func (r *Runner) classify(ctx context.Context, projects []domain.Project, entity domain.Entity) (*string, []Vote) {
	if len(projects) == 0 {
		return nil, nil
	}

	votes := r.runVotes(ctx, projects, entity, r.voteStage1)
	return pickWinner(votes, entity.ID), votes
}

// runVotes fans out one Haiku call per project using the provided
// vote method value. Concurrency is capped at maxConcurrentVotes — the
// inner per-entity cap; the shared system-job limiter (acquired inside
// runHaiku) is the outer global ceiling. The two compose without
// deadlock: a worker holds the inner slot while waiting on the outer
// one, but nothing holds the outer slot while waiting on the inner, so
// there's no hold-and-wait cycle.
func (r *Runner) runVotes(ctx context.Context, projects []domain.Project, entity domain.Entity, vote func(context.Context, domain.Project, domain.Entity) Vote) []Vote {
	sem := make(chan struct{}, maxConcurrentVotes)
	votes := make([]Vote, len(projects))
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Add(1)
		go func(idx int, project domain.Project) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			votes[idx] = vote(ctx, project, entity)
		}(i, p)
	}
	wg.Wait()
	return votes
}

// bestVotes finds the highest-scoring successful vote(s) that clear
// ConfidenceThreshold. score is -1 if none do. tied holds every vote
// at that score — length 1 in the normal case, length ≥2 on an exact
// tie. Shared by pickWinner and the runner's rationale logic so both
// agree on what "tied" means; a duplicated definition risks drifting
// out of sync with ConfidenceThreshold.
func bestVotes(votes []Vote) (score int, tied []Vote) {
	score = -1
	for _, v := range votes {
		if v.Err != nil || v.Score < ConfidenceThreshold {
			continue
		}
		switch {
		case v.Score > score:
			score = v.Score
			tied = []Vote{v}
		case v.Score == score:
			tied = append(tied, v)
		}
	}
	return score, tied
}

// pickWinner returns the highest-scoring above-threshold project_id,
// or nil if no vote clears ConfidenceThreshold. An exact tie for the
// top qualifying score means "can't tell" and also resolves to nil —
// never a coin-flip assignment, since project_id drives team
// visibility on the entity (see OwningTeamForEntitySystem).
func pickWinner(votes []Vote, entityID string) *string {
	score, tied := bestVotes(votes)
	if score < 0 {
		return nil
	}
	if len(tied) > 1 {
		ids := make([]string, len(tied))
		for i, v := range tied {
			ids[i] = v.ProjectID
		}
		classifyLog.Info("exact tie for top score, resolving to unassigned", "entity", entityID, "projects", ids, "score", score)
		return nil
	}
	winner := tied[0].ProjectID
	return &winner
}

// voteStage1 is the single-shot Haiku call. KB inlined up to
// kbInlineMaxBytes.
func (r *Runner) voteStage1(ctx context.Context, project domain.Project, entity domain.Entity) Vote {
	v := Vote{ProjectID: project.ID}

	kb, _, err := readProjectKB(r.orgID, project.ID)
	if err != nil {
		classifyLog.Warn("kb read failed, voting with empty kb", "project", project.ID, "error", err)
		kb = ""
	}

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

	score, rationale, err := r.stage1Fn(ctx, r.orgID, prompt)
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
// model more than a missing file does. Smaller subsequent files in
// lex order are still inlined if they fit, so a project with one
// giant file + several small ones still gets the small ones in the
// prompt.
//
// We Stat each file before reading so we never load oversized content
// into memory. truncated=true signals at least one file was skipped.
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
			classifyLog.Warn("stat kb file failed", "project", projectID, "file", name, "error", err)
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
			classifyLog.Warn("read kb file failed", "project", projectID, "file", name, "error", err)
			continue
		}
		fmt.Fprintf(&buf, "## %s\n\n%s\n\n", name, data)
	}
	if truncated {
		buf.WriteString("\n…[some knowledge-base files exceeded the inline cap and were skipped]")
	}
	return buf.String(), truncated, nil
}

// realRunStage1Haiku runs a single-shot Haiku classification through the
// shared agent runtime (agentproc.Run). It is the default value of the
// Runner's stage1Fn seam (set in NewRunner); tests swap stage1Fn for a stub.
// It reads the per-org credentials off the receiver.
func (r *Runner) realRunStage1Haiku(ctx context.Context, orgID, prompt string) (int, string, error) {
	return r.runHaiku(ctx, agentproc.RunOptions{
		Model:   ai.SystemJobModel,
		Message: prompt,
		TraceID: "classify-stage1",
		OrgID:   orgID,
		Secrets: r.secrets,
	})
}

// runHaiku drives one classification call through agentproc.Run with a
// NoopSink (no transcript persistence) and parses the {score, rationale}
// JSON the model emits. ctx propagates from the Runner's stop channel so
// server shutdown SIGKILLs in-flight calls instead of waiting for the
// model to time out.
func (r *Runner) runHaiku(ctx context.Context, opts agentproc.RunOptions) (int, string, error) {
	// Bound concurrent background sandboxes across all orgs + jobs. This is
	// the outer global ceiling; maxConcurrentVotes (in runVotes) is the inner
	// per-entity fan-out cap. Each vote acquires here, runs, and releases —
	// no held-resource cycle, so they compose without deadlock. A cancelled
	// ctx returns the error as the vote's Err. A nil limiter is an unlimited
	// no-op (used by tests).
	if err := r.limiter.Acquire(ctx); err != nil {
		return 0, "", err
	}
	defer r.limiter.Release()

	// UsageSink accumulates the per-message token breakdown so the run's
	// cost + tokens land in system_llm_runs; the terminal Result string is
	// still parsed below. One row per Run call (the classifier fans out
	// per-entity-per-project, so many rows per cycle — expected). Recording
	// happens whenever an outcome was produced and never breaks classification.
	startedAt := time.Now().UTC()
	usage := &agentproc.UsageSink{}
	outcome, err := agentproc.Run(ctx, opts, usage)
	r.recorder.Record(ctx, systemllm.Call{
		OrgID:     opts.OrgID,
		Job:       systemllm.JobClassifier,
		Model:     opts.Model,
		StartedAt: startedAt,
	}, outcome, usage)
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
