package repoprofile

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/ai"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
	"github.com/sky-ai-eng/todo-tinder/internal/github"
	"github.com/sky-ai-eng/todo-tinder/pkg/websocket"
)

const (
	profileBatchSize = 5
	profilingModel   = "haiku"
	maxDocChars      = 10000
)

// Profiler builds and persists AI-generated profiles for GitHub repositories.
type Profiler struct {
	gh       *github.Client
	database *sql.DB
	ws       *websocket.Hub
}

// NewProfiler creates a Profiler with the given GitHub client, DB handle, and WS hub.
func NewProfiler(gh *github.Client, database *sql.DB, ws *websocket.Hub) *Profiler {
	return &Profiler{gh: gh, database: database, ws: ws}
}

// repoWithDocs groups a repo profile with the documentation text to send to the LLM.
type repoWithDocs struct {
	profile domain.RepoProfile
	docs    string
}

// Run profiles the given repos (from config). For each, it fetches docs
// (README.md, CLAUDE.md, AGENTS.md), then batches through Haiku for profiling.
func (p *Profiler) Run(ctx context.Context, repos []string) error {
	if len(repos) == 0 {
		log.Printf("[repoprofile] no repos configured, skipping")
		return nil
	}

	log.Printf("[repoprofile] profiling %d configured repos", len(repos))

	var withDocs []repoWithDocs
	var withoutDocs []domain.RepoProfile

	for _, name := range repos {
		if err := ctx.Err(); err != nil {
			return err
		}

		parts := strings.SplitN(name, "/", 2)
		if len(parts) != 2 {
			log.Printf("[repoprofile] skipping malformed repo name %q", name)
			continue
		}
		owner, repo := parts[0], parts[1]

		readme, err := p.gh.GetFileContent(owner, repo, "README.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get README.md: %v", name, err)
		}

		claudeMd, err := p.gh.GetFileContent(owner, repo, "CLAUDE.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get CLAUDE.md: %v", name, err)
		}

		agentsMd, err := p.gh.GetFileContent(owner, repo, "AGENTS.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get AGENTS.md: %v", name, err)
		}

		prof := domain.RepoProfile{
			ID:          name,
			Owner:       owner,
			Repo:        repo,
			HasReadme:   readme != "",
			HasClaudeMd: claudeMd != "",
			HasAgentsMd: agentsMd != "",
		}

		// Persist docs flags immediately so the UI can show them before profiling completes
		if err := db.UpsertRepoProfile(p.database, prof); err != nil {
			log.Printf("[repoprofile] upsert %s (docs flags): %v", name, err)
		}
		if p.ws != nil {
			p.ws.Broadcast(websocket.Event{
				Type: "repo_docs_updated",
				Data: map[string]any{
					"id":           name,
					"has_readme":   prof.HasReadme,
					"has_claude_md": prof.HasClaudeMd,
					"has_agents_md": prof.HasAgentsMd,
				},
			})
		}

		docs := buildDocText(readme, claudeMd, agentsMd)
		if docs == "" {
			withoutDocs = append(withoutDocs, prof)
		} else {
			withDocs = append(withDocs, repoWithDocs{profile: prof, docs: docs})
		}
	}

	log.Printf("[repoprofile] %d repos with docs, %d without", len(withDocs), len(withoutDocs))

	// Batch-profile repos that have docs through Haiku.
	profiled := 0
	for i := 0; i < len(withDocs); i += profileBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		end := i + profileBatchSize
		if end > len(withDocs) {
			end = len(withDocs)
		}
		batch := withDocs[i:end]

		results, err := profileBatch(batch)
		if err != nil {
			log.Printf("[repoprofile] batch %d failed: %v", i/profileBatchSize+1, err)
			// Fallback: upsert without profile_text so the row at least exists.
			for _, d := range batch {
				if uErr := db.UpsertRepoProfile(p.database, d.profile); uErr != nil {
					log.Printf("[repoprofile] upsert %s (fallback): %v", d.profile.ID, uErr)
				}
			}
			continue
		}

		byRepo := make(map[string]string, len(results))
		for _, r := range results {
			byRepo[r.Repo] = r.Profile
		}

		now := time.Now()
		for _, d := range batch {
			prof := d.profile
			if text := byRepo[prof.ID]; text != "" {
				prof.ProfileText = text
				prof.ProfiledAt = &now
			}
			if err := db.UpsertRepoProfile(p.database, prof); err != nil {
				log.Printf("[repoprofile] upsert %s: %v", prof.ID, err)
				continue
			}
			if prof.ProfileText != "" {
				profiled++
				if p.ws != nil {
					p.ws.Broadcast(websocket.Event{
						Type: "repo_profile_updated",
						Data: map[string]any{
							"id":           prof.ID,
							"profile_text": prof.ProfileText,
						},
					})
				}
			}
		}
	}

	log.Printf("[repoprofile] done: %d profiled with AI, %d without docs", profiled, len(withoutDocs))
	return nil
}

// repoProfileInput is the per-repo JSON sent to the LLM.
type repoProfileInput struct {
	Repo string `json:"repo"`
	Docs string `json:"docs"`
}

// repoProfileResult is one entry in the LLM's JSON array response.
type repoProfileResult struct {
	Repo    string `json:"repo"`
	Profile string `json:"profile"`
}

func profileBatch(batch []repoWithDocs) ([]repoProfileResult, error) {
	inputs := make([]repoProfileInput, len(batch))
	for i, d := range batch {
		inputs[i] = repoProfileInput{
			Repo: d.profile.ID,
			Docs: d.docs,
		}
	}

	inputJSON, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("marshal batch: %w", err)
	}

	prompt := fmt.Sprintf(ai.RepoProfilePrompt, string(inputJSON))

	args := []string{
		"-p", prompt,
		"--model", profilingModel,
		"--output-format", "json",
	}

	cmd := exec.Command("claude", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude command failed: %w, stderr: %s", err, stderr.String())
	}

	// claude --output-format json wraps the response in {"result": "..."}
	var envelope struct {
		Result string `json:"result"`
	}
	raw := stdout.Bytes()
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Result != "" {
		raw = []byte(envelope.Result)
	}

	raw = ai.StripCodeFences(raw)

	var results []repoProfileResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("parse response: %w, raw: %s", err, string(raw))
	}

	return results, nil
}

// buildDocText concatenates available documentation for a repo into a single
// block to send to the LLM. Returns empty string if no docs were found.
func buildDocText(readme, claudeMd, agentsMd string) string {
	var parts []string
	if readme != "" {
		parts = append(parts, "README.md:\n"+truncateStr(readme, maxDocChars))
	}
	if claudeMd != "" {
		parts = append(parts, "CLAUDE.md:\n"+truncateStr(claudeMd, maxDocChars))
	}
	if agentsMd != "" {
		parts = append(parts, "AGENTS.md:\n"+truncateStr(agentsMd, maxDocChars))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
