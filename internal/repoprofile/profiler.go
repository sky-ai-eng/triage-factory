package repoprofile

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

const (
	profileBatchSize = 5
	profilingModel   = "haiku"
	maxDocChars      = 10000
	reprofileTTL     = 3 * 24 * time.Hour // skip repos profiled within the last 3 days
)

// Profiler builds and persists AI-generated profiles for GitHub repositories.
type Profiler struct {
	resolver github.Resolver         // per-(org, owner) GitHub client source — App-installation token in multi, keychain PAT in local. SKY-389.
	secrets  agentproc.SecretsReader // per-org LLM-credential reader for the profiling Haiku calls (nil in local → ambient subscription; system-door reader in multi). SKY-389.
	database *sql.DB
	repos    db.RepoStore // profile reads + upserts go through the store
	orgs     db.OrgsStore // iterate active orgs at the top of each profile run
	ws       *websocket.Hub
}

// NewProfiler creates a Profiler with the given GitHub resolver, per-org
// secrets reader, DB handle, repo store, orgs store, and WS hub. The
// resolver is consulted per (org, owner) inside the profiling loop so the
// same code path serves both local (keychain PAT) and multi (App token).
func NewProfiler(resolver github.Resolver, secrets agentproc.SecretsReader, database *sql.DB, repos db.RepoStore, orgs db.OrgsStore, ws *websocket.Hub) *Profiler {
	return &Profiler{resolver: resolver, secrets: secrets, database: database, repos: repos, orgs: orgs, ws: ws}
}

// repoWithDocs groups a repo profile with the documentation text to send to the LLM.
type repoWithDocs struct {
	profile domain.RepoProfile
	docs    string
}

// Run iterates active orgs and profiles each one's configured repos.
// Per-org outer loop: resolves the per-org repo list inside the loop
// so a new org added between boot and the next forced re-profile
// picks up correctly. Local mode
// collapses to N=1 — the single runmode.LocalDefaultOrgID sentinel.
//
// If force is true, the TTL check is skipped (used for manual
// re-profile triggered by a GitHub config change).
//
// Per-org errors are logged but do not abort the whole run. The
// returned error is only non-nil if context is cancelled mid-flight;
// individual-org failures degrade gracefully (row writes still happen
// where possible, falls back to docs-only).
func (p *Profiler) Run(ctx context.Context, force bool) error {
	orgIDs, err := p.orgs.ListActiveSystem(ctx)
	if err != nil {
		return fmt.Errorf("list active orgs: %w", err)
	}
	for _, orgID := range orgIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		repos, err := p.repos.ListConfiguredNamesSystem(ctx, orgID)
		if err != nil {
			log.Printf("[repoprofile] org %s: load configured repos: %v", orgID, err)
			continue
		}
		if err := p.runOrg(ctx, orgID, repos, force); err != nil {
			if ctx.Err() != nil {
				return err
			}
			log.Printf("[repoprofile] org %s: %v", orgID, err)
		}
	}
	return nil
}

// runOrg profiles one org's repos. Per-repo failures are logged and
// the loop continues — partial progress is better than aborting the
// whole batch on a transient GitHub API failure or a malformed repo
// name. Cross-org repo dedup at the GitHub fetch layer (two orgs
// both polling owner/repo doing two fetches) is a deferred concern
// — local mode is N=1 so there's no immediate dedup pressure.
func (p *Profiler) runOrg(ctx context.Context, orgID string, repos []string, force bool) error {
	if len(repos) == 0 {
		return nil
	}

	log.Printf("[repoprofile] org %s: profiling %d configured repos", orgID, len(repos))

	// Resolve the clone protocol once for the whole run rather than
	// re-reading per-org settings inside the per-repo loop. The setting
	// can't change mid-run — handleSettingsPost serializes the org-
	// settings write behind the same `onGitHubChanged` callback that
	// owns this goroutine — so capturing it here matches actual
	// semantics and avoids N redundant DB reads.
	preferSSH := false
	if orgSet, oErr := p.orgs.GetSettingsSystem(ctx, orgID); oErr != nil {
		log.Printf("[repoprofile] load org settings to pick clone protocol: %v (defaulting to HTTPS)", oErr)
	} else {
		preferSSH = orgSet.GitHubCloneProtocol == "ssh"
	}

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

		// Skip repos that were recently profiled (unless forced)
		if !force {
			existing, err := p.repos.GetSystem(ctx, orgID, name)
			if err != nil {
				log.Printf("[repoprofile] %s: failed to check profile: %v", name, err)
				continue
			}
			if existing != nil && existing.ProfiledAt != nil {
				age := time.Since(*existing.ProfiledAt)
				if age < reprofileTTL {
					log.Printf("[repoprofile] %s: profiled %s ago, skipping (TTL %s)", name, age.Round(time.Hour), reprofileTTL)
					continue
				}
			}
		}

		// Resolve the per-(org, owner) GitHub client. Done after the TTL
		// skip so we don't mint a token for a repo we're about to skip.
		client, cerr := p.resolver.ClientFor(ctx, orgID, owner)
		if cerr != nil {
			log.Printf("[repoprofile] %s: resolve GitHub client: %v (skipping)", name, cerr)
			continue
		}

		readme, err := client.GetFileContent(owner, repo, "README.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get README.md: %v", name, err)
		}

		claudeMd, err := client.GetFileContent(owner, repo, "CLAUDE.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get CLAUDE.md: %v", name, err)
		}

		agentsMd, err := client.GetFileContent(owner, repo, "AGENTS.md")
		if err != nil {
			log.Printf("[repoprofile] %s: get AGENTS.md: %v", name, err)
		}

		// Fetch repo metadata (default branch, clone URL). Both HTTPS
		// and SSH forms come back from the same /repos/:owner/:repo
		// response, so picking is a one-line branch on the result.
		// Empty SSHURL (legacy GHE deployments without ssh_url
		// surfaced) falls back to HTTPS so we always have *some* URL
		// on the row.
		var defaultBranch, cloneURL string
		if meta, err := client.GetRepoMeta(owner, repo); err != nil {
			log.Printf("[repoprofile] %s: get repo meta: %v", name, err)
		} else {
			defaultBranch = meta.DefaultBranch
			cloneURL = meta.CloneURL
			if preferSSH && meta.SSHURL != "" {
				cloneURL = meta.SSHURL
			}
		}

		prof := domain.RepoProfile{
			ID:            name,
			Owner:         owner,
			Repo:          repo,
			HasReadme:     readme != "",
			HasClaudeMd:   claudeMd != "",
			HasAgentsMd:   agentsMd != "",
			CloneURL:      cloneURL,
			DefaultBranch: defaultBranch,
		}

		// Persist docs flags immediately so the UI can show them before profiling completes
		if err := p.repos.UpsertSystem(ctx, orgID, prof); err != nil {
			log.Printf("[repoprofile] upsert %s (docs flags): %v", name, err)
		}
		if p.ws != nil {
			p.ws.Broadcast(websocket.Event{
				Type:  "repo_docs_updated",
				OrgID: orgID,
				Data: map[string]any{
					"id":            name,
					"has_readme":    prof.HasReadme,
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

		results, err := profileBatch(ctx, orgID, batch, p.secrets)
		if err != nil {
			log.Printf("[repoprofile] batch %d failed: %v", i/profileBatchSize+1, err)
			repoNames := make([]string, len(batch))
			for j, d := range batch {
				repoNames[j] = d.profile.ID
			}
			toast.Warning(p.ws, orgID, fmt.Sprintf("Profiling failed for %s — rows saved without AI summary", strings.Join(repoNames, ", ")))
			// Fallback: upsert without profile_text so the row at least exists.
			for _, d := range batch {
				if uErr := p.repos.UpsertSystem(ctx, orgID, d.profile); uErr != nil {
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
			if err := p.repos.UpsertSystem(ctx, orgID, prof); err != nil {
				log.Printf("[repoprofile] upsert %s: %v", prof.ID, err)
				continue
			}
			if prof.ProfileText != "" {
				profiled++
				if p.ws != nil {
					p.ws.Broadcast(websocket.Event{
						Type:  "repo_profile_updated",
						OrgID: orgID,
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

func profileBatch(ctx context.Context, orgID string, batch []repoWithDocs, secrets agentproc.SecretsReader) ([]repoProfileResult, error) {
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

	// Run through the shared agent runtime. NoopSink discards per-message
	// stream events; we only care about the terminal Result.Result string,
	// which carries the model's JSON array response (same string the old
	// `claude --output-format json` envelope's `.result` field carried).
	outcome, err := agentproc.Run(ctx, agentproc.RunOptions{
		Model:   profilingModel,
		Message: prompt,
		TraceID: "repoprofile-batch",
		OrgID:   orgID,
		Secrets: secrets,
	}, agentproc.NoopSink{})
	if err != nil {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		return nil, fmt.Errorf("repoprofile agent failed: %w, stderr: %s", err, stderr)
	}
	if outcome == nil || outcome.Result == nil {
		return nil, fmt.Errorf("repoprofile agent: no terminal result event")
	}

	raw := ai.StripCodeFences([]byte(outcome.Result.Result))

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
