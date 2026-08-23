package runsidecar

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/apiproxy"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// credRuntime is the sidecar's credential brain: it owns this run's X25519
// private key (born here, never leaving the process), the unsealed bundle,
// and the credential proxies. The orchestrator drives it over the
// supervision channel — relaying the opaque sealed bundle down and the
// non-secret proxy env back up — and answers the git proxy's authorize
// callbacks the other way. It implements sidecarproto.Handler.
//
// Everything credential-bearing lives here and nowhere else: an orchestrator
// compromise reaches no key and no plaintext, and process exit frees this
// whole address space.
type credRuntime struct {
	keypair *credseal.KeyPair

	// conn is the supervision channel back to the orchestrator, used for the
	// git proxy's authorize/denial callbacks. Set once immediately after the
	// conn is constructed (the conn needs this runtime as its Handler first).
	conn *sidecarproto.Conn

	// sharedOrigin is the run's fake-GHE origin listener, bound by the broker on
	// a privileged port and handed down at launch. nil when none was passed, in
	// which case the gh injector binds an ephemeral port itself and the sandbox's
	// git keeps routing at the git proxy's own address. Set once at startup,
	// before any request is served.
	sharedOrigin net.Listener

	mu         sync.Mutex
	bundle     *credbundle.Bundle // newest unsealed bundle; nil until first seal
	proxies    *agentproc.RunProxyHandle
	githubAPI  *apiproxy.Server      // GitHub REST credential proxy; nil unless requested
	jiraAPI    *apiproxy.Server      // Jira REST credential proxy; nil unless requested
	ghInjector *ghinjector.Server    // real-gh credential-injector proxy; nil unless requested
	agentHost  *agenthost.HostDaemon // the relocated exec-verb socket server; nil unless requested
	proxied    bool                  // guards against a duplicate StartProxies
	bootEpoch  int64
}

// newCredRuntime mints the per-run keypair. The public half is published to
// the orchestrator (which relays it to the brain to seal against); the
// private half never leaves this process.
func newCredRuntime() (*credRuntime, error) {
	kp, err := credseal.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	return &credRuntime{keypair: kp}, nil
}

// setConn wires the supervision channel after construction (the conn is built
// with this runtime as its Handler, so the runtime can't be handed the conn
// at construction time).
func (r *credRuntime) setConn(c *sidecarproto.Conn) { r.conn = c }

// setSharedOriginListener adopts the broker-bound shared-origin listener (nil is
// fine — see the field's doc). Called once at startup, before the supervision
// channel serves anything.
func (r *credRuntime) setSharedOriginListener(ln net.Listener) { r.sharedOrigin = ln }

// sharedOriginHost is the host half of the shared origin's address — the value
// GH_HOST is set to, and the host the sandbox's git is routed at. Empty when no
// shared origin was passed.
//
// The PORT IS DROPPED, and that is the whole point: gh resolves a remote's host
// with url.Hostname(), which strips the port, then compares it to GH_HOST
// verbatim. Any port in GH_HOST makes that comparison unsatisfiable, so the
// shared origin lives on 443 and names itself without one.
func (r *credRuntime) sharedOriginHost() string {
	if r.sharedOrigin == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.sharedOrigin.Addr().String())
	if err != nil {
		return ""
	}
	return host
}

// helloBody is the sidecar's opening announcement: its per-run public key,
// base64-encoded, for the orchestrator to publish upward.
func (r *credRuntime) helloBody() sidecarproto.HelloBody {
	return sidecarproto.HelloBody{PubKey: base64.StdEncoding.EncodeToString(r.keypair.Public[:])}
}

// Handle dispatches an inbound supervision request. See sidecarproto.Handler.
func (r *credRuntime) Handle(ctx context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
	switch kind {
	case sidecarproto.KindSealedBundle:
		return nil, r.acceptSealedBundle(body)
	case sidecarproto.KindStartProxies:
		return r.startProxies(ctx, body)
	default:
		return nil, fmt.Errorf("runsidecar: unexpected request kind %q", kind)
	}
}

// acceptSealedBundle opens the relayed ciphertext with the per-run private
// key and stores the plaintext as the newest bundle. Repeatable: the brain's
// refresh sweep re-seals mid-run and the orchestrator re-relays, so a later
// call simply replaces the held bundle and the proxies' live sources pick up
// the newer credential on their next request. An unseal/parse failure is
// returned to the orchestrator (which surfaces it) and the prior bundle, if
// any, is left intact.
func (r *credRuntime) acceptSealedBundle(body json.RawMessage) error {
	var msg sidecarproto.SealedBundleBody
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("runsidecar: decode sealed bundle: %w", err)
	}
	plaintext, err := r.keypair.Open(msg.Sealed)
	if err != nil {
		return fmt.Errorf("runsidecar: unseal bundle: %w", err)
	}
	bundle, err := credbundle.Unmarshal(plaintext)
	if err != nil {
		return fmt.Errorf("runsidecar: parse bundle: %w", err)
	}
	r.mu.Lock()
	r.bundle = bundle
	r.bootEpoch = msg.BootEpoch
	r.mu.Unlock()
	return nil
}

// currentBundle returns the newest held bundle, or nil if none has arrived.
func (r *credRuntime) currentBundle() *credbundle.Bundle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bundle
}

// startProxies binds this run's credential proxies on the host-side veth IP
// and returns the non-secret sandbox env. The LLM/git material comes from the
// held bundle; the git proxy's authorize decision is relayed back to the
// orchestrator (no DB handle here). Idempotent-guarded: a duplicate request
// is an orchestrator bug, so it errors rather than leaking a second listener.
func (r *credRuntime) startProxies(ctx context.Context, body json.RawMessage) (any, error) {
	var req sidecarproto.StartProxiesBody
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("runsidecar: decode start-proxies: %w", err)
	}
	bundle := r.currentBundle()
	if bundle == nil {
		return nil, fmt.Errorf("runsidecar: start-proxies before any credential bundle was relayed")
	}

	r.mu.Lock()
	if r.proxied {
		r.mu.Unlock()
		return nil, fmt.Errorf("runsidecar: proxies already started for this run")
	}
	r.mu.Unlock()

	// The shared fake-GHE origin, when the broker bound one and this run is
	// actually getting an injector on it. Resolved first because the git config
	// the sandbox gets is decided inside StartRunProxies — and gated on the exact
	// same condition the injector start below is, so the sandbox's git is never
	// routed at a listener nothing ends up serving.
	sharedOriginHost := ""
	if ghChannelWanted(req) {
		sharedOriginHost = r.sharedOriginHost()
	}

	// One host for every GitHub lane, read off the bundle that carries the
	// tokens for it. The web base and the REST mount are the same fact in two
	// shapes, so they are derived together, here, from the one value the brain
	// sealed — nothing about the host travels in the frame.
	gitUpstream, apiUpstream := githubUpstreams(bundle)

	var git *agentproc.GitProxyConfig
	if req.GitEnabled {
		git = r.gitProxyConfig(gitUpstream)
		git.SharedOriginHost = sharedOriginHost
		git.SharedOriginCAPath = agentproc.SandboxGHInjectorCertPath
	}

	handle, env, err := agentproc.StartRunProxies(ctx, req.HostVethIP, bundle.LLM, git, r.recordEgressDenial, r.llmSource, req.IdentityConfigPairs...)
	if err != nil {
		return nil, err
	}

	// The LLM coordinates travel back to the orchestrator's own engine only —
	// no jail is ever pointed at the proxy. The proxy holds the key; the
	// caller holds a per-run placeholder.
	result := sidecarproto.StartProxiesResult{Env: env, LLMEnv: handle.LLMEnv()}
	// Surface the git proxy's address + per-run placeholder so the orchestrator
	// routes its OWN pre-sandbox clone through this same proxy — the real token
	// stays here. Empty when no git proxy was started (GitEnabled false).
	result.GitProxyURL, result.GitProxyToken = handle.GitProxy()

	// Read off the bundle that is already open here, so the orchestrator's audit
	// rows can name the credential these proxies inject. Sent unconditionally:
	// every channel the run writes through resolves out of this one bundle, so a
	// single classification answers for all of them.
	result.GitHubCredential = bundle.GitHub.Credential()

	// The GitHub/Jira REST credential proxies the orchestrator's own GetPR +
	// agenthost verbs route through: the orchestrator holds only the
	// placeholder, the sidecar injects the real token on the upstream hop. On
	// any failure here, tear down what already bound so a half-started run
	// leaks no listener.
	if req.GitHubAPIEnabled {
		srv, url, token, aerr := r.startGitHubAPIProxy(req.HostVethIP, apiUpstream)
		if aerr != nil {
			_ = handle.Shutdown(ctx)
			return nil, aerr
		}
		r.githubAPI = srv
		result.GitHubAPIURL, result.GitHubAPIToken = url, token
	}
	if req.JiraAPIEnabled {
		srv, url, token, deployment, aerr := r.startJiraAPIProxy(req.HostVethIP, req.JiraAPIUpstream)
		if aerr != nil {
			_ = handle.Shutdown(ctx)
			if r.githubAPI != nil {
				_ = r.githubAPI.Shutdown(ctx)
			}
			return nil, aerr
		}
		r.jiraAPI = srv
		result.JiraAPIURL, result.JiraAPIToken = url, token
		result.JiraDeployment = string(deployment)
	}

	// The real-gh credential-injector proxy: the TLS listener the sandboxed gh
	// reaches via GH_HOST, injecting the team-set-scoped token upstream while the
	// jail holds only a placeholder — and, when the broker handed one down, the
	// shared origin the sandbox's git was just routed at (see ghChannelWanted).
	if ghChannelWanted(req) {
		host, token, gerr := r.startGHInjector(req.HostVethIP, apiUpstream, req.AgentHost.ConversationID, handle.GitHandler())
		if gerr != nil {
			_ = handle.Shutdown(ctx)
			if r.githubAPI != nil {
				_ = r.githubAPI.Shutdown(ctx)
			}
			if r.jiraAPI != nil {
				_ = r.jiraAPI.Shutdown(ctx)
			}
			return nil, gerr
		}
		result.GHChannelHost, result.GHChannelToken = host, token
	}

	// Host the exec-verb socket server in THIS capless process rather than the
	// orchestrator (the relocation): the hostile-input parser now runs in the
	// per-run jail, holding no db.Stores and no secret store. Its LocalClient's
	// DB effects relay to the orchestrator over the supervision channel; its
	// gh/jira verbs build clients against the REST proxies just bound above, so
	// the real credential never leaves this process's proxy hop.
	if req.AgentHost != nil {
		if aerr := r.startAgentHost(req.AgentHost, result); aerr != nil {
			_ = handle.Shutdown(ctx)
			if r.githubAPI != nil {
				_ = r.githubAPI.Shutdown(ctx)
			}
			if r.jiraAPI != nil {
				_ = r.jiraAPI.Shutdown(ctx)
			}
			if r.ghInjector != nil {
				_ = r.ghInjector.Shutdown(ctx)
			}
			return nil, aerr
		}
	}

	r.mu.Lock()
	r.proxies = handle
	r.proxied = true
	r.mu.Unlock()

	return result, nil
}

// startAgentHost binds the relocated exec-verb socket server: a Server whose
// per-request LocalClient runs over a relay runtime (every DB effect relays to
// the orchestrator) and whose gh/jira verbs route through the REST proxies this
// sidecar just bound (holding only per-run placeholders). It creates the
// /run/tf/<conversationID>.sock the broker bind-mounts into the jail and grants it to the
// sandbox group — the same grant the orchestrator used to do, now owned by this
// process (a member of that group by launch). Identity comes from the
// orchestrator's AgentHostInfo but is never trusted for org-scoping: every
// relayed op is re-bound to the orchestrator's own ConversationInfo.
func (r *credRuntime) startAgentHost(ai *sidecarproto.AgentHostInfo, proxies sidecarproto.StartProxiesResult) error {
	info := agenthost.ConversationInfo{
		OrgID:            ai.OrgID,
		UserID:           ai.UserID,
		TeamID:           ai.TeamID,
		ConversationID:   ai.ConversationID,
		IsEventTriggered: ai.EventTriggered,
	}
	proxyCreds := &agenthost.ProxyCredentials{
		GitHubCredential: proxies.GitHubCredential,
		GitHubAPIURL:     proxies.GitHubAPIURL,
		GitHubAPIToken:   proxies.GitHubAPIToken,
		JiraAPIURL:       proxies.JiraAPIURL,
		JiraAPIToken:     proxies.JiraAPIToken,
		JiraDeployment:   proxies.JiraDeployment,
		GitProxyURL:      proxies.GitProxyURL,
		GitProxyToken:    proxies.GitProxyToken,
	}
	// The provider-credential accessor lets a provider handler (Slack) select
	// its bot token from the sealed bundle in-process — a live read so a mid-run
	// brain re-seal is picked up. The orchestrator is never asked for a secret.
	providerCreds := func(namespace string) (json.RawMessage, bool) {
		b := r.currentBundle()
		if b == nil {
			return nil, false
		}
		return b.ProviderCreds(namespace)
	}
	srv := agenthost.NewServerWithRuntime(agenthost.NewRelayRuntime(r.conn, info, providerCreds), proxyCreds)
	hd, _, err := startAgentHostSocket(srv, info.ConversationID)
	if err != nil {
		return fmt.Errorf("runsidecar: start agenthost: %w", err)
	}
	r.agentHost = hd
	return nil
}

// The two privileged filesystem effects startProxies has, behind vars so a test
// can drive the wiring around them. Both land under the root-only per-run socket
// root (/run/tf) — one writes the injector's certificate there, the other binds
// the exec-verb socket — so an unprivileged test process cannot take either path,
// and a test that needs the surrounding logic has to stand in for them.
//
// The seams live here, in the consumer, deliberately: cmd/exec/agenthost resolves
// those paths as constants and is the privileged side of this boundary. Making
// its path resolution mutable so another package's test can retarget it would
// trade a real invariant for test convenience.
var (
	writeInjectorCert    = agenthost.WriteInjectorCert
	startAgentHostSocket = agenthost.StartWithServer
)

// ghChannelWanted reports whether this request gets a gh injector. It needs the
// run's identity for the per-run cert path (the orchestrator mounts the same
// path), so a gh channel without one is skipped rather than guessed.
//
// One predicate, two readers: the injector start and the git routing that has to
// be decided before it. Splitting them would let the sandbox's git be pointed at
// a shared origin no injector ever serves.
func ghChannelWanted(req sidecarproto.StartProxiesBody) bool {
	return req.GHChannelEnabled && req.AgentHost != nil && req.AgentHost.ConversationID != ""
}

// githubUpstreams derives this run's two GitHub hosts from the sealed bundle:
// the web base the git proxy forwards to, and the REST mount the API proxy and
// the gh injector prepend. A bundle with no GitHub half (a Jira-only org) — or
// one whose org never configured a base — answers github.com / api.github.com,
// which is what an org on the public host has anyway. Neither is ever empty, so
// no lane below carries a default of its own.
func githubUpstreams(b *credbundle.Bundle) (git, api string) {
	base := ""
	if b.GitHub != nil {
		base = b.GitHub.BaseURL
	}
	return ghbase.ResolveBaseURL(base), ghbase.APIBase(base)
}

// startGHInjector stands up the real-gh credential-injector proxy: generates the
// per-run self-signed TLS cert (private key stays here), writes the public cert
// to the per-run path the orchestrator bind-mounts into the jail
// (agenthost.WriteInjectorCert), and serves TLS. The injector's TokenSource
// reads the held bundle's single team-set-scoped CLIToken live, so a mid-run
// re-seal is picked up; the observation callback relays the two artifact-bearing
// mutations to the orchestrator (which writes the artifact row).
//
// gitHandler is the run's git proxy handler, mounted behind the same listener so
// one address serves the API and git smart-HTTP the way a real GHE host does.
// nil (a run with no git proxy) leaves the listener API-only.
//
// Where it serves decides what GH_HOST can be. On the broker-bound shared origin
// it takes that listener and reports the bare host, which is what lets gh match
// a git remote against GH_HOST and infer the repo from a worktree. With no
// shared origin it binds an ephemeral port of its own and reports host:port —
// the channel still works for `gh api` and explicitly-scoped commands, and
// inference stays broken exactly as it was.
func (r *credRuntime) startGHInjector(hostVethIP, upstream, conversationID string, gitHandler http.Handler) (string, string, error) {
	cert, certPEM, err := ghinjector.GenerateCert(hostVethIP)
	if err != nil {
		return "", "", fmt.Errorf("runsidecar: generate gh-injector cert: %w", err)
	}
	// The trust file must exist at the per-run path before the sandbox launches
	// (the OCI spec references the mount source); the sidecar bring-up runs
	// before that launch, so writing here is early enough. It carries the host's
	// system roots plus this run's leaf, because SSL_CERT_FILE is process-global
	// in the jail — see ghinjector.TrustBundlePEM for why that widens nothing.
	if err := writeInjectorCert(conversationID, ghinjector.TrustBundlePEM(certPEM)); err != nil {
		return "", "", fmt.Errorf("runsidecar: write gh-injector cert: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return "", "", err
	}
	srv, err := ghinjector.New(r.ghInjectorConfig(upstream, cert, token, conversationID, gitHandler))
	if err != nil {
		return "", "", fmt.Errorf("runsidecar: construct gh injector: %w", err)
	}
	if ln := r.sharedOrigin; ln != nil {
		if _, err := srv.Serve(ln); err != nil {
			return "", "", fmt.Errorf("runsidecar: serve gh injector on the shared origin: %w", err)
		}
		r.ghInjector = srv
		// The bare host, no port — see sharedOriginHost.
		return r.sharedOriginHost(), token, nil
	}
	addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		return "", "", fmt.Errorf("runsidecar: start gh injector: %w", err)
	}
	r.ghInjector = srv
	return addr, token, nil
}

// ghInjectorConfig builds the gh channel's per-run wiring, the sibling of
// gitProxyConfig: the TokenSource reads the held bundle's single
// team-set-scoped CLIToken live (so a mid-run re-seal is picked up), and the
// two fire-and-forget callbacks relay to the orchestrator, which holds the DB.
//
// Split from startGHInjector so a test can assert on the wiring alone, without
// the listener bring-up around it.
func (r *credRuntime) ghInjectorConfig(upstream string, cert tls.Certificate, token, conversationID string, gitHandler http.Handler) ghinjector.Config {
	return ghinjector.Config{
		Upstream:         upstream,
		IncomingToken:    token,
		Cert:             cert,
		ConversationID:   conversationID,
		AllowNonLoopback: true,
		// The run's git proxy, re-homed behind this listener so the API and git
		// share one origin. Same handler as the standalone git-proxy listener:
		// the ref gate and the base-branch push policy are unchanged, and the
		// jail holds only the per-run placeholder on both halves.
		GitHandler: gitHandler,
		TokenSource: func(context.Context) (string, error) {
			bundle := r.currentBundle()
			if bundle == nil || bundle.GitHub == nil || bundle.GitHub.CLIToken == nil || bundle.GitHub.CLIToken.Token == "" {
				return "", fmt.Errorf("runsidecar: no gh-channel token in current bundle")
			}
			return bundle.GitHub.CLIToken.Token, nil
		},
		Observe: func(_ context.Context, m ghinjector.ObservedMutation) {
			// Fire-and-forget: the mutation already landed on GitHub; the
			// orchestrator (which holds the DB) builds and upserts the artifact.
			r.notifyAudit(agentproc.OpRecordObservation,
				agentproc.RecordObservationArgs{
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
				})
		},
		ObserveWrite: func(_ context.Context, w ghinjector.ObservedWrite) {
			// Fire-and-forget beside Observe: the request already transited, and
			// the orchestrator holds the DB that turns it into an audit row.
			// Every mutating REST call rides this, including the refused ones the
			// artifact path drops. The created object's coordinates ride along
			// for the shapes that make one — this process is the only one that
			// ever sees a response body, so nothing downstream could recover
			// them.
			args := agentproc.RecordGHWriteArgs{
				Method:         w.Method,
				Path:           w.Path,
				Status:         w.Status,
				ExternalID:     w.ExternalID,
				URL:            w.URL,
				Errored:        w.Errored,
				ResponseUnread: w.ResponseUnread,
			}
			// A GraphQL write is named in its request, which only this process
			// ever saw — the orchestrator receives the act's name or nothing.
			args.GraphQL = ghWriteFactsToWire(w.GraphQL)
			r.notifyAudit(agentproc.OpRecordGHWrite, args)
		},
		AuthorizeWrite: func(ctx context.Context, req ghwrite.Request, _ ghwrite.Refusal) bool {
			// A Call, not a notify: the agent's request is held until the
			// orchestrator answers, because a refusal that raced the forward
			// would not be one. The orchestrator re-derives the decision from
			// the shared table and writes the denial row before replying, so
			// this side sends only what was asked and never what it concluded.
			var reply agentproc.AuthorizeGHWriteReply
			if err := agentproc.CallRelay(ctx, r.conn, agentproc.RelayNamespaceCore, agentproc.OpAuthorizeGHWrite,
				agentproc.AuthorizeGHWriteArgs{
					Method:  req.Method,
					Path:    req.Path,
					GraphQL: ghWriteFactsToWire(req.GraphQL),
				}, &reply); err != nil {
				// Fail closed. A gated shape whose decision could not be
				// obtained is refused, or a wedged relay becomes the way past
				// the gate.
				sidecarLog.Warn("gh write authorization relay failed; refusing",
					"method", req.Method, "path", req.Path, "error", err)
				return false
			}
			return reply.Allowed
		},
	}
}

// ghWriteFactsToWire projects the classifier's GraphQL facts onto the relay
// protocol's own shape. nil in, nil out — a REST request carries no GraphQL
// member, and both relay ops that cross this boundary need the same projection.
func ghWriteFactsToWire(f *ghwrite.GraphQLFacts) *agentproc.GraphQLWriteFacts {
	if f == nil {
		return nil
	}
	return &agentproc.GraphQLWriteFacts{
		Operation:  f.Operation,
		Mutations:  f.Fields,
		Subject:    f.Subject,
		Unreadable: f.Unreadable,
	}
}

// startGitHubAPIProxy binds a GitHub-REST credential proxy on the veth IP.
// Its per-repo TokenSource reads the held bundle, so the real installation
// token never leaves the sidecar; the returned placeholder is what the
// orchestrator's ghclient presents. upstream is the bundle-derived REST mount.
func (r *credRuntime) startGitHubAPIProxy(hostVethIP, upstream string) (*apiproxy.Server, string, string, error) {
	token, err := randomToken()
	if err != nil {
		return nil, "", "", err
	}
	srv, err := apiproxy.New(apiproxy.Config{
		Provider:         apiproxy.ProviderGitHub,
		Upstream:         upstream,
		IncomingToken:    token,
		AllowNonLoopback: true,
		TokenSource: func(_ context.Context, owner, repo string) (string, error) {
			bundle := r.currentBundle()
			if bundle == nil {
				return "", fmt.Errorf("runsidecar: no current bundle for github api proxy")
			}
			tok, _, source := credbundle.ResolveRepoToken(bundle.GitHub, owner, repo)
			if source == credbundle.RepoTokenNone {
				return "", fmt.Errorf("runsidecar: no github token for %s/%s in bundle", owner, repo)
			}
			return tok, nil
		},
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("runsidecar: construct github api proxy: %w", err)
	}
	addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		return nil, "", "", fmt.Errorf("runsidecar: start github api proxy: %w", err)
	}
	return srv, "http://" + addr, token, nil
}

// startJiraAPIProxy binds a Jira-REST credential proxy on the veth IP,
// resolving the injected auth (Cloud Basic vs Data Center Bearer) from the
// bundle's Jira credential. upstream defaults to the bundle's Jira URL. The
// deployment is returned alongside so the orchestrator's proxy client can speak
// the same REST version the credential's own backend expects — it is derived
// here because the bundle that names it opens only in this process.
func (r *credRuntime) startJiraAPIProxy(hostVethIP, upstream string) (*apiproxy.Server, string, string, jira.Deployment, error) {
	bundle := r.currentBundle()
	if bundle == nil || bundle.Jira == nil {
		return nil, "", "", "", fmt.Errorf("runsidecar: jira api proxy requested but bundle carries no Jira credential")
	}
	if upstream == "" {
		upstream = bundle.Jira.URL
	}
	var auth apiproxy.AuthHeaderSource
	deployment := jira.DeploymentDataCenter
	if bundle.Jira.AuthMethod == "cloud" {
		auth = apiproxy.JiraBasic(bundle.Jira.Email, bundle.Jira.APIToken)
		deployment = jira.DeploymentCloud
	} else {
		auth = apiproxy.JiraBearer(bundle.Jira.PAT)
	}
	token, err := randomToken()
	if err != nil {
		return nil, "", "", "", err
	}
	srv, err := apiproxy.New(apiproxy.Config{
		Provider:         apiproxy.ProviderJira,
		Upstream:         upstream,
		IncomingToken:    token,
		AllowNonLoopback: true,
		AuthHeaderSource: auth,
	})
	if err != nil {
		return nil, "", "", "", fmt.Errorf("runsidecar: construct jira api proxy: %w", err)
	}
	addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		return nil, "", "", "", fmt.Errorf("runsidecar: start jira api proxy: %w", err)
	}
	return srv, "http://" + addr, token, deployment, nil
}

// randomToken mints a per-run placeholder the orchestrator presents to a
// sidecar API proxy. Non-secret — the proxy authenticates the caller against
// it, but the real credential is injected upstream and never travels here.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("runsidecar: mint proxy token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// llmSource is the SigV4 proxy's live re-read of the newest sealed bundle's
// LLM triple (StartRunProxies' llmSource shape): each request reads the
// held bundle so a role-mode run whose STS session credentials the brain
// re-mints mid-run keeps signing with fresh material. A missing or expired
// bundle surfaces an error so the proxy 502s with the refresh-lagging hint
// rather than signing stale, exactly like the in-process path did.
func (r *credRuntime) llmSource(ctx context.Context) (map[string]string, time.Time, error) {
	bundle := r.currentBundle()
	if bundle == nil {
		return nil, time.Time{}, fmt.Errorf("runsidecar: no current credential bundle — refresh lagging")
	}
	if exp := bundle.LLMExpiry(); !exp.IsZero() && exp.Before(time.Now()) {
		return nil, time.Time{}, fmt.Errorf("runsidecar: current LLM credential expired — refresh lagging")
	}
	return bundle.LLM, bundle.LLMExpiry(), nil
}

// recordEgressDenial relays one refused CONNECT up to the orchestrator, which
// holds the DB and writes the audit row. Unconditionally wired (unlike the git
// hooks, which a repo-less run leaves nil): every sandbox gets an egress proxy,
// and repeated denials are the clearest signal available that an agent is
// probing for a way out — the reason this hook exists at all.
func (r *credRuntime) recordEgressDenial(_ context.Context, denied egressproxy.DeniedConnect) {
	r.notifyAudit(agentproc.OpRecordEgressDenial,
		agentproc.RecordEgressDenialArgs{Target: denied.Target, Reason: denied.Reason})
}

// notifyAudit relays one audit record to the orchestrator, which holds the DB
// that turns it into a row. Every audit relay this process sends goes through
// here so that a record lost on the way is reported rather than discarded: this
// process installs no meter provider and opens no listener, so the only place a
// drop can be counted is the side it was headed for.
//
// Void, like every caller expects — the external act it describes already
// happened, and no audit record may fail the thing it is a record of.
func (r *credRuntime) notifyAudit(op string, args any) {
	if err := agentproc.NotifyRelayAudit(r.conn, agentproc.RelayNamespaceCore, op, args); err != nil {
		sidecarLog.Warn("relaying an audit record failed", "op", op, "error", err)
	}
}

// gitProxyConfig builds the git credential proxy's wiring for the sidecar:
// the TokenSource reads the held bundle's per-repo tokens; ProbeCredentials
// checks the bundle carries any GitHub credential; Authorize, RecordDenial,
// and RecordPush relay across the supervision channel to the orchestrator's
// DB-backed gate and audit path (the capless sidecar holds no stores).
func (r *credRuntime) gitProxyConfig(upstream string) *agentproc.GitProxyConfig {
	return &agentproc.GitProxyConfig{
		Upstream: upstream,
		TokenSource: func(_ context.Context, owner, repo string) (gitproxy.Token, error) {
			bundle := r.currentBundle()
			if bundle == nil {
				return gitproxy.Token{}, fmt.Errorf("%w: no current bundle", agentproc.ErrNoGitCredentials)
			}
			token, expiresAt, source := credbundle.ResolveRepoToken(bundle.GitHub, owner, repo)
			if source == credbundle.RepoTokenNone {
				return gitproxy.Token{}, fmt.Errorf("%w: repo %s/%s", agentproc.ErrNoGitCredentials, owner, repo)
			}
			return gitproxy.Token{Value: token, ExpiresAt: expiresAt}, nil
		},
		ProbeCredentials: func(_ context.Context) error {
			bundle := r.currentBundle()
			if bundle == nil || bundle.GitHub == nil || len(bundle.GitHub.RepoTokens) == 0 {
				return fmt.Errorf("%w: bundle carries no GitHub credential", agentproc.ErrNoGitCredentials)
			}
			return nil
		},
		Authorize: func(ctx context.Context, owner, repo string) (gitproxy.Decision, error) {
			var reply agentproc.AuthorizeRepoReply
			if err := agentproc.CallRelay(ctx, r.conn, agentproc.RelayNamespaceCore, agentproc.OpAuthorizeRepo,
				agentproc.AuthorizeRepoArgs{Owner: owner, Repo: repo}, &reply); err != nil {
				// A failed authorize relay must fail closed (deny), never
				// allow-all — a degraded control channel is exactly when a push
				// must not slip through unauthorized.
				return gitproxy.Decision{Allowed: false}, err
			}
			return gitproxy.Decision{
				Allowed:       reply.Allowed,
				AllowedRefs:   reply.AllowedRefs,
				ProtectedRefs: reply.ProtectedRefs,
				DenyReason:    reply.DenyReason,
				DenyMessage:   reply.DenyMessage,
			}, nil
		},
		RecordDenial: func(_ context.Context, denied gitproxy.DeniedGitOp) {
			r.notifyAudit(agentproc.OpRecordDenial,
				agentproc.RecordDenialArgs{
					Owner:  denied.Owner,
					Repo:   denied.Repo,
					Ref:    denied.Ref,
					Op:     denied.Op,
					Reason: denied.Reason,
				})
		},
		RecordPush: func(_ context.Context, push gitproxy.PushedRef) {
			// Relay the completed push up so the orchestrator records the
			// branch artifact / push-failed row. The pre-push hook stands down
			// in this sandbox (StartRunProxies sets PushCaptureProxy), so this
			// relay is the sole push-capture path on the executor — without it
			// no executor push reaches the audit log.
			r.notifyAudit(agentproc.OpRecordPush,
				agentproc.RecordPushArgs{
					Repo:    push.Repo,
					Ref:     push.Ref,
					NewSHA:  push.NewSHA,
					Created: push.Created,
					Status:  push.Status,
				})
		},
	}
}

// shutdown tears down the proxies (best-effort) on run end. Process exit
// frees the address space regardless; this drains in-flight connections.
func (r *credRuntime) shutdown(ctx context.Context) {
	r.mu.Lock()
	handle := r.proxies
	githubAPI := r.githubAPI
	jiraAPI := r.jiraAPI
	ghInjector := r.ghInjector
	agentHost := r.agentHost
	r.proxies, r.githubAPI, r.jiraAPI, r.ghInjector, r.agentHost = nil, nil, nil, nil, nil
	r.mu.Unlock()
	// Drain the socket server first (unblocks its accept loop + removes the
	// socket file) so a graceful teardown leaves no stale /run/tf socket. A
	// SIGKILL teardown skips this; the file is then reclaimed on the
	// orchestrator side — by this cell's teardown, or failing that by the next
	// engagement's bring-up clear — because a successor sidecar at a different
	// per-run uid could not unlink it under the sticky socket root.
	if agentHost != nil {
		_ = agentHost.Close()
	}
	if handle != nil {
		_ = handle.Shutdown(ctx)
	}
	if githubAPI != nil {
		_ = githubAPI.Shutdown(ctx)
	}
	if jiraAPI != nil {
		_ = jiraAPI.Shutdown(ctx)
	}
	if ghInjector != nil {
		_ = ghInjector.Shutdown(ctx)
	} else if r.sharedOrigin != nil {
		// Nothing ever served on the handed-down listener (this run wanted no gh
		// channel), so its Shutdown never ran. Close it here rather than leaving
		// the run's port held until the process exits.
		_ = r.sharedOrigin.Close()
	}
}
