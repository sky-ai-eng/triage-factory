// Package ghinjector is the per-run credential-injector proxy for the real
// `gh` CLI (TFAC-669). It is the git-proxy pattern extended to gh: the agent's
// gh talks to this listener via GH_HOST (base-URL redirect) holding only a
// per-run placeholder token; the injector strips the placeholder and injects
// the run's real GitHub credential on the upstream hop. The real credential —
// an App-org's single team-repo-set-scoped installation token, or the org PAT —
// lives only in the capless sidecar that runs this proxy and never enters the
// jail's env, argv, or any file (placeholder only).
//
// # Two traffic policies
//
// Which repositories a request reaches is carried by the credential where the
// credential can carry it: a multi-mode App org's token is minted scoped to
// the run's authorized repo set, so a request for a repo outside that set gets
// GitHub's own 404 masking, verbatim. Every other credential — a token org's,
// and any in local mode — reaches further than the run does, and for those the
// repository gate (repos.go) refuses a request that names a repository outside
// the run's set. That gate is courtesy isolation, not a boundary: it stops the
// ordinary case, and repos.go lists what it does not see. A request for a repo
// the run may work in is forwarded whatever it asks of it — with one
// exception, the second policy.
//
// Three families of write are refused before they are forwarded: submitting a
// review, creating a repository, and opening a pull request. So is any GraphQL
// request whose act could not be established at all, since a policy keyed on
// the act's name is walked through by anything that stops the act being named. What is refused, and the
// reasoning for each — including why MERGING is deliberately not among them —
// lives in internal/ghcli/ghwrite alongside the classifier the audit log reads;
// gate.go here is only the enforcement. There is no authorization mechanism and
// no exemption: a mission that needs a gated act cannot perform it and finishes
// by saying so.
//
// This is the choke point, which is why the policy is here rather than at the
// command-level matcher the delegated runtime also applies. That matcher sees
// what a model typed; this sees what actually happened — and any client holding
// the per-run placeholder reaches this proxy, curl included, while the SDK
// runtime has no matcher seam at all.
//
// Everything else the injector does is unchanged: it maps the two GHE-shaped
// path prefixes gh emits to the org's real API, and selects the one credential
// to inject.
//
// # Requests are forwarded unaltered, and read in one place
//
// No request is ever ALTERED. rewrite touches the URL and the Authorization
// header and nothing else, and reading a body neither changes it nor consumes
// it: what the agent sent is what is forwarded, byte for byte, including the
// bodies read below. Buffering adds no loss of its own — which is the whole of
// what it can promise, since a body whose own read fails mid-stream was never
// going to arrive whole regardless of who was in the middle.
//
// Requests are READ in exactly one place: a POST to the GraphQL endpoint, whose
// body is buffered under maxRequestBody and parsed for the name of the mutation
// it performs, or, when it performs none, for the repositories its query names
// and the pull requests, issues, branches and node ids it addresses (see "Read
// touches" below). That is a deliberate, bounded
// exception to reading no request at all, and it is not free — this
// is the one process that holds a run's live GitHub credential while parsing
// hostile upstream output, and it now also parses outbound documents an agent
// composed from that same text.
//
// The exception exists because the alternative was a governance hole rather
// than a policy: nearly every write gh's porcelain performs — a comment, a
// close, a merge, a review — is a POST to /api/graphql, indistinguishable on
// the wire from `gh pr view`. Excluding it did not make those writes safe, only
// unlogged. What bounds the cost is the shape of the read: the envelope's three
// known members, the top level of the operation that runs — including the
// fragments it spreads into that top level, since moving a mutation behind one
// would otherwise be the cheapest way to perform a write nothing could name —
// and a node id from the variables. For a mutation, no argument value and no
// free text is interpreted (internal/ghcli/ghwrite gqldoc.go states what the reader
// refuses to look at), and a document that does not parse is recorded as
// unreadable rather than guessed at. For a query, five argument values are
// read and no others: a repository's owner and name, an object's number, a
// branch's name and a node's id, each accepted only in the shape of what it
// names (internal/ghcli/ghwrite read.go).
//
// Anything beyond that shape still has to re-derive the cost. REST request
// bodies remain unread — a REST path already names its act — so the classifier
// deliberately cannot tell a PATCH that edits a pull request from one that
// closes it.
//
// # What this channel cannot see at all
//
// GH_HOST redirects the base URL, so it reaches only the requests gh BUILDS
// from that base. A command that takes an absolute URL out of a response body
// and follows it — the release asset commands do this, using the upload_url and
// the release object's own url — leaves this channel entirely, and neither its
// act nor its outcome can be recorded here. Naming those shapes in the write
// classifier would therefore be a promise it cannot keep.
//
// In the sandbox they do not merely go unlogged, they fail: the jail has no
// route to those hosts and no keychain to authenticate them with, so the escape
// is a fail-closed one. It is worth knowing about anyway, because "the injector
// observes the agent's GitHub writes" is true only of the traffic the injector
// is in front of, and a probe run outside the jail — on a host with a real
// keyring — will see such a command succeed with nothing recorded.
//
// # TLS, but no interception
//
// gh forces https to any GH_HOST and rejects scheme prefixes, so the injector
// serves TLS with a per-run self-signed certificate (GenerateCert), published
// through the trust file SSL_CERT_FILE points at (cabundle.WithSystemRoots —
// the system roots plus this run's leaf, since that env var is process-global).
// There is no TLS interception of GitHub anywhere — the
// injector terminates gh's connection with its own per-run cert and opens its
// own TLS connection to the real upstream.
//
// # Path surface
//
// gh, treating GH_HOST as a GitHub Enterprise host, emits exactly two prefixes:
// REST under /api/v3/* and GraphQL at /api/graphql (spike-verified across gh
// api / pr view / pr list / pr diff / mutations). The injector maps /api/v3/<x>
// to the REST upstream and /api/graphql to the sibling GraphQL endpoint derived
// from it, mirroring internal/github's own derivation.
//
// # The shared origin: git on the same listener
//
// A real GHE host serves the API and git smart-HTTP on one address, and so does
// this listener when Config.GitHandler is set: git paths are routed to the run's
// gitproxy handler, everything else takes the injection path above. Routing is by
// path and happens before the caller-auth gate, because the two halves
// authenticate differently — git presents the git proxy's per-run Basic password,
// gh presents GH_ENTERPRISE_TOKEN — and each half checks its own.
//
// That arrangement is what makes gh's repo inference work inside the jail. gh
// resolves a repository by matching the worktree's git remotes against GH_HOST;
// while git was routed at a separate proxy address, no remote could ever match,
// and every repo-contextual command needed an explicit -R. Pointing both at one
// origin is the fix, and it is why the run's listener lives on port 443: gh
// strips the port from a remote's host before comparing, so a ported GH_HOST is
// unmatchable no matter what the remote says.
//
// Nothing about the git half is reimplemented here. It is gitproxy's own
// handler — its ref gate, its base-branch push policy, its per-repo authorize
// and audit — served behind a second front door.
//
// # Write audit
//
// Exec-verb self-reporting doesn't exist on this channel, so the injector
// reports every write it forwards through a caller-supplied callback
// (ObserveWrite) the sidecar turns into an orchestrator relay, and every write
// leaves exactly one report whichever transport it took. The deliverables a run
// leaves behind — its pull requests, its reviews — are never minted from this
// channel: the shapes that would create one are refused here and belong to the
// exec verbs, which record what the agent proposed alongside what landed.
//
// REST is read from the response's own request record — method, path, status —
// so those requests stay unread. The shapes the shared classifier
// (internal/ghcli/ghwrite) marks as creates — a posted comment, a review-thread reply
// — additionally have their RESPONSE body parsed for the new object's id and
// link, buffered under a cap and stitched back in front of the stream when the
// cap is passed, so the audit row reaches parity with the equivalent exec
// verb's. Every other shape is fully described by its path and costs no
// buffering at all.
//
// GraphQL needs both ends of the hop. The act is named only in the request, so
// the capture in Handler reads it there; the outcome is only in the response,
// and not in its status line — the endpoint refuses a mutation with a 200
// carrying an errors array — so the audit reads that too and files a refused
// write as an attempt. A request whose document names no mutation is a read and
// reports nothing at all, which is nearly all of this endpoint's traffic.
//
// The injector makes requests of its own, all reads, through the upstream and
// credential the write itself used, before reporting an accepted write: it
// reads what GitHub calls the objects the write names (fillIdentity): the repository's id, and the pull
// request's or issue's node id and kind for a write that names one by number,
// which is what the audit row's entity and repository are resolved from. A
// GraphQL mutation whose subject is always a pull request or always an issue,
// and whose input is bound to the node id its variables name, is identified by
// that node id, so only its repository is read; each repository is read once
// per run.
//
// The callback is not a policy: reporting a write neither permits nor prevents
// it. It fires after the request has already transited, so a refused request
// never reaches it — its one row is the refusal, written by the gate.
//
// # Read touches
//
// A run's touched entities include the pull requests and issues it read, and
// the exec verbs report their own. A read through this channel is reported by
// a second callback (ObserveRead), under the exec verbs' rule: an object the
// request addressed is touched, one returned in a set is not. A REST GET
// addresses the pull request or issue its path numbers, or the one a comment
// its path names by the comment's own id is on, and a GraphQL query the ones it
// selects by number under a repository it names, and the ones it selects by
// node id (ghwrite.ClassifyRead, ghwrite.ClassifyCommentRead,
// ghwrite.ParseGraphQLRead). A list and a search address none.
//
// A comment and a node id name no number, so the injector asks GitHub which
// object they are on or name, with the credential the read was made under:
// it reads the comment by its id over REST, or asks GraphQL what the node id
// names (a pull request, an issue, or a comment, review or review thread on
// one), and touches that object as if the read had named it by number. A node
// id that names anything else touches nothing. Each comment and each node id
// is asked about once per run, and the response the agent receives is not
// read for either.
//
// A GraphQL lookup of a branch's pull requests addresses the branch. It is how
// `gh pr view`, `gh pr status` and `gh pr checks` find the pull request of the
// current branch when they are given no number. The injector looks the branch
// up itself, asking GitHub for the open pull requests whose head is that branch
// of the repository the query names, and touches the pull request if GitHub
// answers exactly one; none or several touch nothing. The lookup asks for the
// repository's own branches, so a pull request from a fork's branch is not
// found. The request does not say whose branch gh meant, so when gh is looking
// at a fork's branch whose name a branch of the repository shares, the pull
// request open on the repository's branch is the one touched. The lookup is
// made once per branch per run, whatever it answers, and the response the
// agent receives is not read for it.
//
// What the request spelled is not what is reported. Once the upstream has
// accepted the read, the injector reads what GitHub calls the object, with the
// credential the read was made under, exactly as it does for a write
// (fillIdentity), and reports the object with those ids. An object GitHub does
// not answer for is not reported, so a request that only spells an object,
// without reading anything, touches what a real read of it would and nothing
// else. Each object is read and reported once per run; that one read is made
// before the agent's response is released, and every later read of the object
// costs nothing of the injector's own. A pull request a branch lookup found,
// and an object found through a comment or a node id, is reported under the
// same rule, once, whichever way the run reached it.
//
// A read is not an act and leaves no audit row.
package ghinjector

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/bindaddr"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/credmiss"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
)

// restPrefix is the GHE REST path prefix gh emits; graphqlPath is the GraphQL
// path. These are the only two surfaces the injector routes (spike finding 4).
const (
	restPrefix  = "/api/v3"
	graphqlPath = "/api/graphql"
)

// maxBufferedBody caps how much of a write's response body the injector will
// BUFFER to read the created object's coordinates or a GraphQL outcome. Those
// responses are a few KB; the cap only guards against a pathological upstream.
// A body past the cap is never truncated — the audit goes without it and the
// body streams on: a declared Content-Length over the cap is skipped without
// reading anything at all, and one discovered mid-read has its consumed prefix
// stitched back in front of the untouched remainder. The agent's response is
// always delivered whole; the audit's detail is the only thing that degrades.
const maxBufferedBody = 1 << 20

// injectorLog carries what this package says out loud, which is only its
// refusals and the requests it found no credential for. Everything it merely
// observes travels as a relayed audit row and is silent here; a refusal is a
// decision this process made, and the line is written locally so it owes
// nothing to the relay hop that records it.
var injectorLog = logging.Component("ghinjector")

// maxRequestBody caps how much of a GraphQL request body the injector will
// buffer to name the act it performs. It is sized to clear anything real:
// GitHub caps a comment at 64 KiB and gh's largest documents are a few, so a
// megabyte is far past every legitimate mutation while still bounding what one
// request can make this process hold.
//
// A body past it is forwarded whole and audited as unreadable. That direction
// matters: the alternative — dropping the record — would make padding a request
// past the cap the way to write without being logged.
const maxRequestBody = 1 << 20

// TokenSource supplies the single real GitHub credential to inject on every
// request — the team-set-scoped installation token (App orgs) or the org PAT.
// Read once per request with no proxy-side caching so a mid-run brain re-seal is
// picked up; an error (or empty token) is answered as a credential miss
// (internal/proxy/credmiss), never a silently-unauthenticated forward.
type TokenSource func(ctx context.Context) (string, error)

// ObservedWrite is one mutating REST request the injector forwarded, with the
// outcome the upstream returned. It is the classifier package's Observation
// under this package's name: the sidecar relays it and the orchestrator turns
// it into an audit row, and one type across that hop is what keeps the two ends
// from drifting.
type ObservedWrite = ghwrite.Observation

// ObservedRead is one pull request or issue a read through this channel
// addressed, with what GitHub answered the injector for it. It is the
// classifier package's ReadObservation under this package's name, for the
// reason ObservedWrite is its Observation: the sidecar relays it and the
// orchestrator records the run's touch from it.
type ObservedRead = ghwrite.ReadObservation

// Config bundles the per-run injector inputs. One Server per run.
type Config struct {
	// Upstream is the org's real REST API base — "https://api.github.com" or a
	// GHES "{host}/api/v3". The GraphQL endpoint is derived from it. Required.
	Upstream string

	// TokenSource resolves the single real credential to inject. Required.
	TokenSource TokenSource

	// IncomingToken is the per-run placeholder every request must present (gh
	// sends it as "Authorization: token <placeholder>"). Constant-time compared;
	// a sibling run holds a different one and so cannot spend this run's
	// credential. Empty disables the check (loopback/test path).
	IncomingToken string

	// Cert is the per-run self-signed certificate the injector serves TLS with
	// (from GenerateCert). Required — gh forces https.
	Cert tls.Certificate

	// ObserveWrite, when non-nil, is called for every mutating REST request the
	// injector forwarded, whatever the outcome — the write audit's callback.
	// Called inline on the response path, so the callback must not block. An
	// accepted write has what GitHub calls its objects read first
	// (fillIdentity), so the observation carries them.
	ObserveWrite func(ctx context.Context, w ObservedWrite)

	// ObserveRead, when non-nil, is called once per run for each pull request
	// or issue a read the upstream accepted addressed, or found by the branch
	// lookup it addressed, with what GitHub calls it (ObservedRead). Called
	// inline on the response path, so the callback must not block.
	ObserveRead func(ctx context.Context, r ObservedRead)

	// AuthorizeWrite decides the gated shapes and records their refusals. See
	// the type's own doc: nil refuses every gated shape without recording one,
	// which is the fail-closed reading of "no decision source is wired" and
	// never the production shape — both modes supply it.
	AuthorizeWrite AuthorizeWrite

	// Repositories, when non-nil, is the run's authorized repository set the
	// repository gate refuses a request outside of (repos.go). nil applies no
	// gate, which leaves the repositories a request reaches to the credential.
	Repositories RepositorySource

	// DenyRepository, when non-nil, is called for each request the repository
	// gate refuses, before the refusal is answered, to record its denial.
	// Called inline, so the callback must not block.
	DenyRepository func(ctx context.Context, d DeniedRepository)

	// ConversationID names the conversation this injector serves, for log
	// attribution only. It is never sent upstream and never reaches an audit
	// row — those are attributed by the orchestrator from its own conversation
	// identity, never from anything the sidecar names. Empty is fine; it costs
	// a log line its conversation.
	ConversationID string

	// AllowNonLoopback opts into binding a non-loopback address (the veth IP the
	// sandbox reaches). Loopback-only by default, exactly like the sibling
	// proxies.
	AllowNonLoopback bool

	// GitHandler, when non-nil, serves the git smart-HTTP paths on this same
	// listener — the run's single fake-GHE origin, mirroring a real GHE host
	// where the API and git share one address. Production wires it to the run's
	// gitproxy handler, so the ref gate, the base-branch push policy, and the
	// credential injection are the ones that package already owns: this is a
	// front door in front of that handler, not a second implementation of it.
	//
	// Requests are routed by path (gitproxy.IsGitSmartHTTPPath) BEFORE the API
	// caller-auth gate below, because the two halves authenticate differently —
	// git presents the git proxy's per-run Basic password, gh presents
	// GH_ENTERPRISE_TOKEN — and each half validates its own. nil keeps this
	// listener API-only, which is what local mode and every existing test get.
	GitHandler http.Handler
}

// Server is one per-run injector instance.
type Server struct {
	cfg        Config
	restURL    *url.URL
	graphqlURL *url.URL
	proxy      *httputil.ReverseProxy

	requestCount atomic.Int64

	// misses answers and logs the requests no credential could be found for.
	misses *credmiss.Responder

	// reads is the REST upstream the injector's own reads go through, with the
	// credential every forwarded request is given, queries the GraphQL
	// upstream its node lookups go through, with the same credential, and
	// identities the run's cache of what GitHub calls the objects its writes
	// and reads name.
	reads      ghclient.Getter
	queries    ghclient.Querier
	identities *ghclient.IdentityReader

	// addressed is the objects this run's reads have already been reported
	// for (ghwrite.ObjectRef.Key), so each is read and reported once, and
	// lookedUp the branches the injector has looked up for them
	// (ghwrite.BranchRef.Key), so each is looked up once.
	addressedMu sync.Mutex
	addressed   map[string]bool
	lookedUp    map[string]bool

	listener net.Listener
	httpSrv  *http.Server
	serveErr chan error
}

// New validates the config and constructs a Server without listening.
func New(cfg Config) (*Server, error) {
	if cfg.TokenSource == nil {
		return nil, errors.New("ghinjector: TokenSource is required")
	}
	rest, err := parseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	gql, err := url.Parse(graphqlUpstream(cfg.Upstream))
	if err != nil {
		return nil, fmt.Errorf("ghinjector: derive graphql upstream: %w", err)
	}

	s := &Server{
		cfg:        cfg,
		restURL:    rest,
		graphqlURL: gql,
		misses:     credmiss.NewResponder("ghinjector", cfg.ConversationID, injectorLog),
		identities: ghclient.NewIdentityReader(),
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
	}
	s.reads = ghclient.NewTokenGetter(rest.String(), cfg.TokenSource, proxyTransport{s})
	s.queries = ghclient.NewTokenQuerier(gql.String(), cfg.TokenSource, proxyTransport{s})
	return s, nil
}

// parseUpstream validates the REST base: scheme + host, https unless loopback.
func parseUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("ghinjector: Upstream is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ghinjector: parse upstream %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("ghinjector: upstream %q missing scheme or host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ghinjector: upstream %q must not include query or fragment", raw)
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("ghinjector: upstream %q must use https (loopback http allowed for tests)", raw)
		}
	}
	return u, nil
}

// graphqlUpstream derives the GraphQL endpoint from the REST base:
//
//	https://api.github.com      → https://api.github.com/graphql
//	https://<host>/api/v3       → https://<host>/api/graphql
//
// Dotcom is detected by an EXACT hostname compare, not a substring test: a
// GHES host that merely contains the dotcom name (api.github.com.example.com,
// or a lookalike an org mis-configures) must keep routing GraphQL to its own
// host, never to github.com. A base that won't parse falls through to the
// sibling-path derivation, which is the safe default (it stays on whatever host
// the REST base named).
func graphqlUpstream(restBase string) string {
	trimmed := strings.TrimRight(restBase, "/")
	if u, err := url.Parse(trimmed); err == nil && strings.EqualFold(u.Hostname(), dotcomAPIHost) {
		return "https://api.github.com/graphql"
	}
	return strings.TrimSuffix(trimmed, "/v3") + "/graphql"
}

// dotcomAPIHost is the public GitHub REST API host, matched exactly.
const dotcomAPIHost = "api.github.com"

// authCtxKey carries the resolved upstream Authorization value from the Handler
// wrapper to the Rewrite hook (off the inbound header set, so it never logs).
type authCtxKey struct{}

// Handler exposes the proxy as an http.Handler (production Start wraps it in
// TLS; tests can drive it via httptest.NewUnstartedServer + StartTLS).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Git first, and before the API caller-auth gate: a git request carries
		// the git proxy's credential, not gh's placeholder, so checking it here
		// would 401 every fetch. The git handler runs its own equivalent gate.
		if s.routesToGit(r) {
			s.cfg.GitHandler.ServeHTTP(w, r)
			return
		}
		if !s.callerAuthorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Read before forwarding, because after the hop the act is unrecoverable:
		// a GraphQL response says nothing about which mutation produced it, and
		// an aliased one says nothing at all. The gate below needs it for the
		// same reason — on this transport the act is named in the request or
		// nowhere.
		facts, addressed := s.captureGraphQL(r)
		// Before the credential is resolved, so a refused request never reaches
		// the credential pipeline at all.
		if s.refused(w, r, facts) {
			return
		}
		tok, err := s.cfg.TokenSource(r.Context())
		if err == nil && tok == "" {
			err = errors.New("ghinjector: token source returned an empty token")
		}
		if err != nil {
			// The answer names the reason and nothing else: the source error
			// may carry credential-setup detail.
			s.misses.Respond(w, err)
			return
		}
		// After the credential, which the gate's own lookup of a name it does
		// not know is made with.
		if s.refusedRepository(w, r, addressed) {
			return
		}
		ctx := context.WithValue(r.Context(), authCtxKey{}, tok)
		if facts != nil {
			ctx = context.WithValue(ctx, graphQLFactsKey{}, facts)
		}
		if !addressed.Empty() {
			ctx = context.WithValue(ctx, graphQLReadsKey{}, addressed)
		}
		r = r.WithContext(ctx)
		s.proxy.ServeHTTP(w, r)
	})
}

// graphQLFactsKey carries what the request envelope disclosed from the capture
// in Handler to the audit in ModifyResponse, where the outcome is finally
// known. The request is the only place the act is named and the response is the
// only place its outcome is, so one write's row needs both ends of the hop.
type graphQLFactsKey struct{}

// graphQLReadsKey carries the objects, branches and node ids a GraphQL query
// addressed from the capture in Handler to the read touch in ModifyResponse,
// which reports them once the upstream has accepted the request.
type graphQLReadsKey struct{}

// captureGraphQL buffers a GraphQL request body, re-presents it unchanged, and
// reads it: the act out of a mutation, or the pull requests, issues, branches
// and node ids a query addresses. Both are empty for everything else — a
// different endpoint, a read that addresses nothing.
//
// facts is one value rather than a (facts, ok) pair, because both the audit
// and the refusal policy branch on "is there anything to say about this
// request", and a boolean that only ever means "the pointer is non-nil" is one
// place for the two to disagree.
//
// The mutation read runs whether or not the write audit is wired, because the
// refusal policy reads the same facts and a run with no audit callback is still
// a run that may not merge. The addressed read runs only for a request that is
// not a write, and only when something reads it: the read touch, or the
// repository gate.
//
// The buffering is unconditional for POSTs to the GraphQL endpoint, because
// whether a body is a read cannot be known before reading it. What stays cheap
// is the parse: ParseGraphQLRequest decodes the envelope and stops at a
// document with no mutation keyword in it, and ParseGraphQLRead stops at one
// that never mentions a repository or a node, so query traffic that addresses
// nothing costs one copy and two JSON decodes and never reaches the document
// reader.
func (s *Server) captureGraphQL(r *http.Request) (facts *ghwrite.GraphQLFacts, addressed ghwrite.AddressedReads) {
	if r.Method != http.MethodPost || !isGraphQLPath(r.URL.Path) {
		return nil, addressed
	}

	body, unread := bufferRequest(r)
	if unread != "" {
		// No document to read, so nothing can say what this request performs.
		// It is refused on exactly that ground (see ghwrite.gateGraphQL) and the
		// reason travels verbatim, because the two causes are not alike — a cap
		// this side chose, against a caller that stopped sending — and a
		// refusal in this band that is not vanishingly rare means something
		// operational rather than someone probing.
		return &ghwrite.GraphQLFacts{Unreadable: unread}, addressed
	}

	write, isWrite := ghwrite.ParseGraphQLRequest(body)
	if isWrite {
		return &write, addressed
	}
	if s.cfg.ObserveRead == nil && s.cfg.Repositories == nil {
		return nil, addressed
	}
	return nil, ghwrite.ParseGraphQLRead(body)
}

// isGraphQLPath reports whether an INBOUND path addresses the GraphQL endpoint,
// matching what rewrite routes there.
func isGraphQLPath(path string) bool {
	return path == graphqlPath || strings.HasPrefix(path, graphqlPath+"/")
}

// bufferRequest reads a request body into memory and re-presents it, returning
// the bytes. unread is empty when the whole body was buffered; otherwise it
// names why not, in the classifier's own vocabulary, and body is nil.
//
// The two reasons are kept apart because they are different facts and the
// guarantee differs between them:
//
//   - Past the cap. The read itself was clean, so the consumed prefix is
//     stitched back in front of an intact remainder and the request reaches
//     GitHub byte-for-byte. Only the audit degrades. (A declared length over the
//     cap settles this before any I/O, leaving the body untouched entirely.)
//   - The read broke mid-stream. The prefix is stitched back the same way, so
//     this proxy still drops nothing — but the remainder is a stream that has
//     already failed and will most likely fail again on the forward. What can be
//     promised here is only that buffering added no loss of its own; a body the
//     caller could not deliver is not one anything downstream can make whole.
//
// On the whole-body path GetBody is set alongside, which is what the buffered
// body owes the transport: an in-memory body may be replayed, and a request
// that can be replayed is one net/http may safely retry on a stale connection
// rather than failing the agent's call.
func bufferRequest(r *http.Request) (body []byte, unread string) {
	if r.Body == nil {
		return nil, ""
	}
	if r.ContentLength > maxRequestBody {
		return nil, ghwrite.GraphQLOverCap
	}
	// One byte past the cap, so "over the cap" is distinguishable from "exactly
	// the cap" — ReadAll on a LimitReader reports no error at the limit.
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(buf) > maxRequestBody {
		r.Body = &prefixedBody{r: io.MultiReader(bytes.NewReader(buf), r.Body), c: r.Body}
		if err != nil {
			return nil, ghwrite.GraphQLRequestUnread
		}
		return nil, ghwrite.GraphQLOverCap
	}
	// The original body is left for the server to close, as it always does — it
	// is drained to EOF here, so nothing is holding the connection.
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
	return buf, ""
}

// routesToGit decides which half of the shared origin owns this request.
//
// The two API prefixes gh emits win outright, ahead of any git shape: they are
// this listener's other half, they are matched by prefix (not by suffix, as the
// git shapes must be), and a git-looking tail under one of them —
// /api/v3/…/info/refs — is an API path with an odd name, not a git request. No
// real GHE serves a repository at "api/v3", so nothing legitimate is lost by
// letting the prefix decide.
//
// Everything else that looks like git smart-HTTP routes to the git handler,
// well-formed or not: see gitproxy.IsGitSmartHTTPPath for why a malformed git
// path must reach the git gate's refusal rather than fall through to the API
// credential path.
func (s *Server) routesToGit(r *http.Request) bool {
	if s.cfg.GitHandler == nil {
		return false
	}
	p := r.URL.Path
	if p == graphqlPath || strings.HasPrefix(p, graphqlPath+"/") ||
		p == restPrefix || strings.HasPrefix(p, restPrefix+"/") {
		return false
	}
	return gitproxy.IsGitSmartHTTPPath(r.Method, p)
}

// callerAuthorized reports whether the request presents the per-run placeholder.
// gh sends "Authorization: token <placeholder>"; Bearer and Basic are also
// accepted defensively. An empty IncomingToken disables the gate (test path).
func (s *Server) callerAuthorized(r *http.Request) bool {
	if s.cfg.IncomingToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	presented := auth
	switch {
	case strings.HasPrefix(auth, "token "):
		presented = strings.TrimPrefix(auth, "token ")
	case strings.HasPrefix(auth, "Bearer "):
		presented = strings.TrimPrefix(auth, "Bearer ")
	case strings.HasPrefix(auth, "Basic "):
		_, presented, _ = r.BasicAuth()
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.IncomingToken)) == 1
}

// rewrite maps the GHE-shaped request onto the org's real API and swaps in the
// real credential. No traffic policy: every request is forwarded; only the base
// URL and the Authorization header change.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	inPath := pr.In.URL.Path
	if inPath == graphqlPath || strings.HasPrefix(inPath, graphqlPath+"/") {
		setTarget(pr, s.graphqlURL, "")
	} else {
		rest := strings.TrimPrefix(inPath, restPrefix)
		setTarget(pr, s.restURL, rest)
	}

	// Suppress proxy-chain headers (we deliberately do not SetXForwarded).
	pr.Out.Header.Del("X-Forwarded-For")
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Header.Del("X-Forwarded-Proto")
	pr.Out.Header.Del("Forwarded")
	pr.Out.Header.Del("Proxy-Authorization")
	pr.Out.Header.Del("Proxy-Connection")

	tok, _ := pr.In.Context().Value(authCtxKey{}).(string)
	if tok == "" {
		// Fail closed: strip the caller placeholder so the request goes
		// anonymous (upstream rejects) rather than forwarding the placeholder.
		pr.Out.Header.Del("Authorization")
		return
	}
	// gh's native scheme for installation tokens; api.github.com and GHES both
	// accept "token <ghs_...>".
	pr.Out.Header.Set("Authorization", "token "+tok)
}

// setTarget points the outgoing request at base with base's path joined ahead
// of extraPath, and fixes the Host header. Mirrors the effect of SetURL but
// lets the injector strip the /api/v3 prefix and pick REST vs GraphQL per
// request.
func setTarget(pr *httputil.ProxyRequest, base *url.URL, extraPath string) {
	pr.Out.URL.Scheme = base.Scheme
	pr.Out.URL.Host = base.Host
	pr.Out.URL.Path = singleJoiningSlash(base.Path, extraPath)
	// RawPath left empty so net/http re-derives escaping from Path; the incoming
	// query is preserved on Out.URL.RawQuery by ReverseProxy.
	pr.Out.URL.RawPath = ""
	pr.Out.Host = base.Host
	pr.Out.Header.Set("Host", base.Host)
}

// singleJoiningSlash joins a and b with exactly one slash between them (the
// stdlib httputil helper, inlined since it is unexported).
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash && b != "":
		return a + "/" + b
	}
	return a + b
}

// writeMethods is the set of REST methods that mutate. A method outside it is
// a read, and reads are outside the audit charter (a refused GET is
// indistinguishable from an ordinary 404); touchReads reports the object an
// accepted one addressed.
var writeMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPatch:  true,
	http.MethodPut:    true,
	http.MethodDelete: true,
}

// auditWrite reports one mutating REST request to the write-audit callback,
// whatever the upstream returned. GraphQL is skipped by the caller's flag —
// auditGraphQLWrite owns that transport, and running both would put two rows on
// one request.
//
// Method, path, and status are already-parsed fields of the response's request
// record, so a REST request is still never read. body
// is the buffered RESPONSE body, non-nil only when the caller determined this
// shape creates an object worth naming; an empty or unparseable one costs the
// row its id and link, never the row itself.
func (s *Server) auditWrite(resp *http.Response, graphql bool, body []byte) {
	if s.cfg.ObserveWrite == nil || graphql || !writeMethods[resp.Request.Method] {
		return
	}
	w := ObservedWrite{
		Method: resp.Request.Method,
		Path:   resp.Request.URL.Path,
		Status: resp.StatusCode,
	}
	if len(body) > 0 {
		w.ExternalID, w.NodeID, w.URL = parseCreatedObject(body)
	}
	s.fillIdentity(&w)
	s.cfg.ObserveWrite(context.Background(), w)
}

// auditGraphQLWrite reports one GraphQL mutation to the write-audit callback.
// Both ends of the hop meet here: the act comes from the request the capture
// read, the outcome from the response.
//
// body is the buffered response, empty when it was past the cap or broke
// mid-read. Losing it costs the row its link and its knowledge of whether the
// write was refused, so the record says the outcome went unread rather than
// claiming a success nobody observed — and says that in its own words, since
// "the server refused this" and "we could not tell" are different facts.
func (s *Server) auditGraphQLWrite(resp *http.Response, facts *ghwrite.GraphQLFacts, body []byte) {
	if s.cfg.ObserveWrite == nil {
		return
	}
	w := ObservedWrite{
		Method:  resp.Request.Method,
		Path:    resp.Request.URL.Path,
		Status:  resp.StatusCode,
		GraphQL: facts,
	}
	if len(body) > 0 {
		read := ghwrite.ReadGraphQLResponse(body, *facts)
		w.Errored, w.URL, w.ExternalID, w.NodeID = read.Errored, read.URL, read.ExternalID, read.NodeID
	} else {
		w.ResponseUnread = true
	}
	s.fillIdentity(&w)
	s.cfg.ObserveWrite(context.Background(), w)
}

// proxyTransport sends the injector's own reads through the transport the
// proxy forwards with, read when each request is sent, so they reach the
// upstream the way a forwarded request does.
type proxyTransport struct{ s *Server }

func (t proxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt := t.s.proxy.Transport; rt != nil {
		return rt.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// identityReadTimeout bounds the reads fillIdentity makes: the agent's
// response waits on them, and past this the write is reported unlinked rather
// than held.
const identityReadTimeout = 5 * time.Second

// fillIdentity reads what GitHub calls the objects an accepted write names,
// with the credential the write was made under (ghclient.IdentityReader), and
// sets it on the observation: the repository's id, and, for a write that names
// a pull request or issue by number, its node id and kind. A GraphQL mutation
// whose subject is always a pull request or always an issue
// (Shape.SubjectKind), and whose input is bound to the node id its variables
// name (Shape.SubjectBound), is identified by that node id, and only the
// repository its response locates is read. Any other GraphQL write is read
// like a REST write, from the object its response's url locates, and one
// whose response locates nothing carries no identity: a subject the document
// may not have used is never taken for the object, since a request can spell
// its arguments inline beside a variable naming some other node id. A refused
// write names nothing the audit row is linked to, and is not read for. A
// failed read is logged and leaves the identity empty, which records the
// write unlinked; the write is reported either way.
func (s *Server) fillIdentity(w *ObservedWrite) {
	if !w.Succeeded() {
		return
	}
	var owner, repo, nodeID, kind string
	var number int
	if shape, ok := ghwrite.Resolve(*w); ok {
		owner, repo, number = shape.Owner, shape.Repo, shape.Number
		if shape.SubjectKind != "" && shape.SubjectBound {
			nodeID, kind = shape.Subject, shape.SubjectKind
		}
	} else if w.GraphQL == nil {
		owner, repo, _ = strings.Cut(ghwrite.RepoPath(w.Path), "/")
	}
	if nodeID == "" && (owner == "" || repo == "") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), identityReadTimeout)
	defer cancel()
	var id domain.GitHubIdentity
	var err error
	switch {
	case nodeID != "":
		id = domain.GitHubIdentity{NodeID: nodeID, Kind: kind}
		if owner != "" && repo != "" {
			var r domain.GitHubIdentity
			if r, err = s.identities.Repository(ctx, s.reads, owner, repo); err == nil {
				id.RepositoryID, id.Repository = r.RepositoryID, r.Repository
			}
		}
	case number > 0:
		id, err = s.identities.Object(ctx, s.reads, owner, repo, number)
	default:
		id, err = s.identities.Repository(ctx, s.reads, owner, repo)
	}
	if err != nil {
		injectorLog.Warn("read what GitHub calls a write's objects failed; recording the write unlinked",
			"conversation", s.cfg.ConversationID, "repo", owner+"/"+repo, "number", number, "error", err)
	}
	w.Identity = id
}

// auditWantsBody reports whether the write audit needs this response's body:
// the callback is wired, the request was a REST write the upstream accepted,
// and the classifier says the response names an object that did not exist
// before it. Everything else is described by its path alone.
func (s *Server) auditWantsBody(resp *http.Response, graphql bool) bool {
	if s.cfg.ObserveWrite == nil || graphql || !writeMethods[resp.Request.Method] {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	shape, ok := ghwrite.Classify(resp.Request.Method, resp.Request.URL.Path)
	return ok && shape.CreatesObject
}

// modifyResponse bumps the request counter and reports every mutating request
// to the write audit, buffering the body first for the shapes whose outcome or
// created object lives in it, then streams it on unchanged. Never returns an
// error (that would 502 the agent on a successful upstream write) — an audit
// that could not read its body records what it could and moves on.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	if resp.Request == nil {
		return nil
	}
	graphql := resp.Request.URL.Path == s.graphqlURL.Path

	auditingCreate := s.auditWantsBody(resp, graphql)
	// Non-nil only for a GraphQL request the capture in Handler read as a write.
	facts, _ := resp.Request.Context().Value(graphQLFactsKey{}).(*ghwrite.GraphQLFacts)

	var buf []byte
	if auditingCreate || facts != nil {
		buf, _ = bufferBody(resp)
	}

	if facts != nil {
		// A GraphQL write's outcome is not in its status line, so this arm needs
		// the body whatever the act was — an errors array is how the endpoint
		// refuses, and a row that read only the 200 would report a refused merge
		// as a merge.
		s.auditGraphQLWrite(resp, facts, buf)
	} else {
		// The REST audit runs whatever the buffering produced — it covers every
		// outcome, a refused write and every mutating method alike — but it is
		// handed a body only when the classifier said this shape's response
		// names a created object.
		s.auditWrite(resp, graphql, buf)
	}
	s.touchReads(resp, graphql)
	return nil
}

// touchReads reports the pull requests and issues a read the upstream accepted
// addressed: the one a REST GET's path names, or the one the comment it names
// is on, or the ones the capture in Handler read out of a GraphQL query, the
// pull request open on each branch the query looked pull requests up by, and
// the object each node id it selected names. A request the upstream refused
// addressed nothing the run was shown. GraphQL answers a query for an object
// that does not exist with a 200, which the identity read in touchRead
// settles, the branch lookup in touchBranch settles a branch's, and the node
// lookup in touchNode a node id's.
//
// The agent's response waits on the injector's reads for what the run has not
// addressed before, so they share one identityReadTimeout however many the
// request named.
func (s *Server) touchReads(resp *http.Response, graphql bool) {
	if s.cfg.ObserveRead == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	var reads ghwrite.AddressedReads
	var comment ghwrite.CommentRef
	var commented bool
	if graphql {
		reads, _ = resp.Request.Context().Value(graphQLReadsKey{}).(ghwrite.AddressedReads)
	} else if ref, ok := ghwrite.ClassifyRead(resp.Request.Method, resp.Request.URL.Path); ok {
		reads.Objects = []ghwrite.ObjectRef{ref}
	} else {
		comment, commented = ghwrite.ClassifyCommentRead(resp.Request.Method, resp.Request.URL.Path)
	}
	if reads.Empty() && !commented {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), identityReadTimeout)
	defer cancel()
	if commented {
		s.touchComment(ctx, comment)
	}
	for _, ref := range reads.Objects {
		s.touchRead(ctx, ref)
	}
	for _, ref := range reads.Branches {
		s.touchBranch(ctx, ref)
	}
	for _, id := range reads.Nodes {
		s.touchNode(ctx, id)
	}
}

// touchRead reports one object a read addressed, the first time the run
// addresses it. It reads what GitHub calls the object with the credential the
// read was made under (ghclient.IdentityReader), the way fillIdentity does for
// a write, and reports it only on an answer: an object GitHub does not answer
// for, or a read that failed, is not reported and not remembered, so a later
// read of the same object asks again. No later read of a reported object costs
// a read of the injector's own. ctx bounds the identity read.
func (s *Server) touchRead(ctx context.Context, ref ghwrite.ObjectRef) {
	s.addressedMu.Lock()
	seen := s.addressed[ref.Key()]
	s.addressedMu.Unlock()
	if seen {
		return
	}
	id, err := s.identities.Object(ctx, s.reads, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		// Ordinary for a query that named a number GitHub has nothing at.
		injectorLog.Debug("read what GitHub calls an addressed object failed; not recording the touch",
			"conversation", s.cfg.ConversationID, "repo", ref.Owner+"/"+ref.Repo, "number", ref.Number, "error", err)
		return
	}
	s.reportRead(ref, id)
}

// touchComment reports the object a read of a comment by its own id is on. It
// reads the comment with the credential the read was made under
// (ghclient.IdentityReader.CommentSubject), for the number of the pull request
// or issue it is on, and reports that object as touchRead does, spelled under
// the repository the request named. The reader remembers each comment it has
// read, so a comment is read once per run; one GitHub does not answer for, or
// whose read failed, is not reported and not remembered. ctx bounds both
// reads.
func (s *Server) touchComment(ctx context.Context, c ghwrite.CommentRef) {
	number, err := s.identities.CommentSubject(ctx, s.reads, c.Owner, c.Repo, c.Space, c.ID)
	if err != nil {
		injectorLog.Debug("read the object an addressed comment is on failed; not recording the touch",
			"conversation", s.cfg.ConversationID, "repo", c.Owner+"/"+c.Repo, "comment", c.ID, "space", c.Space, "error", err)
		return
	}
	s.touchRead(ctx, ghwrite.ObjectRef{Owner: c.Owner, Repo: c.Repo, Number: number})
}

// touchNode reports the object a read named by node id: the pull request or
// issue it names, or the one the comment, review or review thread it names is
// on. It asks GitHub's GraphQL API which object that is, with the credential
// the read was made under (ghclient.IdentityReader.NodeSubject), and reports
// the object as touchRead does, under the repository name GitHub answered. A node id that
// names anything else touches nothing. The reader remembers each node id it
// had an answer for, so a node id is asked about once per run; one GitHub
// answers null for, or whose lookup failed, is not remembered. ctx bounds the
// lookup and the read.
func (s *Server) touchNode(ctx context.Context, id string) {
	subject, err := s.identities.NodeSubject(ctx, s.queries, id)
	if err != nil {
		injectorLog.Debug("look up the object an addressed node id names failed; not recording a touch",
			"conversation", s.cfg.ConversationID, "node", id, "error", err)
		return
	}
	ref := ghwrite.ObjectRef{Owner: subject.Owner, Repo: subject.Repo, Number: subject.Number}
	// The repository goes into the path of the identity read, so it is held
	// to the shape a request's spelling is.
	if subject.Number == 0 || !ghwrite.ValidRepository(ref.Owner, ref.Repo) {
		return
	}
	s.touchRead(ctx, ref)
}

// touchBranch reports the pull request open on a branch a read looked pull
// requests up by, the first time the run looks the branch up. The injector
// asks GitHub itself, with the credential the read was made under
// (ghclient.IdentityReader.OpenPullForBranch), for the open pull requests
// whose head is that branch of the repository, and reports the pull request
// when there is exactly one. What the upstream answered the agent's own lookup
// is not read: a read's response is streamed to the agent unbuffered, and its
// shape is the agent's own selection, which can alias any field under the name
// a reader would look for.
//
// An answer is remembered for the run whatever it was, so a branch is looked
// up once: a pull request opened on it after that is not found by a later
// lookup in the run. A failed lookup is not remembered, and a later read of the
// branch asks again. ctx bounds the lookup.
func (s *Server) touchBranch(ctx context.Context, ref ghwrite.BranchRef) {
	key := ref.Key()
	s.addressedMu.Lock()
	seen := s.lookedUp[key]
	s.addressedMu.Unlock()
	if seen {
		return
	}
	pull, err := s.identities.OpenPullForBranch(ctx, s.reads, ref.Owner, ref.Repo, ref.Branch)
	if err != nil {
		injectorLog.Debug("look up the pull request open on an addressed branch failed; not recording a touch",
			"conversation", s.cfg.ConversationID, "repo", ref.Owner+"/"+ref.Repo, "branch", ref.Branch, "error", err)
		return
	}
	// Two concurrent first lookups of one branch both reach here; one goes on.
	s.addressedMu.Lock()
	if s.lookedUp == nil {
		s.lookedUp = map[string]bool{}
	}
	answered := s.lookedUp[key]
	s.lookedUp[key] = true
	s.addressedMu.Unlock()
	if answered || pull.Number == 0 {
		return
	}
	s.reportRead(ghwrite.ObjectRef{Owner: ref.Owner, Repo: ref.Repo, Number: pull.Number}, pull.Identity)
}

// reportRead reports one object, with what GitHub calls it, unless the run has
// already reported it. Two concurrent first reads of one object both reach
// here; one reports.
func (s *Server) reportRead(ref ghwrite.ObjectRef, id domain.GitHubIdentity) {
	key := ref.Key()
	s.addressedMu.Lock()
	if s.addressed == nil {
		s.addressed = map[string]bool{}
	}
	reported := s.addressed[key]
	s.addressed[key] = true
	s.addressedMu.Unlock()
	if reported {
		return
	}
	s.cfg.ObserveRead(context.Background(), ObservedRead{ObjectRef: ref, Identity: id})
}

// parseCreatedObject reads the id, node id, and html url of the object a create
// response names. All empty when the body doesn't parse or carries no id — a
// 2xx without a recognizable object still happened, so the caller records the
// act and lets the deep link degrade.
func parseCreatedObject(body []byte) (externalID, nodeID, url string) {
	var o struct {
		ID      int64  `json:"id"`
		NodeID  string `json:"node_id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &o); err != nil || o.ID == 0 {
		return "", "", ""
	}
	return strconv.FormatInt(o.ID, 10), o.NodeID, o.HTMLURL
}

// bufferBody reads the whole response body into memory and re-presents it to
// the caller, returning the buffered bytes. ok is false when the body is larger
// than the injector will buffer or the read broke mid-stream; in both cases the
// response is restored to a readable state (consumed prefix stitched back in
// front of the remainder, original body kept as the Closer) and the audit goes
// without it.
//
// Past the cap, that restoration is exact and the agent's response is delivered
// whole, with the audit's detail the only thing degraded. On a broken read the
// same stitch happens and this proxy still drops nothing of its own, but the
// remainder is a stream that already failed — an upstream body that cannot be
// read is not one anything in the middle can deliver whole.
func bufferBody(resp *http.Response) ([]byte, bool) {
	// A declared length past the cap settles the outcome before any I/O: the
	// body would be skipped anyway, so read none of it and leave it streaming
	// straight through to the agent. ContentLength is -1 when unknown (chunked),
	// which falls through to the read below.
	if resp.ContentLength > maxBufferedBody {
		return nil, false
	}

	// Read one byte past the cap so "body exceeded the cap" is distinguishable
	// from "body is exactly the cap" (ReadAll on a LimitReader reports no error
	// when it stops at the limit).
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBufferedBody+1))
	if err != nil || len(buf) > maxBufferedBody {
		resp.Body = &prefixedBody{r: io.MultiReader(bytes.NewReader(buf), resp.Body), c: resp.Body}
		return nil, false
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}

// prefixedBody re-presents an already-partially-consumed response body: reads
// deliver the buffered prefix followed by the live remainder, while Close still
// closes the original body (which was never closed, so the remainder is intact).
type prefixedBody struct {
	r io.Reader
	c io.Closer
}

func (b *prefixedBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *prefixedBody) Close() error               { return b.c.Close() }

// Start binds addr and serves TLS with the per-run cert until Shutdown. Returns
// the bound "host:port" (no scheme) — the value GH_HOST is set to.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("ghinjector: already started; create a new Server per run")
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if !s.cfg.AllowNonLoopback {
		if err := bindaddr.RequireLoopback(addr); err != nil {
			return "", fmt.Errorf("ghinjector: %w", err)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("ghinjector: listen on %s: %w", addr, err)
	}
	return s.Serve(ln)
}

// Serve takes ownership of an ALREADY-BOUND listener and serves TLS on it until
// Shutdown, returning its "host:port". It exists because the run's shared origin
// must answer on port 443 — gh drops the port when it matches a git remote
// against GH_HOST, so a ported GH_HOST can never resolve a repo from the tree —
// and the process that runs this injector is capless by construction and cannot
// bind a privileged port. The cap-broker binds it and passes the fd down.
//
// Ownership transfers: Shutdown closes the listener. The AllowNonLoopback check
// Start applies is the binder's to make here — it already chose the address.
func (s *Server) Serve(ln net.Listener) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("ghinjector: already started; create a new Server per run")
	}
	if ln == nil {
		return "", errors.New("ghinjector: Serve requires a bound listener")
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{s.cfg.Cert}},
	}
	// No recover: net/http recovers a handler panic per connection, so nothing
	// a request does reaches this frame; ServeTLS itself only accepts, and the
	// buffered send + close cannot panic (one sender, one close).
	go func() {
		// Certs already live in TLSConfig, so ServeTLS's file args are empty.
		err := s.httpSrv.ServeTLS(ln, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
		close(s.serveErr)
	}()
	return ln.Addr().String(), nil
}

// Err returns the first unexpected Serve error (channel closes on exit).
func (s *Server) Err() <-chan error { return s.serveErr }

// Shutdown stops serving and drains in-flight requests. Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// RequestCount returns how many upstream responses the proxy has observed.
func (s *Server) RequestCount() int64 { return s.requestCount.Load() }
