package runsidecar

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/apiproxy"
	"github.com/sky-ai-eng/triage-factory/internal/cabundle"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
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

	// records is the run's one audit-record sender on conn, set beside it.
	// Every record this process sends goes through it — the proxies' and the
	// exec verbs' alike — because a single sender is what keeps them in the
	// order the acts happened.
	records *agentproc.RecordSender

	// sharedOrigin is the run's fake-GHE origin listener, bound by the broker on
	// a privileged port and handed down at launch. nil when none was passed, in
	// which case the gh injector binds an ephemeral port itself and the sandbox's
	// git keeps routing at the git proxy's own address. Set once at startup,
	// before any request is served.
	sharedOrigin net.Listener

	mu         sync.Mutex
	bundle     *credbundle.Bundle // newest unsealed bundle; nil until first seal
	githubBase string             // web base every GitHub lane forwards to; set at proxy start
	proxies    *agentproc.RunProxyHandle
	githubAPI  *apiproxy.Server      // GitHub REST credential proxy; nil unless requested
	jiraAPI    *apiproxy.Server      // Jira REST credential proxy; nil unless the manifest names Jira usable
	linearAPI  *apiproxy.Server      // Linear GraphQL credential proxy; nil unless the manifest names Linear usable
	jiraBase   string                // the Jira site the Jira proxy forwards to; set at proxy start
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
// at construction time), and starts the run's audit-record sender on it.
func (r *credRuntime) setConn(c *sidecarproto.Conn) {
	r.conn = c
	r.records = agentproc.NewRecordSender(c)
}

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
	case sidecarproto.KindDrainRecords:
		return r.records.Drain(ctx), nil
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
	gitUpstream, apiUpstream, err := githubUpstreams(bundle)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.githubBase = gitUpstream
	r.mu.Unlock()
	conversationID := runConversationID(req)

	var git *agentproc.GitProxyConfig
	if req.GitEnabled {
		git = r.gitProxyConfig(gitUpstream)
		git.ConversationID = conversationID
		git.SharedOriginHost = sharedOriginHost
		git.SharedOriginCAPath = cell.CABundlePath
	}

	// The egress denial guidance reads the same two values every GitHub lane
	// was just built from, so the hosts a refusal explains are this run's
	// actual GitHub — GHES and data-residency deployments included, not just
	// the public github.com family.
	ghHosts := egressproxy.GitHubHostsForUpstreams(apiUpstream, gitUpstream)
	handle, env, err := agentproc.StartRunProxies(ctx, req.HostVethIP, bundle.LLM, git, ghHosts, r.recordEgressDenial, r.llmSource, req.IdentityConfigPairs...)
	if err != nil {
		return nil, err
	}

	// The LLM coordinates travel back to the orchestrator's own engine only —
	// no jail is ever pointed at the proxy. The proxy holds the key; the
	// caller holds a per-run placeholder.
	result := sidecarproto.StartProxiesResult{Env: env, LLMEnv: handle.LLMEnv(), JailEndpoints: handle.JailEndpoints()}
	// Surface the git proxy's address + per-run placeholder so the orchestrator
	// routes its OWN pre-sandbox clone through this same proxy — the real token
	// stays here. Empty when no git proxy was started (GitEnabled false).
	result.GitProxyURL, result.GitProxyToken = handle.GitProxy()

	// Read off the bundle that is already open here, so the orchestrator's audit
	// rows can name the credential these proxies inject. Sent unconditionally:
	// every channel the run writes through resolves out of this one bundle, so a
	// single classification answers for all of them.
	result.GitHubCredential = bundle.GitHub.Credential()

	// The GitHub/Jira/Linear API credential proxies the orchestrator's own
	// GetPR + agenthost verbs route through: the orchestrator holds only the
	// placeholder, the sidecar injects the real token on the upstream hop. The
	// GitHub one is requested; Jira and Linear start for exactly the sources
	// the bundle's source manifest names usable — sealed, or unresolved and
	// awaiting the re-seal that brings the credential — whatever source
	// triggered the run. On any failure here, tear down what already bound so
	// a half-started run leaks no listener.
	if req.GitHubAPIEnabled {
		srv, url, token, aerr := r.startGitHubAPIProxy(req.HostVethIP, apiUpstream, conversationID)
		if aerr != nil {
			_ = handle.Shutdown(ctx)
			return nil, aerr
		}
		r.githubAPI = srv
		result.GitHubAPIURL, result.GitHubAPIToken = url, token
	}
	if jiraProxyWanted(bundle) {
		srv, url, token, deployment, aerr := r.startJiraAPIProxy(req.HostVethIP, conversationID)
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
	if bundle.Sources.Usable(eventsource.KindLinear) {
		srv, url, token, aerr := r.startLinearAPIProxy(req.HostVethIP, conversationID)
		if aerr != nil {
			_ = handle.Shutdown(ctx)
			if r.githubAPI != nil {
				_ = r.githubAPI.Shutdown(ctx)
			}
			if r.jiraAPI != nil {
				_ = r.jiraAPI.Shutdown(ctx)
			}
			return nil, aerr
		}
		r.linearAPI = srv
		result.LinearAPIURL, result.LinearAPIToken = url, token
	}

	// The real-gh credential-injector proxy: the TLS listener the sandboxed gh
	// reaches via GH_HOST, injecting the team-set-scoped token upstream while the
	// jail holds only a placeholder — and, when the broker handed one down, the
	// shared origin the sandbox's git was just routed at (see ghChannelWanted).
	var ghLeafPEM []byte
	if ghChannelWanted(req) {
		host, token, leafPEM, gerr := r.startGHInjector(req.HostVethIP, apiUpstream, req.AgentHost.ConversationID, handle.GitHandler())
		if gerr != nil {
			_ = handle.Shutdown(ctx)
			if r.githubAPI != nil {
				_ = r.githubAPI.Shutdown(ctx)
			}
			if r.jiraAPI != nil {
				_ = r.jiraAPI.Shutdown(ctx)
			}
			if r.linearAPI != nil {
				_ = r.linearAPI.Shutdown(ctx)
			}
			return nil, gerr
		}
		result.GHChannelHost, result.GHChannelToken = host, token
		result.JailEndpoints = append(result.JailEndpoints, r.ghInjectorEndpoint(host))
		ghLeafPEM = leafPEM
	}

	// The jail's CA bundle, a step of its own rather than part of any channel:
	// every TLS client in the jail reads it, so every run with a jail gets one,
	// whatever channels it has. It carries the egress interceptor's CA, which
	// every registry the proxy admits is signed by, and the gh injector's leaf
	// when that channel is on. It has to exist before the launch, which mounts
	// it; the bundle is public certificates only, and the CA's key stays in
	// the egress proxy.
	if conversationID != "" {
		if werr := writeCABundle(conversationID, cabundle.WithSystemRoots(handle.EgressCACertPEM(), ghLeafPEM)); werr != nil {
			_ = handle.Shutdown(ctx)
			if r.githubAPI != nil {
				_ = r.githubAPI.Shutdown(ctx)
			}
			if r.jiraAPI != nil {
				_ = r.jiraAPI.Shutdown(ctx)
			}
			if r.linearAPI != nil {
				_ = r.linearAPI.Shutdown(ctx)
			}
			if r.ghInjector != nil {
				_ = r.ghInjector.Shutdown(ctx)
			}
			return nil, fmt.Errorf("runsidecar: write the jail's CA bundle: %w", werr)
		}
	}

	// Host the exec-verb socket server in THIS capless process rather than the
	// orchestrator (the relocation): the hostile-input parser now runs in the
	// per-run jail, holding no db.Stores and no secret store. Its LocalClient's
	// DB effects relay to the orchestrator over the supervision channel; its
	// gh/jira/linear verbs build clients against the API proxies just bound
	// above, so the real credential never leaves this process's proxy hop.
	if req.AgentHost != nil {
		if aerr := r.startAgentHost(req.AgentHost, result); aerr != nil {
			_ = handle.Shutdown(ctx)
			if r.githubAPI != nil {
				_ = r.githubAPI.Shutdown(ctx)
			}
			if r.jiraAPI != nil {
				_ = r.jiraAPI.Shutdown(ctx)
			}
			if r.linearAPI != nil {
				_ = r.linearAPI.Shutdown(ctx)
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
// the orchestrator) and whose gh/jira/linear verbs route through the proxies this
// sidecar just bound (holding only per-run placeholders). It creates the
// agenthost socket in the run's own socket directory (/run/tf/<conversationID>/),
// which the broker bind-mounts into the jail, and grants it to the run's jail
// gid — which the directory, setgid to that gid, gives the socket at birth.
// Identity comes from the
// orchestrator's AgentHostInfo but is never trusted for org-scoping: every
// relayed op is re-bound to the orchestrator's own ConversationInfo.
func (r *credRuntime) startAgentHost(ai *sidecarproto.AgentHostInfo, proxies sidecarproto.StartProxiesResult) error {
	info := agenthost.ConversationInfo{
		OrgID:            ai.OrgID,
		UserID:           ai.UserID,
		TeamID:           ai.TeamID,
		ConversationID:   ai.ConversationID,
		IsEventTriggered: ai.EventTriggered,
		RunURL:           ai.RunURL,
	}
	proxyCreds := &agenthost.ProxyCredentials{
		GitHubCredential: proxies.GitHubCredential,
		GitHubAPIURL:     proxies.GitHubAPIURL,
		GitHubAPIToken:   proxies.GitHubAPIToken,
		JiraAPIURL:       proxies.JiraAPIURL,
		JiraAPIToken:     proxies.JiraAPIToken,
		JiraDeployment:   proxies.JiraDeployment,
		LinearAPIURL:     proxies.LinearAPIURL,
		LinearAPIToken:   proxies.LinearAPIToken,
		GitProxyURL:      proxies.GitProxyURL,
		GitProxyToken:    proxies.GitProxyToken,
	}
	// The runtime reads the held bundle in-process: a provider handler (Slack)
	// selects its bot token from it, and the verbs explain a refusal from its
	// source manifest — live reads, so a mid-run brain re-seal is picked up.
	// The orchestrator is never asked for a secret.
	srv := agenthost.NewServerWithRuntime(agenthost.NewRelayRuntime(r.conn, r.records, info, r), proxyCreds)
	hd, _, err := startAgentHostSocket(srv, info.ConversationID)
	if err != nil {
		return fmt.Errorf("runsidecar: start agenthost: %w", err)
	}
	r.agentHost = hd
	return nil
}

// The two privileged filesystem effects startProxies has, behind vars so a test
// can drive the wiring around them. Both land under the root-only per-run socket
// root (/run/tf) — one writes the jail's CA bundle there, the other binds the
// exec-verb socket — so an unprivileged test process cannot take either path,
// and a test that needs the surrounding logic has to stand in for them.
//
// The seams live here, in the consumer, deliberately: cmd/exec/agenthost resolves
// those paths as constants and is the privileged side of this boundary. Making
// its path resolution mutable so another package's test can retarget it would
// trade a real invariant for test convenience.
//
// Both grant what they create to the run's jail gid, which this process derives
// from its own uid (ownJailGID) — so a test standing in for them stands in for
// that derivation too, and never needs to run at a sidecar uid.
var (
	writeCABundle = func(conversationID string, bundlePEM []byte) error {
		gid, err := ownJailGID()
		if err != nil {
			return err
		}
		return agenthost.WriteCABundle(conversationID, bundlePEM, gid)
	}
	startAgentHostSocket = func(srv *agenthost.Server, conversationID string) (*agenthost.HostDaemon, sandbox.Mount, error) {
		gid, err := ownJailGID()
		if err != nil {
			return nil, sandbox.Mount{}, err
		}
		return agenthost.StartWithServer(srv, conversationID, gid)
	}
)

// ownJailGID is the jail gid of the run this sidecar serves, derived from the
// sidecar's own uid: the broker launches it at SidecarUID(idx), so the uid
// names the group the run's files are granted to. A process outside the
// sidecar band is not a run's sidecar and has no jail to grant anything to.
func ownJailGID() (int, error) {
	gid, ok := sandbox.SidecarJailGID(os.Getuid())
	if !ok {
		return 0, fmt.Errorf("runsidecar: uid %d is not a sidecar uid; no jail to grant the run's files to", os.Getuid())
	}
	return gid, nil
}

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

// runConversationID is the conversation the request names, for the proxies' log
// attribution. Empty when the request carries no agenthost identity, which
// costs those lines their conversation and nothing else.
func runConversationID(req sidecarproto.StartProxiesBody) string {
	if req.AgentHost == nil {
		return ""
	}
	return req.AgentHost.ConversationID
}

// githubUpstreams derives this run's two GitHub hosts from the sealed bundle:
// the web base the git proxy forwards to, and the REST mount the API proxy and
// the gh injector prepend. A bundle with no GitHub half carries no GitHub
// credential, so its lanes forward unauthenticated to the deployment's GitHub
// (ghbase.DefaultBaseURL). A GitHub half with no host is refused: its tokens
// were issued by some GitHub, and the default may not be that one. Neither
// value is ever empty, so no lane below carries a default of its own.
func githubUpstreams(b *credbundle.Bundle) (git, api string, err error) {
	if b.GitHub == nil {
		base := ghbase.DefaultBaseURL()
		return base, ghbase.APIBase(base), nil
	}
	base := ghbase.CanonicalBaseURL(b.GitHub.BaseURL)
	if base == "" {
		return "", "", errors.New("runsidecar: the bundle's GitHub credentials name no GitHub host")
	}
	return base, ghbase.APIBase(base), nil
}

// heldGitHub returns the held bundle's GitHub half when it was resolved for
// the GitHub this run's lanes forward to, and nil when it was not; ok is false
// when no bundle has arrived. The lanes are fixed at proxy start, while the
// bundle is re-sealed through the run and read live: a re-seal resolved after
// the org reconnected GitHub on another URL carries tokens that host issued,
// and attaching them here would send them to this one.
func (r *credRuntime) heldGitHub() (gh *credbundle.GitHubCreds, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bundle == nil {
		return nil, false
	}
	gh = r.bundle.GitHub
	if gh == nil {
		return nil, true
	}
	if base := ghbase.CanonicalBaseURL(gh.BaseURL); base == "" || base != r.githubBase {
		return nil, true
	}
	return gh, true
}

// startGHInjector stands up the real-gh credential-injector proxy: generates the
// per-run self-signed TLS cert (private key stays here), serves TLS, and returns
// the public cert for the jail's CA bundle, which startProxies writes once every
// channel is up. The injector's TokenSource reads the held bundle's single
// team-set-scoped CLIToken live, so a mid-run re-seal is picked up; the
// write-audit callback relays every forwarded write to the orchestrator (which
// writes the audit row).
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
func (r *credRuntime) startGHInjector(hostVethIP, upstream, conversationID string, gitHandler http.Handler) (string, string, []byte, error) {
	cert, certPEM, err := ghinjector.GenerateCert(hostVethIP)
	if err != nil {
		return "", "", nil, fmt.Errorf("runsidecar: generate gh-injector cert: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return "", "", nil, err
	}
	srv, err := ghinjector.New(r.ghInjectorConfig(upstream, cert, token, conversationID, gitHandler))
	if err != nil {
		return "", "", nil, fmt.Errorf("runsidecar: construct gh injector: %w", err)
	}
	if ln := r.sharedOrigin; ln != nil {
		if _, err := srv.Serve(ln); err != nil {
			return "", "", nil, fmt.Errorf("runsidecar: serve gh injector on the shared origin: %w", err)
		}
		r.ghInjector = srv
		// The bare host, no port — see sharedOriginHost.
		return r.sharedOriginHost(), token, certPEM, nil
	}
	addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		return "", "", nil, fmt.Errorf("runsidecar: start gh injector: %w", err)
	}
	r.ghInjector = srv
	return addr, token, certPEM, nil
}

// ghInjectorEndpoint is the gh injector's "host:port" as the jail dials it.
// On the shared origin GHChannelHost is the bare host, its port implied by
// https, so the endpoint is the origin listener's own address; otherwise it is
// the address the injector bound.
func (r *credRuntime) ghInjectorEndpoint(host string) string {
	if r.sharedOrigin != nil {
		return r.sharedOrigin.Addr().String()
	}
	return host
}

// ghInjectorConfig builds the gh channel's per-run wiring, the sibling of
// gitProxyConfig: the TokenSource reads the held bundle's single
// team-set-scoped CLIToken live (so a mid-run re-seal is picked up), and the
// two callbacks relay to the orchestrator, which holds the DB: the observed
// write as a queued audit record, the authorization as a call.
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
			gh, ok := r.heldGitHub()
			if !ok {
				return "", fmt.Errorf("runsidecar: gh channel: %w", credbundle.ErrNoBundle)
			}
			if gh == nil || gh.CLIToken == nil || gh.CLIToken.Token == "" {
				return "", fmt.Errorf("runsidecar: gh channel: %w", credbundle.ErrNoCLIToken)
			}
			return gh.CLIToken.Token, nil
		},
		ObserveWrite: func(_ context.Context, w ghinjector.ObservedWrite) {
			// Queued, not awaited: the request already transited, and the
			// orchestrator holds the DB that turns it into an audit row.
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
				NodeID:         w.NodeID,
				URL:            w.URL,
				Errored:        w.Errored,
				ResponseUnread: w.ResponseUnread,
			}
			// A GraphQL write is named in its request, which only this process
			// ever saw — the orchestrator receives the act's name or nothing.
			args.GraphQL = ghWriteFactsToWire(w.GraphQL)
			r.recordAudit(agentproc.OpRecordGHWrite, args)
		},
		AuthorizeWrite: func(ctx context.Context, req ghwrite.Request, _ ghwrite.Refusal) bool {
			// A Call, not a record: the agent's request is held until the
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
		Operation:   f.Operation,
		Mutations:   f.Fields,
		Subject:     f.Subject,
		Unreadable:  f.Unreadable,
		NamesReview: f.NamesReview,
	}
}

// startGitHubAPIProxy binds a GitHub-REST credential proxy on the veth IP.
// Its per-repo TokenSource reads the held bundle, so the real installation
// token never leaves the sidecar; the returned placeholder is what the
// orchestrator's ghclient presents. upstream is the bundle-derived REST mount.
func (r *credRuntime) startGitHubAPIProxy(hostVethIP, upstream, conversationID string) (*apiproxy.Server, string, string, error) {
	token, err := randomToken()
	if err != nil {
		return nil, "", "", err
	}
	srv, err := apiproxy.New(apiproxy.Config{
		Provider:         apiproxy.ProviderGitHub,
		Upstream:         upstream,
		IncomingToken:    token,
		AllowNonLoopback: true,
		ConversationID:   conversationID,
		TokenSource:      r.githubAPIToken,
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

// githubAPIToken is the GitHub REST proxy's TokenSource: the held bundle's
// token for the repository the request targets, read live so a mid-run re-seal
// is picked up.
func (r *credRuntime) githubAPIToken(_ context.Context, owner, repo string) (string, error) {
	gh, ok := r.heldGitHub()
	if !ok {
		return "", fmt.Errorf("runsidecar: github api proxy: %w", credbundle.ErrNoBundle)
	}
	tok, _, source := credbundle.ResolveRepoToken(gh, owner, repo)
	if source == credbundle.RepoTokenNone {
		return "", fmt.Errorf("runsidecar: github api proxy, %s/%s: %w", owner, repo, credbundle.ErrNoRepoToken)
	}
	return tok, nil
}

// jiraProxyWanted reports whether this run gets a Jira proxy: the bundle's
// manifest names Jira usable, and the bundle names the Jira site to forward
// to. The site travels with the credential, so a Jira the brain could not
// resolve would arrive with no site and no proxy; the brain fails the seal on a
// Jira resolve failure rather than recording it unresolved, so a usable Jira
// always carries one.
func jiraProxyWanted(b *credbundle.Bundle) bool {
	if !b.Sources.Usable(eventsource.KindJira) {
		return false
	}
	if b.Jira == nil || b.Jira.URL == "" {
		sidecarLog.Warn("the bundle's source manifest names Jira usable but carries no Jira site; starting no Jira proxy")
		return false
	}
	return true
}

// startJiraAPIProxy binds a Jira-REST credential proxy on the veth IP,
// forwarding to the Jira site the held bundle names. The injected auth (Cloud
// Basic vs Data Center Bearer) is resolved per request (jiraAuthHeader), so
// each request answers with the bundle held then. The deployment is returned
// alongside so the orchestrator's proxy client can speak the same REST version
// the credential's own backend expects — it is derived here because the
// bundle that names it opens only in this process.
func (r *credRuntime) startJiraAPIProxy(hostVethIP, conversationID string) (*apiproxy.Server, string, string, jira.Deployment, error) {
	bundle := r.currentBundle()
	if bundle == nil || bundle.Jira == nil {
		return nil, "", "", "", fmt.Errorf("runsidecar: jira api proxy requested: %w", credbundle.SourceMiss(nil, eventsource.KindJira))
	}
	upstream := bundle.Jira.URL
	deployment := jira.DeploymentDataCenter
	if bundle.Jira.AuthMethod == "cloud" {
		deployment = jira.DeploymentCloud
	}
	token, err := randomToken()
	if err != nil {
		return nil, "", "", "", err
	}
	r.mu.Lock()
	r.jiraBase = upstream
	r.mu.Unlock()
	srv, err := apiproxy.New(apiproxy.Config{
		Provider:         apiproxy.ProviderJira,
		Upstream:         upstream,
		IncomingToken:    token,
		AllowNonLoopback: true,
		ConversationID:   conversationID,
		AuthHeaderSource: r.jiraAuthHeader,
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

// jiraAuthHeader is the Jira proxy's AuthHeaderSource: the Authorization
// value for the held bundle's Jira credential, read on every request so a
// bundle the brain re-seals mid-run is used from the next request on. A
// manifest that no longer names Jira sealed answers with its state. A
// credential resolved for another Jira site than the one this proxy forwards
// to is never sent there: the org moved sites since the run started.
func (r *credRuntime) jiraAuthHeader(ctx context.Context) (string, error) {
	bundle := r.currentBundle()
	if bundle == nil {
		return "", fmt.Errorf("runsidecar: jira api proxy: %w", credbundle.ErrNoBundle)
	}
	sources := bundle.Sources.At(time.Now())
	if e, _ := sources.Entry(eventsource.KindJira); e.State != domain.SourceSealed {
		return "", fmt.Errorf("runsidecar: jira api proxy: %w", credbundle.SourceMiss(sources, eventsource.KindJira))
	}
	jc := bundle.Jira
	if jc == nil {
		return "", errors.New("runsidecar: jira api proxy: the manifest names Jira sealed but the bundle carries no Jira credential")
	}
	r.mu.Lock()
	base := r.jiraBase
	r.mu.Unlock()
	if jc.URL != base {
		return "", errors.New("runsidecar: jira api proxy: the held Jira credential is for another Jira site")
	}
	if jc.AuthMethod == "cloud" {
		return apiproxy.JiraBasic(jc.Email, jc.APIToken)(ctx)
	}
	return apiproxy.JiraBearer(jc.PAT)(ctx)
}

// linearUpstream is the origin the Linear proxy forwards to: Linear's API
// host, which serves every workspace, so neither the bundle nor the frame
// names it. The path is the GraphQL endpoint the client posts to, which the
// proxy forwards alone. A var only so a test can point the proxy at a fake.
var linearUpstream = strings.TrimSuffix(linear.DefaultEndpoint, "/graphql")

// startLinearAPIProxy binds a Linear GraphQL credential proxy on the veth IP.
// It starts whether the bundle's manifest names Linear sealed or unresolved:
// the brain seals a bundle without the credential when it could not resolve
// one, and a run should not fail to start over a source it may never use, nor
// stay without Linear once a re-seal brings the credential. The injected auth
// is resolved per request (linearAuthHeader), so each request answers with the
// bundle held then.
func (r *credRuntime) startLinearAPIProxy(hostVethIP, conversationID string) (*apiproxy.Server, string, string, error) {
	token, err := randomToken()
	if err != nil {
		return nil, "", "", err
	}
	srv, err := apiproxy.New(apiproxy.Config{
		Provider:         apiproxy.ProviderLinear,
		Upstream:         linearUpstream,
		IncomingToken:    token,
		AllowNonLoopback: true,
		ConversationID:   conversationID,
		AuthHeaderSource: r.linearAuthHeader,
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("runsidecar: construct linear api proxy: %w", err)
	}
	addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		return nil, "", "", fmt.Errorf("runsidecar: start linear api proxy: %w", err)
	}
	return srv, "http://" + addr, token, nil
}

// linearAuthHeader is the Linear proxy's AuthHeaderSource: the Authorization
// value for the held bundle's Linear credential, read on every request so a
// bundle the brain re-seals mid-run is used from the next request on. A
// personal API key is the whole value, with no scheme; an app install's access
// token is a Bearer.
//
// A manifest that does not name Linear sealed answers with its state. That
// includes an access token past its expiry, which the manifest reads as
// unresolved (domain.SourceManifest.At): the brain seals a token with more life
// left than the bundle goes between seals, so one past its expiry here means
// the brain could not refresh it, and the agent is told to retry rather than
// sent a refusal Linear would give as if the install were gone.
func (r *credRuntime) linearAuthHeader(context.Context) (string, error) {
	bundle := r.currentBundle()
	if bundle == nil {
		return "", fmt.Errorf("runsidecar: linear api proxy: %w", credbundle.ErrNoBundle)
	}
	sources := bundle.Sources.At(time.Now())
	if e, _ := sources.Entry(eventsource.KindLinear); e.State != domain.SourceSealed {
		return "", fmt.Errorf("runsidecar: linear api proxy: %w", credbundle.SourceMiss(sources, eventsource.KindLinear))
	}
	lc := bundle.Linear
	if lc == nil {
		return "", errors.New("runsidecar: linear api proxy: the manifest names Linear sealed but the bundle carries no Linear credential")
	}
	switch linear.AuthMethod(lc.AuthMethod) {
	case linear.AuthMethodAPIKey:
		if lc.APIKey == "" {
			return "", errors.New("runsidecar: linear api proxy: empty api key")
		}
		return lc.APIKey, nil
	case linear.AuthMethodAppInstall:
		if lc.AccessToken == "" {
			return "", errors.New("runsidecar: linear api proxy: empty access token")
		}
		return "Bearer " + lc.AccessToken, nil
	default:
		return "", fmt.Errorf("runsidecar: linear api proxy: unknown auth method %q", lc.AuthMethod)
	}
}

// ProviderCreds is the held bundle's sealed keyed set for a provider namespace
// (agenthost.HeldBundle), read live so a mid-run brain re-seal is picked up.
func (r *credRuntime) ProviderCreds(namespace string) (json.RawMessage, bool) {
	b := r.currentBundle()
	if b == nil {
		return nil, false
	}
	return b.ProviderCreds(namespace)
}

// Sources is the held bundle's source manifest as it stands now
// (agenthost.HeldBundle): the exec verbs explain a refusal from it, and the
// help index annotates itself from it. nil before the first bundle.
func (r *credRuntime) Sources() domain.SourceManifest {
	b := r.currentBundle()
	if b == nil {
		return nil
	}
	return b.Sources.At(time.Now())
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
	r.recordAudit(agentproc.OpRecordEgressDenial,
		agentproc.RecordEgressDenialArgs{Target: denied.Target, Reason: denied.Reason})
}

// recordAudit queues one audit record for the orchestrator, which holds the DB
// that turns it into a row. It goes through the run's one record sender, the
// same one the exec verbs record through, so records reach the orchestrator in
// the order the acts happened and are resent until it has applied them. A
// record the sender gives up on is counted at the end-of-run drain: this
// process installs no meter provider and opens no listener, so the only place a
// drop can be counted is the side it was headed for.
//
// Void, like every caller expects — the external act it describes already
// happened, and no audit record may fail the thing it is a record of.
func (r *credRuntime) recordAudit(op string, args any) {
	if err := r.records.Send(agentproc.RelayNamespaceCore, op, args); err != nil {
		sidecarLog.Warn("queueing an audit record failed", "op", op, "error", err)
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
			gh, ok := r.heldGitHub()
			if !ok {
				return gitproxy.Token{}, fmt.Errorf("%w: %w", agentproc.ErrNoGitCredentials, credbundle.ErrNoBundle)
			}
			token, expiresAt, source := credbundle.ResolveRepoToken(gh, owner, repo)
			if source == credbundle.RepoTokenNone {
				return gitproxy.Token{}, fmt.Errorf("%w: repo %s/%s: %w", agentproc.ErrNoGitCredentials, owner, repo, credbundle.ErrNoRepoToken)
			}
			return gitproxy.Token{Value: token, ExpiresAt: expiresAt}, nil
		},
		ProbeCredentials: func(_ context.Context) error {
			if gh, _ := r.heldGitHub(); gh == nil || len(gh.RepoTokens) == 0 {
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
			r.recordAudit(agentproc.OpRecordDenial,
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
			r.recordAudit(agentproc.OpRecordPush,
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
	linearAPI := r.linearAPI
	ghInjector := r.ghInjector
	agentHost := r.agentHost
	r.proxies, r.githubAPI, r.jiraAPI, r.linearAPI, r.ghInjector, r.agentHost = nil, nil, nil, nil, nil, nil
	r.mu.Unlock()
	// Drain the socket server first (unblocks its accept loop + removes the
	// socket file) so a graceful teardown leaves no stale /run/tf socket. A
	// SIGKILL teardown skips this; the run's socket directory, file and all, is
	// then removed by the cell's teardown, or failing that replaced by the next
	// engagement's bring-up — both the cap-broker's, since this process can
	// write nowhere in the socket root but its own run's directory.
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
	if linearAPI != nil {
		_ = linearAPI.Shutdown(ctx)
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
