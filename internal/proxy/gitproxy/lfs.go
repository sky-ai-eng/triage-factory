package gitproxy

// Git LFS through the proxy.
//
// git-lfs speaks a JSON API beside git's own protocol, at <remote>.git/info/lfs,
// so it reaches this proxy through the same insteadOf rewrite git does. Its
// batch call asks the server where to fetch or send a set of objects, and the
// answer names, per object, a storage URL and the headers to present there:
// typically a signed URL on another host, sometimes with a credential of its
// own in the headers. Passing that answer through would hand a storage
// credential to the agent and send it to a host a jailed run cannot reach.
//
// So the proxy keeps each storage action host-side and answers with a URL of
// its own in its place. The agent's transfer comes back here, passes the same
// live gate a fetch or a push does, and the proxy performs it against the URL
// and headers the server named. The installation token goes only to the batch
// and lock-verification calls on the git host, never to a storage host; the
// only value the agent ever holds is the per-run placeholder it already
// presents for git.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"time"
)

const (
	// lfsMediaType is the content type of every Git LFS API request and
	// answer. git-lfs reads an error's `message` from a body of this type,
	// so the proxy's own refusals use it to reach the agent intact.
	lfsMediaType = "application/vnd.git-lfs+json"

	// A batch request names at most a few hundred objects (git-lfs sends 100
	// per call), and an answer a few hundred bytes per object, so both caps
	// are far above any real call and exist so a body is never read without
	// bound.
	lfsBatchRequestLimit  = 1 << 20
	lfsBatchResponseLimit = 16 << 20

	// lfsTransferTTL caps how long a transfer URL stays valid, whatever the
	// server allowed for the storage URL behind it. The answer carries the
	// capped expiry, so git-lfs asks for a fresh batch rather than using a
	// URL the proxy has forgotten.
	lfsTransferTTL = time.Hour

	// lfsMaxTransfers bounds the transfer URLs one proxy holds at once. A
	// transfer that succeeds is forgotten, so in practice this holds what one
	// or two batches have handed out and not yet used. In local mode the
	// proxy runs in the TF server itself, which is why it is bounded at all:
	// an agent asking for upload URLs it never uses would otherwise grow the
	// server's memory for as long as those URLs live.
	lfsMaxTransfers = 10000
)

// lfsActionMethods is every transfer action the basic adapter defines, with
// the HTTP method git-lfs performs it with. An answer naming any other action
// is refused, because its URL could be neither proxied nor handed on.
var lfsActionMethods = map[string]string{
	"download": http.MethodGet,
	"upload":   http.MethodPut,
	"verify":   http.MethodPost,
}

// errLFSTransfersFull is answered 503: the proxy is holding as many transfer
// URLs as it will, and a retry once some are used or expire can succeed.
var errLFSTransfersFull = errors.New("gitproxy: too many outstanding Git LFS transfers; retry once the current ones finish")

// lfsTransfer is one storage action the proxy performs on the agent's behalf.
// href and header are the server's, and never leave this process.
type lfsTransfer struct {
	repoKey string // lower("owner/repo") of the batch that named it
	action  string // a key of lfsActionMethods
	href    *url.URL
	header  map[string]string
	expires time.Time
}

// lfsAction is one entry of a batch answer's `actions` object.
type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int64             `json:"expires_in,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
}

// lfsBatchCall carries what the batch answer's rewrite needs from the inbound
// request to ModifyResponse.
type lfsBatchCall struct {
	repoKey string
	// base is the transfer URL prefix, ending in "/transfers/", as the agent
	// reached this proxy: its scheme and host, and the repository path it
	// used. A transfer URL built from it matches the same http.<url>.* git
	// config the batch call did, so git-lfs presents the same placeholder.
	base string
}

type lfsBatchCallKey struct{}

// lfsTransferCall carries a transfer from the Handler to the Rewrite and
// ModifyResponse hooks, and the upstream's status back.
type lfsTransferCall struct {
	t      *lfsTransfer
	status int
}

type lfsTransferCallKey struct{}

func isLFSOp(op string) bool {
	return op == gitOpLFSBatch || op == gitOpLFSLocksVerify || op == gitOpLFSTransfer
}

// validLFSTransferID accepts exactly the ids storeLFSTransfer mints.
func validLFSTransferID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func repoKey(owner, repo string) string { return strings.ToLower(owner + "/" + repo) }

// mayPush reports whether a decision lets the run write to the repository. An
// LFS upload changes no ref, so the ref gate has nothing to judge; what it
// writes is storage the repository's pushes will refer to, so it is allowed
// exactly where some push is. With no gate wired everything is allowed, as for
// git.
func (s *Server) mayPush(d Decision) bool {
	return s.cfg.Authorize == nil || len(d.AllowedRefs) > 0
}

// authorizeLFS runs the live gate for one Git LFS operation, plus the push
// right an upload needs. ok=false means the refusal has been recorded and
// answered.
func (s *Server) authorizeLFS(w http.ResponseWriter, r *http.Request, owner, repo, op string) bool {
	d, msg, ok := s.authorize(r.Context(), owner, repo, op)
	if !ok {
		writeLFSError(w, http.StatusForbidden, msg)
		return false
	}
	if op == gitOpLFSUpload && !s.mayPush(d) {
		s.recordDenial(DeniedGitOp{Owner: owner, Repo: repo, Op: op, Reason: "push-not-authorized"})
		writeLFSError(w, http.StatusForbidden, fmt.Sprintf("gitproxy: this run may not push to %s/%s, so it may not upload Git LFS objects there", owner, repo))
		return false
	}
	return true
}

// writeLFSError answers in the Git LFS error shape, which git-lfs prints as
// the failure's explanation; a plain-text body would be replaced by its
// generic message for the status.
func writeLFSError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", lfsMediaType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
	}{msg})
}

func (s *Server) serveLFS(w http.ResponseWriter, r *http.Request, owner, repo, op string) {
	switch op {
	case gitOpLFSBatch:
		s.serveLFSBatch(w, r, owner, repo)
	case gitOpLFSLocksVerify:
		// The lock check git-lfs makes before every push. It only reads, so
		// it needs what a fetch needs; the push it precedes is judged by the
		// ref gate on its own.
		if !s.authorizeLFS(w, r, owner, repo, op) {
			return
		}
		r, ok := s.withToken(w, r, owner, repo)
		if !ok {
			return
		}
		s.proxy.ServeHTTP(w, r)
	case gitOpLFSTransfer:
		s.serveLFSTransfer(w, r, owner, repo)
	}
}

// serveLFSBatch gates a batch call on the operation its body names, restricts
// it to the basic transfer adapter (the one whose actions this proxy knows how
// to perform), and forwards it with the credential. The answer is rewritten on
// the way back (rewriteLFSBatchResponse).
func (s *Server) serveLFSBatch(w http.ResponseWriter, r *http.Request, owner, repo string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, lfsBatchRequestLimit+1))
	if err != nil {
		writeLFSError(w, http.StatusBadRequest, "gitproxy: could not read the Git LFS batch request")
		return
	}
	if len(body) > lfsBatchRequestLimit {
		writeLFSError(w, http.StatusRequestEntityTooLarge, "gitproxy: Git LFS batch request is too large")
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		writeLFSError(w, http.StatusBadRequest, "gitproxy: Git LFS batch request is not a JSON object")
		return
	}
	var operation string
	_ = json.Unmarshal(req["operation"], &operation)
	var op string
	switch operation {
	case "download":
		op = gitOpLFSDownload
	case "upload":
		op = gitOpLFSUpload
	default:
		writeLFSError(w, http.StatusUnprocessableEntity, fmt.Sprintf("gitproxy: Git LFS batch operation %q is not download or upload", operation))
		return
	}

	if !s.authorizeLFS(w, r, owner, repo, op) {
		return
	}

	req["transfers"] = json.RawMessage(`["basic"]`)
	out, err := json.Marshal(req)
	if err != nil {
		writeLFSError(w, http.StatusInternalServerError, "gitproxy: could not re-encode the Git LFS batch request")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(out)), nil }

	r, ok := s.withToken(w, r, owner, repo)
	if !ok {
		return
	}
	call := lfsBatchCall{repoKey: repoKey(owner, repo), base: lfsTransferBase(r)}
	r = r.WithContext(context.WithValue(r.Context(), lfsBatchCallKey{}, call))
	s.lfsBatch.ServeHTTP(w, r)
}

// lfsTransferBase is the transfer URL prefix for a request the agent sent to
// this proxy: its own scheme, host and repository path, so the URL resolves
// back to the same listener and matches the same http.<url>.* config.
func lfsTransferBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// The last occurrence, because an owner or repository may itself be
	// named info or lfs; the LFS endpoint's own path never repeats it.
	p := r.URL.Path
	return scheme + "://" + r.Host + p[:strings.LastIndex(p, lfsPathSegment)] + lfsPathSegment + "transfers/"
}

// rewriteLFSBatchRequest is the git host's own rewrite with the answer asked
// for uncompressed, because rewriteLFSBatchResponse has to read it.
func (s *Server) rewriteLFSBatchRequest(pr *httputil.ProxyRequest) {
	s.rewrite(pr)
	pr.Out.Header.Set("Accept-Encoding", "identity")
}

// rewriteLFSBatchResponse replaces every storage action in a successful batch
// answer with a transfer URL of this proxy's. Any other answer — an error the
// server explains, a per-object error — names no storage URL and passes
// through unchanged.
func (s *Server) rewriteLFSBatchResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil
	}
	call, _ := resp.Request.Context().Value(lfsBatchCallKey{}).(lfsBatchCall)
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return fmt.Errorf("gitproxy: Git LFS batch answer is %s-encoded though identity was asked for", enc)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, lfsBatchResponseLimit+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("gitproxy: read the Git LFS batch answer: %w", err)
	}
	if len(raw) > lfsBatchResponseLimit {
		return errors.New("gitproxy: Git LFS batch answer is too large")
	}
	out, err := s.rewriteLFSBatch(raw, call)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// rewriteLFSBatch is the body of rewriteLFSBatchResponse. Members it has no
// reason to touch are carried through as the server sent them. It either
// rewrites every action or fails, forgetting what it minted: an answer
// half-rewritten would hand the agent the storage URLs of the rest.
func (s *Server) rewriteLFSBatch(raw []byte, call lfsBatchCall) (_ []byte, err error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("gitproxy: Git LFS batch answer is not a JSON object: %w", err)
	}
	if t, ok := doc["transfer"]; ok {
		var name string
		if json.Unmarshal(t, &name) != nil || (name != "" && name != "basic") {
			return nil, fmt.Errorf("gitproxy: Git LFS server chose transfer adapter %s, not basic", t)
		}
	}
	var objects []map[string]json.RawMessage
	if o, ok := doc["objects"]; ok {
		if err := json.Unmarshal(o, &objects); err != nil {
			return nil, fmt.Errorf("gitproxy: Git LFS batch answer's objects: %w", err)
		}
	}

	now := s.timeNow()
	var minted []string
	defer func() {
		if err != nil {
			s.forgetLFSTransfers(minted)
		}
	}()
	for _, obj := range objects {
		rawActions, ok := obj["actions"]
		if !ok {
			continue
		}
		var actions map[string]lfsAction
		if err := json.Unmarshal(rawActions, &actions); err != nil {
			return nil, fmt.Errorf("gitproxy: Git LFS batch answer's actions: %w", err)
		}
		rewritten := make(map[string]lfsAction, len(actions))
		for name, a := range actions {
			if _, known := lfsActionMethods[name]; !known {
				return nil, fmt.Errorf("gitproxy: Git LFS server named transfer action %q, which the basic adapter does not define", name)
			}
			href, err := s.lfsHref(a.Href)
			if err != nil {
				return nil, err
			}
			t := &lfsTransfer{
				repoKey: call.repoKey,
				action:  name,
				href:    href,
				header:  a.Header,
				expires: lfsExpiry(a, now),
			}
			id, err := s.storeLFSTransfer(t, now)
			if err != nil {
				return nil, err
			}
			minted = append(minted, id)
			rewritten[name] = lfsAction{Href: call.base + id, ExpiresAt: t.expires.UTC().Format(time.RFC3339)}
		}
		if obj["actions"], err = json.Marshal(rewritten); err != nil {
			return nil, err
		}
		// The transfer URL is this proxy's, reached with the placeholder git
		// config already supplies, so git-lfs has no credential to look up.
		obj["authenticated"] = json.RawMessage("true")
	}
	if doc["objects"], err = json.Marshal(objects); err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

// lfsHref accepts a storage URL only over https: the proxy is about to send
// the server's headers, which may be a credential, wherever it names. Plain
// http is allowed only for a loopback storage URL behind a loopback http
// upstream, which is a test.
func (s *Server) lfsHref(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("gitproxy: Git LFS server named a transfer URL that does not parse")
	}
	if u.Scheme == "https" {
		return u, nil
	}
	if u.Scheme == "http" && s.upstreamURL.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return u, nil
		}
	}
	// Scheme and host only: the rest of a storage URL is often its signature.
	return nil, fmt.Errorf("gitproxy: Git LFS server named a transfer URL on %s://%s; only https is allowed", u.Scheme, u.Host)
}

// lfsExpiry is when a transfer URL stops working: the earlier of the storage
// URL's own expiry and lfsTransferTTL, to the second, because the answer
// states it in RFC 3339 seconds and the two must agree.
func lfsExpiry(a lfsAction, now time.Time) time.Time {
	exp := now.Add(lfsTransferTTL)
	if a.ExpiresIn > 0 {
		if t := now.Add(time.Duration(a.ExpiresIn) * time.Second); t.Before(exp) {
			exp = t
		}
	}
	if a.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, a.ExpiresAt); err == nil && t.Before(exp) {
			exp = t
		}
	}
	return exp.Truncate(time.Second)
}

func (s *Server) storeLFSTransfer(t *lfsTransfer, now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gitproxy: mint a Git LFS transfer id: %w", err)
	}
	id := hex.EncodeToString(b[:])
	s.lfsMu.Lock()
	defer s.lfsMu.Unlock()
	if s.lfsTransfers == nil {
		s.lfsTransfers = make(map[string]*lfsTransfer)
	}
	if len(s.lfsTransfers) >= lfsMaxTransfers {
		for k, v := range s.lfsTransfers {
			if !now.Before(v.expires) {
				delete(s.lfsTransfers, k)
			}
		}
		if len(s.lfsTransfers) >= lfsMaxTransfers {
			return "", errLFSTransfersFull
		}
	}
	s.lfsTransfers[id] = t
	return id, nil
}

// lfsTransferFor returns the live transfer behind id, or nil.
func (s *Server) lfsTransferFor(id string, now time.Time) *lfsTransfer {
	s.lfsMu.Lock()
	defer s.lfsMu.Unlock()
	t := s.lfsTransfers[id]
	if t != nil && !now.Before(t.expires) {
		delete(s.lfsTransfers, id)
		return nil
	}
	return t
}

func (s *Server) forgetLFSTransfers(ids []string) {
	s.lfsMu.Lock()
	defer s.lfsMu.Unlock()
	for _, id := range ids {
		delete(s.lfsTransfers, id)
	}
}

// serveLFSTransfer performs one transfer a batch answer handed out, after the
// same gate the batch passed: untracking the repository, or losing the right
// to push to it, stops the next transfer as it would the next fetch or push.
func (s *Server) serveLFSTransfer(w http.ResponseWriter, r *http.Request, owner, repo string) {
	id := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	t := s.lfsTransferFor(id, s.timeNow())
	if t == nil || t.repoKey != repoKey(owner, repo) {
		writeLFSError(w, http.StatusNotFound, "gitproxy: unknown or expired Git LFS transfer; request a new batch")
		return
	}
	if method := lfsActionMethods[t.action]; r.Method != method {
		writeLFSError(w, http.StatusMethodNotAllowed, fmt.Sprintf("gitproxy: this Git LFS transfer is a %s, performed with %s", t.action, method))
		return
	}
	op := gitOpLFSDownload
	if t.action != "download" {
		op = gitOpLFSUpload
	}
	if !s.authorizeLFS(w, r, owner, repo, op) {
		return
	}

	call := &lfsTransferCall{t: t}
	s.lfsTransfer.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), lfsTransferCallKey{}, call)))
	// Reached only when the whole answer was relayed; a body cut off
	// mid-stream aborts the handler instead, leaving the transfer for
	// git-lfs's retry.
	if call.status >= 200 && call.status <= 299 {
		s.forgetLFSTransfers([]string{id})
	}
}

// rewriteLFSTransfer sends the transfer to the storage URL with the server's
// headers in place of everything that identifies the agent's side of the hop.
func (s *Server) rewriteLFSTransfer(pr *httputil.ProxyRequest) {
	call := pr.In.Context().Value(lfsTransferCallKey{}).(*lfsTransferCall)
	u := *call.t.href
	pr.Out.URL = &u
	pr.Out.Host = ""
	for _, h := range []string{
		"Authorization", "Cookie",
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded",
		"Proxy-Authorization", "Proxy-Connection",
	} {
		pr.Out.Header.Del(h)
	}
	for k, v := range call.t.header {
		pr.Out.Header.Set(k, v)
	}
}

func (s *Server) observeLFSTransfer(resp *http.Response) error {
	s.requestCount.Add(1)
	if call, ok := resp.Request.Context().Value(lfsTransferCallKey{}).(*lfsTransferCall); ok {
		call.status = resp.StatusCode
	}
	return nil
}

// lfsProxyError answers a failed LFS hop in the Git LFS error shape.
func (s *Server) lfsProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errLFSTransfersFull) {
		writeLFSError(w, http.StatusServiceUnavailable, errLFSTransfersFull.Error())
		return
	}
	gitproxyLog.Warn("Git LFS request failed upstream", "conversation", s.cfg.ConversationID, "error", err)
	writeLFSError(w, http.StatusBadGateway, "gitproxy: the Git LFS request failed upstream")
}
