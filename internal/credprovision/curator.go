package credprovision

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
)

// ProvisionForCuratorTurn resolves a homed curator turn's credentials and
// seals them to the per-turn sidecar key its home executor published on the
// conversation's active claim, writing the shared claim_credentials channel
// (RunCredentials, keyed by the conversation id). The curator-turn analog of
// ProvisionForRun — called synchronously off the executor's
// curator_cred_request notification (the fast path) and by the backstop
// sweep. Idempotent and safe to call repeatedly for the same turn.
//
// A tolerant no-op (nil error, no write) when the conversation has no active
// claim or the claim hasn't published a sidecar pubkey — there is nothing to
// seal to, and the turn may have just finished between the notification
// firing and this handler running. Same posture as ProvisionForRun's
// not-claimed no-op.
func (m *Manager) ProvisionForCuratorTurn(ctx context.Context, orgID, conversationID string) error {
	turn, ok, err := m.stores.Curator.GetTurnProvisionInfoSystem(ctx, orgID, conversationID)
	if err != nil {
		return fmt.Errorf("credprovision: read curator turn %s: %w", conversationID, err)
	}
	if !ok || turn.HomeInstanceID == "" || turn.CredPubKey == "" {
		return nil
	}

	inst, err := m.stores.Instances.Get(ctx, turn.HomeInstanceID)
	if err != nil {
		return fmt.Errorf("credprovision: read curator home instance %s: %w", turn.HomeInstanceID, err)
	}
	if inst == nil {
		log.Warn("curator turn home instance not found; skipping (not yet registered)",
			"conversation", conversationID, "home", turn.HomeInstanceID)
		return nil
	}
	// Seal to the home's current boot epoch directly: a curator turn runs on the
	// live home's per-project session loop, so there is no separate claim epoch
	// to reconcile the way ProvisionForRun guards against (a run parked under an
	// earlier claim than the instance's current boot). The executor-side Get
	// epoch check plus Put's never-regress guard cover a home restart mid-flight.
	pubBytes, err := base64.StdEncoding.DecodeString(turn.CredPubKey)
	if err != nil || len(pubBytes) != 32 {
		return fmt.Errorf("credprovision: curator turn %s carries a malformed recipient pubkey", conversationID)
	}
	var pub [32]byte
	copy(pub[:], pubBytes)

	bundle := &credbundle.Bundle{BootEpoch: inst.BootEpoch}

	// LLM — the conversation id becomes the STS RoleSessionName for per-turn
	// CloudTrail attribution, exactly like runs; fall back to raw-secret
	// resolution when no llmcred resolver is wired.
	if m.llm != nil {
		mat, err := m.llm.ResolveForBundle(ctx, orgID, conversationID)
		if err != nil && !llmcred.IsNoCredentials(err) {
			return fmt.Errorf("credprovision: resolve LLM credentials for org %s (curator turn %s): %w", orgID, conversationID, err)
		}
		bundle.LLM = mat.Env
		if !mat.Expiry.IsZero() {
			bundle.LLMExpiryUnix = mat.Expiry.Unix()
		}
	} else {
		llm, err := agentproc.ResolveCredentialsForBundle(ctx, agentproc.NewSystemSecretsReader(m.stores.Secrets), orgID)
		if err != nil && !errors.Is(err, agentproc.ErrNoCredentialsConfigured) {
			return fmt.Errorf("credprovision: resolve LLM credentials for org %s: %w", orgID, err)
		}
		bundle.LLM = llm
	}

	// GitHub authorized set is the project's pinned repos ∩ the team's tracked
	// set — a curator turn has no conversation_worktrees ledger and no task repo, it reads
	// only the pinned repos the project surfaces. The project read is admin-pool
	// (the brain has no JWT claims here); the team is the conversation's
	// snapshot, carried on the provisioning projection.
	pinned := []string(nil)
	if m.stores.Projects != nil {
		project, err := m.stores.Projects.GetSystem(ctx, orgID, turn.ProjectID)
		if err != nil {
			return fmt.Errorf("credprovision: read project %s for curator turn %s: %w", turn.ProjectID, conversationID, err)
		}
		if project != nil {
			pinned = project.PinnedRepos
		}
	}
	if gh, err := m.resolveGitHubForCuratorTurn(ctx, orgID, turn.TeamID, pinned); err != nil {
		return fmt.Errorf("credprovision: resolve github credentials for curator turn %s: %w", conversationID, err)
	} else {
		bundle.GitHub = gh
	}

	if jc, err := m.resolveJira(ctx, orgID); err != nil {
		return fmt.Errorf("credprovision: resolve jira credentials for org %s: %w", orgID, err)
	} else {
		bundle.Jira = jc
	}

	// Providers resolve their own sealed sets. A curator turn has no task, so
	// TaskID is empty; the built-in resolvers key on org/team/run and tolerate it.
	if providers, err := agenthost.ResolveProviderCredentials(ctx, m.stores, agenthost.ProvisionScope{
		OrgID: orgID, TeamID: turn.TeamID, TaskID: "", RunID: conversationID,
	}); err != nil {
		return fmt.Errorf("credprovision: resolve provider credentials for curator turn %s: %w", conversationID, err)
	} else {
		bundle.Providers = providers
	}

	plaintext, err := bundle.Marshal()
	if err != nil {
		return fmt.Errorf("credprovision: marshal bundle for curator turn %s: %w", conversationID, err)
	}
	sealed, err := credseal.Seal(pub, plaintext)
	if err != nil {
		return fmt.Errorf("credprovision: seal bundle for curator turn %s: %w", conversationID, err)
	}
	if err := m.stores.RunCredentials.Put(ctx, orgID, conversationID, turn.HomeInstanceID, inst.BootEpoch, sealed); err != nil {
		return fmt.Errorf("credprovision: write bundle for curator turn %s: %w", conversationID, err)
	}
	log.Debug("provisioned curator turn credential bundle", "conversation", conversationID, "org", orgID, "home", turn.HomeInstanceID, "boot_epoch", inst.BootEpoch)
	return nil
}

// resolveGitHubForCuratorTurn is the curator-turn analog of resolveGitHub: the
// authorized repo set is the project's pinned repos ∩ the team's tracked set,
// and it mints one repo-scoped installation token per member. Deliberately
// parallel to (rather than a refactor of) resolveGitHub so the delegated-run
// path stays byte-identical. nil when the org has no usable GitHub credential
// (a Jira-only org — no regression, its curator does no git).
func (m *Manager) resolveGitHubForCuratorTurn(ctx context.Context, orgID, teamID string, pinnedRepos []string) (*credbundle.GitHubCreds, error) {
	scoped, ok := m.ghResolver.(ghclient.ScopedResolver)
	if !ok {
		return nil, nil
	}
	has, err := scoped.HasAnyCredential(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("probe github credential: %w", err)
	}
	if !has {
		return nil, nil
	}

	gh := &credbundle.GitHubCreds{Mode: "app", RepoTokens: map[string]credbundle.RepoToken{}}
	if base, err := m.ghResolver.BaseURLFor(ctx, orgID); err == nil {
		gh.BaseURL = base
	}
	if name, email, ok := m.ghResolver.OrgIdentityFor(ctx, orgID); ok {
		gh.IdentityName = name
		gh.IdentityEmail = email
	}

	seen := map[string]bool{}
	var authorized []string
	for _, repoID := range pinnedRepos {
		key := strings.ToLower(repoID)
		if seen[key] {
			continue
		}
		owner, repo, ok := strings.Cut(repoID, "/")
		if !ok {
			continue
		}
		// pinned ∩ tracked: mint only for a repo the team actually tracks, the
		// same filter authorizedRepos applies on the run path. A nil store or a
		// lookup error drops the repo closed (better no token than an
		// unauthorized one).
		if m.stores.TeamGitHubRepos == nil {
			continue
		}
		tracks, err := m.stores.TeamGitHubRepos.TracksRepoSystem(ctx, teamID, owner, repo)
		if err != nil || !tracks {
			continue
		}
		seen[key] = true
		tok, err := scoped.TokenForRepoScoped(ctx, orgID, owner, repo, nil)
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				continue
			}
			return nil, fmt.Errorf("mint token for %s: %w", repoID, err)
		}
		if tok.ExpiresAt.IsZero() {
			gh.Mode = "pat"
			gh.PAT = tok.Value
		}
		gh.RepoTokens[repoID] = credbundle.RepoToken{Token: tok.Value, ExpiresAt: tok.ExpiresAt}
		authorized = append(authorized, repoID)
	}

	// The real-gh channel's single team-set-scoped token, same as the delegated
	// run path (see resolveGitHub's CLIToken block). A curator turn has no
	// single "primary repo", so the primary owner is chosen from the pinned set
	// alone.
	if owner, names := cliChannelScope("", authorized); owner != "" {
		cliTok, err := scoped.TokenForReposScoped(ctx, orgID, owner, names, nil)
		if err != nil {
			if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				return nil, fmt.Errorf("mint gh-channel token for owner %s: %w", owner, err)
			}
		} else if cliTok.Value != "" {
			gh.CLIToken = &credbundle.RepoToken{Token: cliTok.Value, ExpiresAt: cliTok.ExpiresAt}
		}
	}
	return gh, nil
}
