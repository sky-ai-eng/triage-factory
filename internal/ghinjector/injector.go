// Package ghinjector is the per-run credential-injector proxy for the real
// `gh` CLI (TFAC-669). It is the git-proxy pattern extended to gh: the agent's
// gh talks to this listener via GH_HOST (base-URL redirect) holding only a
// per-run placeholder token; the injector strips the placeholder and injects
// the run's real GitHub credential on the upstream hop. The real credential —
// an App-org's single team-repo-set-scoped installation token, or the org PAT —
// lives only in the capless sidecar that runs this proxy and never enters the
// jail's env, argv, or any file (placeholder only).
//
// # One traffic policy
//
// Most of the scope is carried by the credential: an App-org token is minted
// scoped to the team's authorized repo set, so a request for a repo outside
// that set gets GitHub's own 404 masking, verbatim. There is no path allowlist,
// and a request for a repo the run may work in is forwarded whatever it asks
// of it — with one exception, which is the whole of this proxy's traffic policy.
//
// Two families of write are refused before they are forwarded: submitting a
// review, and creating a repository. So is any GraphQL request whose act could
// not be established at all, since a policy keyed on the act's name is walked
// through by anything that stops the act being named. What is refused, and the
// reasoning for each — including why MERGING is deliberately not among them —
// lives in internal/ghwrite alongside the classifier the audit log reads;
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
// it performs. That is a deliberate, bounded exception to what used to be a
// blanket invariant, and it is not free — this is the one process that holds a
// run's live GitHub credential while parsing hostile upstream output, and it
// now also parses outbound documents an agent composed from that same text.
//
// The exception exists because the alternative was a governance hole rather
// than a policy: nearly every write gh's porcelain performs — a comment, a
// close, a merge, a review — is a POST to /api/graphql, indistinguishable on
// the wire from `gh pr view`. Excluding it did not make those writes safe, only
// unlogged. What bounds the cost is the shape of the read: the envelope's three
// known members, the top level of the operation that runs — including the
// fragments it spreads into that top level, since moving a mutation behind one
// would otherwise be the cheapest way to perform a write nothing could name —
// and a node id from the variables. No argument value and no free text is
// interpreted (internal/ghwrite gqldoc.go states what the reader refuses to look
// at), and a document that does not parse is recorded as unreadable rather than
// guessed at.
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
// serves TLS with a per-run self-signed certificate (GenerateCert), published to
// the jail through a bind-mounted trust file SSL_CERT_FILE points at
// (TrustBundlePEM — the system roots plus this run's leaf, since that env var is
// process-global). There is no TLS interception of GitHub anywhere — the
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
// # Observation
//
// Exec-verb self-reporting doesn't exist on this channel, so the injector emits
// an observation for the artifact-bearing mutations it can recognize, via a
// caller-supplied callback the sidecar turns into an orchestrator relay. Two
// transports carry them, because gh's porcelain and `gh api` do not agree:
//
//   - GraphQL. Every mutation in gh's porcelain goes through POST /api/graphql
//     (verified against the pinned gh release). A successful create is
//     recognized by the exact top-level response key data.createPullRequest —
//     never by walking the payload for any pullRequest object, since a mere
//     `gh pr view` nests one under data.repository.pullRequest and attributing
//     a PR the run only read as one it produced is worse than missing a
//     mutation. gh's field selection is just `pullRequest { id url }`, so the
//     observation carries the node id and the html url (owner/repo/number
//     parsed out of it) and nothing else; title and refs are left empty for
//     the reconciler to fill, as it already does for its backstop rows.
//   - REST, for `gh api` calls the agent writes by hand:
//     POST .../pulls (PR created) and POST .../pulls/{n}/reviews (review
//     posted), which is also the only way inline review comments are posted.
//
// The review half of that pair no longer fires, because a review submitted
// through this channel is now refused before it is forwarded. It is kept rather
// than deleted: the observation is what the artifact row would be built from if
// the policy ever admits the shape, and nothing about it is specific to the
// policy that currently does not.
//
// Decoding a response narrows nothing the agent may do and never alters a
// request. `gh pr review` posts addPullRequestReview, whose response carries
// only clientMutationId — the PR it targets is named in the *request*, so
// observing it is out of scope by the invariant above; a reconciler backstop
// owns that case.
//
// # Write audit
//
// Observation covers only the two artifact-bearing creates, so an edit, a
// merge, a close, and every refused write used to leave no trace at all. The
// second callback (ObserveWrite) closes that gap on both transports, and every
// write leaves exactly one report whichever one it took.
//
// REST is read from the response's own request record — method, path, status —
// so those requests stay unread. The shapes the shared classifier
// (internal/ghwrite) marks as creates — a posted comment, a review-thread reply
// — additionally have their RESPONSE body parsed for the new object's id and
// link, through the same cap-and-stitch machinery artifact observation uses, so
// the audit row reaches parity with the equivalent exec verb's. Every other
// shape is fully described by its path and costs no buffering at all.
//
// GraphQL needs both ends of the hop. The act is named only in the request, so
// the capture in Handler reads it there; the outcome is only in the response,
// and not in its status line — the endpoint refuses a mutation with a 200
// carrying an errors array — so the audit reads that too and files a refused
// write as an attempt. A request whose document names no mutation is a read and
// reports nothing at all, which is nearly all of this endpoint's traffic.
//
// Neither callback is a policy: reporting a write neither permits nor prevents
// it. Both fire after the request has already transited, so a refused request
// reaches neither of them — its one row is the refusal, written by the gate.
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
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// restPrefix is the GHE REST path prefix gh emits; graphqlPath is the GraphQL
// path. These are the only two surfaces the injector routes (spike finding 4).
const (
	restPrefix  = "/api/v3"
	graphqlPath = "/api/graphql"
)

// maxObserveBody caps how much of an observed mutation's response body the
// injector will BUFFER to parse the created object's coordinates. PR-create and
// review-post responses are a few KB; the cap only guards against a pathological
// upstream. A body past the cap is never truncated — observation is skipped for
// that response (the reconciler backstop covers it) and the body streams on: a
// declared Content-Length over the cap is skipped without reading anything at
// all, and one discovered mid-read has its consumed prefix stitched back in
// front of the untouched remainder. The agent's response is always delivered
// whole; observation is the only thing that degrades.
const maxObserveBody = 1 << 20

// injectorLog carries what this package says out loud, which is only its
// refusals. Everything it merely observes travels as a relayed audit row and is
// silent here; a refusal is a decision this process made, and the line is
// written locally so it owes nothing to the relay hop that records it.
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
// picked up; an error (or empty token) surfaces to the agent as a 502, never a
// silently-unauthenticated forward.
type TokenSource func(ctx context.Context) (string, error)

// ObservedMutation is one artifact-bearing mutation the injector saw complete
// successfully. The sidecar relays it to the orchestrator, which owns the DB and
// builds the artifact row (the injector holds no DB handle and no domain types).
// Kind is "pull_request" or "review".
type ObservedMutation struct {
	Kind   string
	Owner  string
	Repo   string
	Number int // PR number (response for a PR; request path for a REST review)

	// PR-create fields. A GraphQL create populates only NodeID and URL — gh
	// selects nothing else — so Head/Base/Title/Body/Draft carry values only on
	// the REST path.
	NodeID string
	Head   string
	Base   string
	URL    string
	Title  string
	Body   string
	Draft  bool

	// Review-post fields.
	ReviewID    int
	ReviewState string // GitHub's review state, e.g. APPROVED / COMMENTED / CHANGES_REQUESTED
}

// ObservedWrite is one mutating REST request the injector forwarded, with the
// outcome the upstream returned. It is the classifier package's Observation
// under this package's name: the sidecar relays it and the orchestrator turns
// it into an audit row, and one type across that hop is what keeps the two ends
// from drifting.
type ObservedWrite = ghwrite.Observation

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

	// Observe, when non-nil, is called for each PR-create / review-post the
	// injector sees complete. Best-effort and out of the response's critical
	// path shape (called after the body is buffered, before it streams on).
	Observe func(ctx context.Context, m ObservedMutation)

	// ObserveWrite, when non-nil, is called for every mutating REST request the
	// injector forwarded, whatever the outcome — the audit-parity callback, as
	// opposed to Observe's artifact-parity one. Both fire for a successful REST
	// PR create, and neither is redundant with the other: an artifact says the
	// pull request exists, an action says this run opened it, and a reader
	// asking either question is not served by the other's answer.
	// Called inline on the response path, so the callback must not block.
	ObserveWrite func(ctx context.Context, w ObservedWrite)

	// AuthorizeWrite decides the gated shapes and records their refusals. See
	// the type's own doc: nil refuses every gated shape without recording one,
	// which is the fail-closed reading of "no decision source is wired" and
	// never the production shape — both modes supply it.
	AuthorizeWrite AuthorizeWrite

	// ConversationID names the run this injector serves, for log attribution only. It is
	// never sent upstream and never reaches an audit row — those are attributed
	// by the orchestrator from its own run identity, never from anything the
	// sidecar names. Empty is fine; it costs a log line its run.
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

	s := &Server{cfg: cfg, restURL: rest, graphqlURL: gql}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
	}
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
		facts := s.captureGraphQLWrite(r)
		// Before the credential is resolved, so a refused request never reaches
		// the credential pipeline at all.
		if s.refused(w, r, facts) {
			return
		}
		tok, err := s.cfg.TokenSource(r.Context())
		if err != nil || tok == "" {
			// 502: the proxy is alive but the credential pipeline is broken.
			// Never leak the source error (may carry credential-setup detail).
			http.Error(w, "ghinjector: failed to resolve upstream credential", http.StatusBadGateway)
			return
		}
		ctx := context.WithValue(r.Context(), authCtxKey{}, tok)
		if facts != nil {
			ctx = context.WithValue(ctx, graphQLFactsKey{}, facts)
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

// captureGraphQLWrite buffers a GraphQL request body, re-presents it unchanged,
// and reads the act out of it. nil for everything that is not a GraphQL
// mutation — a different endpoint, a read — and that is the overwhelming
// majority of what passes through here.
//
// One return value rather than a (facts, ok) pair, because both the audit and
// the refusal policy branch on "is there anything to say about this request",
// and a boolean that only ever means "the pointer is non-nil" is one place for
// the two to disagree.
//
// It runs whether or not the write audit is wired, because the refusal policy
// reads the same facts and a run with no audit callback is still a run that may
// not merge.
//
// The buffering is unconditional for POSTs to the GraphQL endpoint, because
// whether a body is a read cannot be known before reading it. What stays cheap
// is the parse: ParseGraphQLRequest decodes the envelope and stops at a
// document with no mutation keyword in it, so ordinary query traffic costs one
// copy and one JSON decode and never reaches the document reader.
func (s *Server) captureGraphQLWrite(r *http.Request) *ghwrite.GraphQLFacts {
	if r.Method != http.MethodPost || !isGraphQLPath(r.URL.Path) {
		return nil
	}

	body, unread := bufferRequest(r)
	if unread != "" {
		// No document to read, so nothing can say what this request performs.
		// It is refused on exactly that ground (see ghwrite.gateGraphQL) and the
		// reason travels verbatim, because the two causes are not alike — a cap
		// this side chose, against a caller that stopped sending — and a
		// refusal in this band that is not vanishingly rare means something
		// operational rather than someone probing.
		return &ghwrite.GraphQLFacts{Unreadable: unread}
	}

	facts, isWrite := ghwrite.ParseGraphQLRequest(body)
	if !isWrite {
		return nil
	}
	return &facts
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
// indistinguishable from an ordinary 404).
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
		w.ExternalID, w.URL = parseCreatedObject(body)
	}
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
		w.Errored, w.URL = ghwrite.ReadGraphQLResponse(body, facts.Mutation())
	} else {
		w.ResponseUnread = true
	}
	s.cfg.ObserveWrite(context.Background(), w)
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

// observationCandidate reports whether this response is one of the
// artifact-bearing mutations, with the coordinates the path supplies. GraphQL
// is a candidate on shape alone — the operation is named in the request, which
// is off limits, so its body has to say what it was.
func (s *Server) observationCandidate(resp *http.Response, graphql bool) (kind, owner, repo string, number int, ok bool) {
	if s.cfg.Observe == nil || resp.Request.Method != http.MethodPost ||
		resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", 0, false
	}
	if graphql {
		return "", "", "", 0, true
	}
	return classifyMutationPath(resp.Request.URL.Path)
}

// modifyResponse bumps the request counter, reports every mutating REST request
// to the write audit, and, for the shapes whose response names an object,
// buffers+parses the body before streaming it on unchanged. Never returns an
// error (that would 502 the agent on a successful upstream write) — observation
// failures are silently dropped and left to the reconciler backstop.
//
// The body is buffered at most once and shared by both consumers: they read
// different objects out of it, but a response has one body and reading it twice
// is not a thing that can be made to work.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	if resp.Request == nil {
		return nil
	}
	graphql := resp.Request.URL.Path == s.graphqlURL.Path

	kind, owner, repo, number, observing := s.observationCandidate(resp, graphql)
	auditingCreate := s.auditWantsBody(resp, graphql)
	// Non-nil only for a GraphQL request the capture in Handler read as a write.
	facts, _ := resp.Request.Context().Value(graphQLFactsKey{}).(*ghwrite.GraphQLFacts)

	var buf []byte
	if observing || auditingCreate || facts != nil {
		buf, _ = bufferForObservation(resp)
	}

	if facts != nil {
		// A GraphQL write's outcome is not in its status line, so this arm needs
		// the body whatever the act was — an errors array is how the endpoint
		// refuses, and a row that read only the 200 would report a refused merge
		// as a merge.
		s.auditGraphQLWrite(resp, facts, buf)
	} else {
		// The REST audit runs whatever the buffering produced — it covers the
		// outcomes observation drops, a refused write and every mutating method
		// that makes no artifact — but it is handed a body only when the
		// classifier said this shape's response names a created object. The two
		// conditions overlap for a REST create and diverge elsewhere, and
		// handing over a body buffered for some other reason would hang whatever
		// it happens to contain on a row with no use for it.
		createdBody := buf
		if !auditingCreate {
			createdBody = nil
		}
		s.auditWrite(resp, graphql, createdBody)
	}

	if !observing || buf == nil {
		return nil
	}
	var (
		m      ObservedMutation
		parsed bool
	)
	if graphql {
		m, parsed = parseGraphQLObservation(buf)
	} else {
		m, parsed = parseObservation(kind, owner, repo, number, buf)
	}
	if parsed {
		s.cfg.Observe(context.Background(), m)
	}
	return nil
}

// parseCreatedObject reads the id and html url of the object a create response
// names. Both empty when the body doesn't parse or carries no id — a 2xx
// without a recognizable object still happened, so the caller records the act
// and lets the deep link degrade.
func parseCreatedObject(body []byte) (externalID, url string) {
	var o struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &o); err != nil || o.ID == 0 {
		return "", ""
	}
	return strconv.FormatInt(o.ID, 10), o.HTMLURL
}

// bufferForObservation reads the whole response body into memory and re-presents
// it to the caller, returning the buffered bytes. ok is false when the body is
// larger than the injector will buffer or the read broke mid-stream; in both
// cases the response is restored to a readable state (consumed prefix stitched
// back in front of the remainder, original body kept as the Closer) and
// observation is skipped for it.
//
// Past the cap, that restoration is exact and the agent's response is delivered
// whole, with observation the only thing degraded. On a broken read the same
// stitch happens and this proxy still drops nothing of its own, but the
// remainder is a stream that already failed — an upstream body that cannot be
// read is not one anything in the middle can deliver whole.
func bufferForObservation(resp *http.Response) ([]byte, bool) {
	// A declared length past the cap settles the outcome before any I/O: the
	// body would be skipped anyway, so read none of it and leave it streaming
	// straight through to the agent. ContentLength is -1 when unknown (chunked),
	// which falls through to the read below.
	if resp.ContentLength > maxObserveBody {
		return nil, false
	}

	// Read one byte past the cap so "body exceeded the cap" is distinguishable
	// from "body is exactly the cap" (ReadAll on a LimitReader reports no error
	// when it stops at the limit).
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxObserveBody+1))
	if err != nil || len(buf) > maxObserveBody {
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

// classifyMutationPath recognizes the two artifact-bearing POST paths on the
// upstream request URL (post-rewrite, so REST base already prepended): a PR
// create (.../repos/{o}/{r}/pulls) or a review post
// (.../repos/{o}/{r}/pulls/{n}/reviews). Returns the kind + coordinates parsed
// from the path; number is 0 for a PR create (assigned in the response).
func classifyMutationPath(path string) (kind, owner, repo string, number int, ok bool) {
	i := strings.Index(path, "/repos/")
	if i < 0 {
		return "", "", "", 0, false
	}
	segs := strings.Split(strings.Trim(path[i+len("/repos/"):], "/"), "/")
	// pulls:            owner repo pulls
	// reviews:          owner repo pulls {n} reviews
	if len(segs) == 3 && segs[2] == "pulls" && segs[0] != "" && segs[1] != "" {
		return "pull_request", segs[0], segs[1], 0, true
	}
	if len(segs) == 5 && segs[2] == "pulls" && segs[4] == "reviews" {
		n, err := strconv.Atoi(segs[3])
		if err != nil || segs[0] == "" || segs[1] == "" {
			return "", "", "", 0, false
		}
		return "review", segs[0], segs[1], n, true
	}
	return "", "", "", 0, false
}

// createPullRequestField is the exact top-level response key GitHub returns for
// the create mutation, and the sole trigger for a GraphQL observation.
const createPullRequestField = "createPullRequest"

// parseGraphQLObservation recognizes a PR creation in a GraphQL response body.
// It keys on the exact top-level data.createPullRequest field rather than
// searching the payload for a pullRequest object: a query result nests one under
// data.repository.pullRequest, and recording a PR the run merely read as one it
// created is a worse failure than missing an aliased hand-written mutation.
//
// gh's selection set is `createPullRequest(input:$input){ pullRequest{ id url }}`
// and nothing more, so owner/repo/number come from parsing the html url; the
// fields the REST shape supplies (title, refs, draft) have no GraphQL source
// here and stay empty for the reconciler to fill.
func parseGraphQLObservation(body []byte) (ObservedMutation, bool) {
	// The field name is a prerequisite for the key being present, so this
	// prescreen never costs a real observation — it just keeps the ordinary
	// query traffic (which is most of what gh sends) out of the decoder.
	if !bytes.Contains(body, []byte(createPullRequestField)) {
		return ObservedMutation{}, false
	}
	var payload struct {
		Data struct {
			CreatePullRequest *struct {
				PullRequest struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"pullRequest"`
			} `json:"createPullRequest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data.CreatePullRequest == nil {
		return ObservedMutation{}, false
	}
	pr := payload.Data.CreatePullRequest.PullRequest
	owner, repo, number, ok := ghwrite.ParsePullRequestURL(pr.URL)
	if !ok {
		return ObservedMutation{}, false
	}
	return ObservedMutation{
		Kind:   "pull_request",
		Owner:  owner,
		Repo:   repo,
		Number: number,
		NodeID: pr.ID,
		URL:    pr.URL,
	}, true
}

// parseObservation extracts the created object's coordinates from a mutation's
// JSON response body.
func parseObservation(kind, owner, repo string, number int, body []byte) (ObservedMutation, bool) {
	switch kind {
	case "pull_request":
		var pr struct {
			Number  int    `json:"number"`
			NodeID  string `json:"node_id"`
			HTMLURL string `json:"html_url"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			Draft   bool   `json:"draft"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		if err := json.Unmarshal(body, &pr); err != nil || pr.Number == 0 {
			return ObservedMutation{}, false
		}
		return ObservedMutation{
			Kind:   "pull_request",
			Owner:  owner,
			Repo:   repo,
			Number: pr.Number,
			NodeID: pr.NodeID,
			Head:   pr.Head.Ref,
			Base:   pr.Base.Ref,
			URL:    pr.HTMLURL,
			Title:  pr.Title,
			Body:   pr.Body,
			Draft:  pr.Draft,
		}, true
	case "review":
		var rv struct {
			ID      int    `json:"id"`
			HTMLURL string `json:"html_url"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(body, &rv); err != nil || rv.ID == 0 {
			return ObservedMutation{}, false
		}
		return ObservedMutation{
			Kind:        "review",
			Owner:       owner,
			Repo:        repo,
			Number:      number,
			ReviewID:    rv.ID,
			ReviewState: rv.State,
			URL:         rv.HTMLURL,
		}, true
	}
	return ObservedMutation{}, false
}

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
		if err := assertLoopback(addr); err != nil {
			return "", err
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

// assertLoopback mirrors apiproxy.assertLoopback: nil iff addr binds loopback.
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ghinjector: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("ghinjector: bind address %q binds all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("ghinjector: bind address %q is not loopback — set AllowNonLoopback=true", addr)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("ghinjector: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("ghinjector: bind host %q resolves to non-loopback %s — set AllowNonLoopback=true", host, a)
		}
	}
	return nil
}
