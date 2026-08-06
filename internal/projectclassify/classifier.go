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
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

//go:embed prompts/classify_stage1.txt
var stage1Prompt string

//go:embed prompts/classify_stage1_system.txt
var stage1SystemPrompt string

//go:embed prompts/classify_stage1_user.txt
var stage1UserPrompt string

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

// haikuPrompt bundles the local-mode combined message alongside the
// multi-mode system+user split. voteStage1 builds all three from the same
// project/entity/kb inputs so runHaiku (via systemllm.Complete) can pick
// the shape each mode needs without re-deriving one from the other — the
// data is sandwiched between two instruction sections in Message, which
// rules out reconstructing SystemPrompt/UserMessage from Message by simple
// string splitting.
type haikuPrompt struct {
	Message      string
	SystemPrompt string
	UserMessage  string
}

// voteStage1 is the single-shot Haiku call. KB inlined up to
// kbInlineMaxBytes.
func (r *Runner) voteStage1(ctx context.Context, project domain.Project, entity domain.Entity) Vote {
	v := Vote{ProjectID: project.ID}

	kb, _, err := r.readProjectKB(ctx, project.ID)
	if err != nil {
		classifyLog.Warn("kb read failed, voting with empty kb", "project", project.ID, "error", err)
		kb = ""
	}

	fields := []any{
		project.Name,
		project.Description,
		kb,
		entity.Source,
		entity.SourceID,
		entity.Kind,
		entity.Title,
		truncateDescription(entity.Description),
	}
	prompt := haikuPrompt{
		Message:      fmt.Sprintf(stage1Prompt, fields...),
		SystemPrompt: stage1SystemPrompt,
		UserMessage:  fmt.Sprintf(stage1UserPrompt, fields...),
	}

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
//
// Multi mode reads the same .md set from the blob store (control hosts no KB
// on disk); local mode keeps the byte-identical on-disk read.
func (r *Runner) readProjectKB(ctx context.Context, projectID string) (string, bool, error) {
	// One span per vote, and a vote runs per (entity, project) pair — so
	// multi mode makes N×M blob-store round trips per cycle before it can
	// even build a prompt. Local mode reads the same set off disk under the
	// same span, which is what makes the two comparable. The byte count
	// explains a slow read; the file names and contents are project data.
	ctx, span := tracer.Start(ctx, "projectclassify.read_kb")
	defer span.End()

	var (
		content   string
		truncated bool
		err       error
	)
	if r.kb != nil && runmode.Current() == runmode.ModeMulti {
		content, truncated, err = readProjectKBFromStore(ctx, r.kb, r.orgID, projectID)
	} else {
		content, truncated, err = readProjectKBFromDisk(r.orgID, projectID)
	}
	if err != nil {
		span.SetStatus(codes.Error, "read kb")
		return content, truncated, err
	}
	span.SetAttributes(
		telemetry.Count(len(content)),
		attribute.Bool("truncated", truncated),
	)
	return content, truncated, nil
}

func readProjectKBFromDisk(orgID, projectID string) (string, bool, error) {
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

// readProjectKBFromStore is the multi-mode counterpart of
// readProjectKBFromDisk: it concatenates the project's .md knowledge from the
// blob store under the same budget and header format, so classifier prompts
// are byte-identical between modes for the same content. Oversize files are
// skipped whole (never truncated mid-content), same as the disk path.
func readProjectKBFromStore(ctx context.Context, kb *kbstore.Store, orgID, projectID string) (string, bool, error) {
	files, err := kb.List(ctx, orgID, projectID)
	if err != nil {
		return "", false, err
	}
	names := make([]string, 0, len(files))
	sizeByName := make(map[string]int64, len(files))
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".md") {
			continue
		}
		names = append(names, f.Name)
		sizeByName[f.Name] = f.Size
	}
	sort.Strings(names)

	var buf bytes.Buffer
	truncated := false
	for _, name := range names {
		headerOverhead := len("## ") + len(name) + len("\n\n") + len("\n\n")
		needed := buf.Len() + headerOverhead + int(sizeByName[name])
		if needed > kbInlineMaxBytes {
			truncated = true
			continue
		}
		rc, err := kb.Get(ctx, orgID, projectID, name)
		if err != nil {
			classifyLog.Warn("read kb file from store failed", "project", projectID, "file", name, "error", err)
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			classifyLog.Warn("read kb file from store failed", "project", projectID, "file", name, "error", err)
			continue
		}
		fmt.Fprintf(&buf, "## %s\n\n%s\n\n", name, data)
	}
	if truncated {
		buf.WriteString("\n…[some knowledge-base files exceeded the inline cap and were skipped]")
	}
	return buf.String(), truncated, nil
}

// realRunStage1Haiku runs a single-shot Haiku classification. It is the
// default value of the Runner's stage1Fn seam (set in NewRunner); tests
// swap stage1Fn for a stub. It reads the per-org credentials off the
// receiver.
func (r *Runner) realRunStage1Haiku(ctx context.Context, orgID string, p haikuPrompt) (int, string, error) {
	return r.runHaiku(ctx, orgID, p)
}

// runHaiku drives one classification call through systemllm.Complete —
// local mode via the shared agent runtime, multi mode via a direct
// Anthropic/Bedrock API call — and parses the {score, rationale} JSON the
// model emits. ctx propagates from the Runner's stop channel so server
// shutdown aborts in-flight calls instead of waiting for the model to time
// out.
func (r *Runner) runHaiku(ctx context.Context, orgID string, p haikuPrompt) (int, string, error) {
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

	result, err := r.recorder.Complete(ctx, systemllm.CompleteOptions{
		OrgID:        orgID,
		Job:          systemllm.JobClassifier,
		Message:      p.Message,
		SystemPrompt: p.SystemPrompt,
		UserMessage:  p.UserMessage,
		Model:        ai.SystemJobModel,
		DirectModel:  ai.SystemJobModelDirect,
		MaxTokens:    2048,
		Temperature:  0.1,
		TraceID:      "classify-stage1",
		Secrets:      r.secrets,
		LLMResolver:  r.llmResolve,
	})
	if err != nil {
		return 0, "", fmt.Errorf("classify agent failed: %w", err)
	}

	raw := ai.StripCodeFences([]byte(result.Text))

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
