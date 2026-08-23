package delegate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghbin"
	"github.com/sky-ai-eng/triage-factory/internal/ghchannel"
	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// noopChannelCloser stands in for a channel that never started, so callers can
// defer Close unconditionally.
type noopChannelCloser struct{}

func (noopChannelCloser) Close() error { return nil }

// startLocalGHChannel provisions the real-`gh` credential channel for a
// local-mode run: the TF-owned pinned binary, a loopback injector holding the
// org's live credential, and the per-run placeholder + trust file the agent's gh
// presents. It is the local counterpart of the sidecar's channel, and produces
// the same agentproc.GHChannelParams shape, so the agent-facing surface is one
// surface in both modes.
//
// Returns (nil, no-op closer) — never an error — whenever the channel can't be
// stood up: outside local mode (multi brings its own via the sidecar, and its
// executors deliberately hold no live secret store to resolve a credential
// from), with no credential resolver wired, on a platform with no pinned gh, or
// when the fetch itself fails. Every one of those degrades the run to the scoped
// `exec gh` verbs, which still work; failing the run instead would trade a
// missing convenience for a dead task.
//
// owner is the run's primary repo owner, used to resolve which credential the
// injector injects. Empty (a Jira run with no repo yet) still resolves the org's
// account-level credential, which is what a PAT org has anyway.
func (s *Spawner) startLocalGHChannel(ctx context.Context, orgID, conversationID, owner string, info agenthost.ConversationInfo, gitHandlers ...http.Handler) (*agentproc.GHChannelParams, io.Closer) {
	if runmode.Current() != runmode.ModeLocal {
		return nil, noopChannelCloser{}
	}
	if conversationID == "" {
		return nil, noopChannelCloser{}
	}

	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return nil, noopChannelCloser{}
	}
	stores, storesSet := s.getStores()

	ghPath, err := ghbin.Ensure(ctx)
	if err != nil {
		// Expected on an unpinned platform and on any host that can't reach
		// the release; both are ordinary, so say what was lost and move on.
		if errors.Is(err, ghbin.ErrUnsupportedPlatform) {
			delegateLog.Info("no pinned gh for this platform; the agent continues on the scoped exec verbs", "conversation", conversationID)
		} else {
			delegateLog.Warn("provision pinned gh failed; the agent continues on the scoped exec verbs", "conversation", conversationID, "error", err)
		}
		return nil, noopChannelCloser{}
	}

	// Which credential the injector will attach, settled once here rather than
	// per observation: it comes from the same resolver call the TokenSource
	// below makes, so the two can never disagree, and a run's channel holds one
	// credential for its whole life. A resolve failure leaves the App default
	// and costs a label — the first real request surfaces the same failure
	// where it is actionable.
	credential := domain.CredentialGitHubApp
	if tok, terr := resolver.TokenFor(ctx, orgID, owner); terr == nil && tok.Value != "" && tok.ExpiresAt.IsZero() {
		// No expiry is the PAT tell: an installation token always carries one.
		credential = domain.CredentialGitHubPAT
	}

	var gitHandler http.Handler
	if len(gitHandlers) > 0 {
		gitHandler = gitHandlers[0]
	}
	ch, err := ghchannel.Start(ghchannel.Config{
		ConversationID: conversationID,
		Upstream:       s.githubAPIUpstreamFor(ctx, orgID),
		BinDir:         filepath.Dir(ghPath),
		// Resolved per request against the live secret store — no caching in
		// front of it — so a credential rotated mid-run is picked up on the
		// next call. Local mode has no executor split and no sealed bundle,
		// which is why reading the live store here is correct rather than the
		// recurring executor-side bug it would be in multi.
		TokenSource: func(ctx context.Context) (string, error) {
			tok, err := resolver.TokenFor(ctx, orgID, owner)
			if err != nil {
				return "", err
			}
			if tok.Value == "" {
				return "", ghclient.ErrNoGitHubCredentials
			}
			return tok.Value, nil
		},
		Observe: func(ctx context.Context, m ghinjector.ObservedMutation) {
			if !storesSet {
				return
			}
			recordLocalObservation(ctx, stores, info, m)
		},
		ObserveWrite: func(ctx context.Context, w ghinjector.ObservedWrite) {
			if !storesSet {
				return
			}
			// Same builder the relayed (multi) path calls, on the same
			// observation type, so a write audited here is byte-identical to one
			// audited from a sandbox.
			agenthost.RecordExternalWrite(ctx, stores, info, nil,
				agenthost.GHChannelWriteAction(w, credential))
		},
		AuthorizeWrite: func(ctx context.Context, req ghwrite.Request, ref ghwrite.Refusal) bool {
			// The same decision the sandbox path relays, minus the relay and
			// minus the re-derivation it exists for: the proxy that recognized
			// the shape, the policy, and the database are one process here, so
			// there is no second reading to reconcile. A run whose stores never
			// arrived still refuses — the decision does not depend on being
			// able to record it — and loses only the row.
			if storesSet {
				agenthost.RecordExternalWrite(ctx, stores, info, nil,
					agenthost.GHWriteDeniedAction(req, ref, credential))
			}
			return false
		},
		GitHandler: gitHandler,
	})
	if err != nil {
		delegateLog.Warn("start local gh channel failed; the agent continues on the scoped exec verbs", "conversation", conversationID, "error", err)
		return nil, noopChannelCloser{}
	}

	delegateLog.Info("local gh channel up", "conversation", conversationID, "host", ch.Host)
	return &agentproc.GHChannelParams{
		Host:           ch.Host,
		Token:          ch.Token,
		CertSourcePath: ch.CertPath,
		BinDir:         ch.BinDir,
		ConfigDir:      ch.ConfigDir,
	}, ch
}

// recordLocalObservation upserts the artifact behind one observed gh mutation.
// The mutation already landed on GitHub, so this is best-effort by construction:
// the same mapping and the same recording funnel the relayed (multi) path uses,
// minus the relay — local mode holds the DB in this very process.
func recordLocalObservation(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, m ghinjector.ObservedMutation) {
	art, ok := agenthost.ObservationArtifact(agentproc.RecordObservationArgs{
		Kind:        m.Kind,
		Owner:       m.Owner,
		Repo:        m.Repo,
		Number:      m.Number,
		NodeID:      m.NodeID,
		Head:        m.Head,
		Base:        m.Base,
		URL:         m.URL,
		Title:       m.Title,
		Body:        m.Body,
		Draft:       m.Draft,
		ReviewID:    m.ReviewID,
		ReviewState: m.ReviewState,
	}, info.ConversationID)
	if !ok {
		return
	}
	agenthost.RecordExternalWrite(ctx, stores, info, &art, nil)
}

// githubAPIUpstreamFor resolves the REST API base the local gh channel's
// injector forwards to — api.github.com, a GHES {base}/api/v3, or a .ghe.com
// api host — derived from the org's non-secret configured GitHub base via the
// same APIBase mapping the client uses. Empty (resolver unwired / read error)
// defaults to api.github.com inside ghchannel. Local mode only: a sandboxed
// run's injector reads the host off its sealed bundle instead.
func (s *Spawner) githubAPIUpstreamFor(ctx context.Context, orgID string) string {
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return ""
	}
	base, err := resolver.BaseURLFor(ctx, orgID)
	if err != nil {
		delegateLog.Warn("resolve github base for the local gh channel failed; defaulting to api.github.com", "org", orgID, "error", err)
		return ""
	}
	return ghbase.APIBase(base)
}
