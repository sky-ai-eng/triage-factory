package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Limits and timeouts for one intercepted session.
const (
	// maxRequestHead bounds a request's line and headers. A registry
	// request's head is well under a kilobyte; this leaves room for long
	// query strings without letting one request hold a large buffer.
	maxRequestHead = 64 << 10

	// jailHandshakeTimeout bounds the jail's TLS handshake with the leaf.
	jailHandshakeTimeout = 10 * time.Second

	// jailIdleTimeout is how long a kept-alive session waits for the jail's
	// next request before closing.
	jailIdleTimeout = 90 * time.Second

	// upstreamResponseHeaderTimeout bounds the wait for a registry's
	// response headers. The body that follows streams with no bound: a large
	// tarball can take minutes.
	upstreamResponseHeaderTimeout = 60 * time.Second

	// maxQuotedValue caps how much of a client-supplied value (a path, a
	// Host, a TLS server name) a refusal and its denial record repeat.
	maxQuotedValue = 200

	// maxSessions bounds the sessions one run's proxy holds at once. Every
	// session costs a resolve, a dial and two TLS handshakes before it
	// carries a byte, and the dials leave from the executor's address, so an
	// agent opening sessions without limit would spend its own sidecar's
	// file descriptors and the address every other run on the host shares. A
	// package manager keeps far fewer: npm and pnpm cap their sockets at 16
	// or less.
	maxSessions = 64
)

// methodOverrideHeaders are headers some servers read as a request's real
// method, matched ignoring case and with underscores read as dashes. A
// request carrying any of them, or a `_method` query parameter, is refused
// whatever its value: read-only means nothing if a GET can be turned into a
// write.
var methodOverrideHeaders = []string{"x-http-method-override", "x-http-method", "x-method-override"}

// hopByHopHeaders describe one connection rather than the request or
// response, so they are not carried across the interceptor (RFC 9110
// §7.6.1).
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// errHeadTooLarge is a request head that does not fit in maxRequestHead.
var errHeadTooLarge = errors.New("request head too large")

// refusal is one request the interceptor will not relay: the status the jail
// is answered with and the reason it reads.
type refusal struct {
	status int
	reason string
}

// upstreamTLSError is a failed TLS handshake with a registry, verification
// failures included. It is a refusal rather than a network fault: the jail is
// told, and a denial is recorded, but nothing is tunneled instead.
type upstreamTLSError struct {
	host string
	err  error
}

func (e upstreamTLSError) Error() string {
	return fmt.Sprintf("TLS with %s failed: %v", e.host, e.err)
}

func (e upstreamTLSError) Unwrap() error { return e.err }

// dialUpstream opens a verified TLS connection to host: the vetted dial (so
// the internal denylist applies to every connection, the first and every
// redial), then a handshake under upstreamTLSConfig. host is the CONNECT host;
// every upstream connection of the session is to that name and no other.
func (s *Server) dialUpstream(ctx context.Context, host string) (net.Conn, error) {
	raw, err := s.dialVetted(ctx, host)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, upstreamTLSConfig(host))
	hctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		return nil, upstreamTLSError{host: host, err: err}
	}
	return conn, nil
}

// upstreamTransport relays one session's requests to host. Its first
// connection is the one handleRequest already verified before answering the
// CONNECT; any later one is dialed the same way. It speaks HTTP/1.1 only,
// never decompresses (a response reaches the jail as the registry encoded
// it), and uses no proxy.
//
// The returned func closes the transport's connections, including a first
// connection no request used.
func (s *Server) upstreamTransport(host string, first net.Conn) (*http.Transport, func()) {
	var mu sync.Mutex
	pending := first
	take := func() net.Conn {
		mu.Lock()
		defer mu.Unlock()
		c := pending
		pending = nil
		return c
	}
	t := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if c := take(); c != nil {
				return c, nil
			}
			return s.dialUpstream(ctx, host)
		},
		DisableCompression:    true,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       jailIdleTimeout,
		ResponseHeaderTimeout: upstreamResponseHeaderTimeout,
	}
	return t, func() {
		if c := take(); c != nil {
			_ = c.Close()
		}
		t.CloseIdleConnections()
	}
}

// intercept serves an admitted CONNECT to host once handleRequest has a
// verified upstream connection for it. It answers the CONNECT, terminates the
// jail's TLS with a leaf for host, and relays every request the policy
// admits. target is the CONNECT authority as the client sent it, for denial
// records.
//
// It runs on the handler's goroutine after the hijack, so net/http's
// per-connection recover still covers the parsing below.
func (s *Server) intercept(w http.ResponseWriter, host, target string, upstream net.Conn) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "egress proxy: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		egressLog.Warn("hijack failed", "target", target, "error", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.closed {
		// Shutdown has already swept the sessions; one registered now
		// would outlive it.
		s.mu.Unlock()
		cancel()
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	s.sessions[client] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, client)
		s.mu.Unlock()
		cancel()
		_ = client.Close()
	}()

	transport, closeTransport := s.upstreamTransport(host, upstream)
	defer closeTransport()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	// A failed handshake is recorded too: besides a refused server name, it is
	// how a toolchain that does not trust the run's CA shows up. The error
	// after the reason's prefix varies by client (OpenSSL and Node clients
	// show as a bad record MAC or EOF, Go's as a bad certificate), so the
	// reason names the likely cause itself.
	var mismatched *string
	jail := tls.Server(bufferedConn(client, brw.Reader), jailTLSConfig(host, s.ca, func(serverName string) {
		mismatched = &serverName
	}))
	_ = jail.SetDeadline(time.Now().Add(jailHandshakeTimeout))
	if err := jail.HandshakeContext(ctx); err != nil {
		reason := fmt.Sprintf("TLS handshake with the sandbox client failed (usually a client that does not trust the run's CA bundle): %v", err)
		if mismatched != nil {
			reason = fmt.Sprintf("TLS server name %q is not the CONNECT host %q", quotable(*mismatched), host)
		}
		s.recordDenial(target, reason)
		return
	}
	// close_notify before the TCP close, so a body that ends with the
	// connection reads as complete rather than truncated.
	defer func() { _ = jail.Close() }()
	_ = jail.SetDeadline(time.Time{})

	s.serveSession(ctx, jail, host, target, transport)
}

// serveSession reads the jail's requests one at a time and relays each the
// policy admits. A refusal answers the jail and closes the session, since a
// refused request's body is never read.
func (s *Server) serveSession(ctx context.Context, jail *tls.Conn, host, target string, transport http.RoundTripper) {
	br := bufio.NewReaderSize(jail, maxRequestHead)
	bw := bufio.NewWriter(jail)
	for {
		_ = jail.SetReadDeadline(time.Now().Add(jailIdleTimeout))
		head, err := peekRequestHead(br)
		if err != nil {
			if errors.Is(err, errHeadTooLarge) {
				s.refuseRequest(bw, target, nil, refusal{http.StatusRequestHeaderFieldsTooLarge, "request head is larger than the egress proxy accepts"})
			}
			return
		}
		if ref := ambiguousFraming(head); ref != nil {
			s.refuseRequest(bw, target, nil, *ref)
			return
		}
		req, err := http.ReadRequest(br)
		if err != nil {
			s.refuseRequest(bw, target, nil, refusal{http.StatusBadRequest, fmt.Sprintf("malformed request: %v", err)})
			return
		}
		_ = jail.SetReadDeadline(time.Time{})
		if ref := s.requestPolicy(host, req); ref != nil {
			s.refuseRequest(bw, target, req, *ref)
			return
		}
		if !s.relay(ctx, bw, host, target, req, transport) {
			return
		}
	}
}

// relay forwards one admitted request to host and writes the registry's
// response to the jail as it arrived: status, headers less the hop-by-hop
// ones, and the body copied byte for byte. Nothing here can rewrite a body;
// there is no hook for it. Reports whether the session can take another
// request.
//
// A session closes after a POST, so a body the registry answered without
// reading in full can never be parsed as the next request.
func (s *Server) relay(ctx context.Context, bw *bufio.Writer, host, target string, req *http.Request, transport http.RoundTripper) bool {
	if req.Method == http.MethodPost && strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
		// The client waits for this before sending the body the upstream
		// request is about to read.
		if _, err := bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return false
		}
		if err := bw.Flush(); err != nil {
			return false
		}
	}
	resp, err := transport.RoundTrip(outboundRequest(ctx, host, req))
	if err != nil {
		// A redial the denylist or verification refused is a denial; any
		// other failure is the registry or the network, answered just as
		// loudly but not recorded as policy.
		var pd policyDenial
		var tlsErr upstreamTLSError
		switch {
		case errors.As(err, &pd):
			s.refuseRequest(bw, target, req, refusal{http.StatusForbidden, pd.reason})
		case errors.As(err, &tlsErr):
			s.refuseRequest(bw, target, req, refusal{http.StatusBadGateway, tlsErr.Error()})
		default:
			egressLog.Warn("upstream request failed", "target", target, "error", err)
			writeRefusal(bw, http.StatusBadGateway, fmt.Sprintf("upstream request to %s failed: %v", host, err))
		}
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Uncompressed {
		// The transport decompressed a body, which DisableCompression rules
		// out; refuse rather than hand the jail bytes the registry didn't send.
		s.refuseRequest(bw, target, req, refusal{http.StatusBadGateway, "upstream response was decoded in transit"})
		return false
	}
	removeHopByHop(resp.Header)
	// Whether the jail's session stays open is the jail's connection, not the
	// registry's: a registry closing its side costs a redial on the next
	// request, not the session. A body with neither a length nor chunking can
	// only be ended by closing.
	resp.Close = req.Close || req.Method == http.MethodPost ||
		(resp.ContentLength < 0 && !isChunked(resp.TransferEncoding))
	if err := resp.Write(bw); err != nil {
		return false
	}
	if err := bw.Flush(); err != nil {
		return false
	}
	return !resp.Close
}

func isChunked(te []string) bool { return len(te) > 0 && te[0] == "chunked" }

// requestPolicy decides one request against the session's host. Nil admits
// it. The checks are ordered so the reason names the first rule broken.
func (s *Server) requestPolicy(host string, req *http.Request) *refusal {
	if req.ProtoMajor != 1 {
		return &refusal{http.StatusHTTPVersionNotSupported, "only HTTP/1.x is accepted inside the tunnel"}
	}
	if req.Method == http.MethodConnect {
		return &refusal{http.StatusForbidden, "CONNECT inside the tunnel is refused"}
	}
	if !strings.HasPrefix(req.RequestURI, "/") {
		return &refusal{http.StatusBadRequest, "the request target must be a path (origin form)"}
	}
	if !hostMatches(req.Host, host) {
		return &refusal{http.StatusForbidden, fmt.Sprintf("Host %q is not the CONNECT host %q", quotable(req.Host), host)}
	}
	if req.Header.Get("Upgrade") != "" || headerHasToken(req.Header, "Connection", "upgrade") {
		return &refusal{http.StatusForbidden, "protocol upgrades are refused"}
	}
	if name, ok := methodOverride(req); ok {
		return &refusal{http.StatusForbidden, fmt.Sprintf("the method override %s is refused", name)}
	}
	if e := req.Header.Get("Expect"); e != "" && !strings.EqualFold(e, "100-continue") {
		return &refusal{http.StatusExpectationFailed, fmt.Sprintf("Expect %q is not supported", quotable(e))}
	}
	hasBody := req.ContentLength != 0 || len(req.TransferEncoding) > 0
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		if hasBody {
			return &refusal{http.StatusForbidden, "a GET or HEAD with a body is refused"}
		}
		return nil
	case http.MethodPost:
		if _, ok := s.registries[host][req.URL.Path]; ok && req.URL.RawPath == "" {
			return nil
		}
		return &refusal{http.StatusForbidden, fmt.Sprintf(
			"package registries are read-only from this sandbox, and this path is not one of %s's read endpoints", host)}
	default:
		return &refusal{http.StatusForbidden, fmt.Sprintf(
			"package registries are read-only from this sandbox; only GET, HEAD and POST to a registry's read endpoints are allowed, so %s is refused", req.Method)}
	}
}

// outboundRequest is the request sent upstream for in: the same method, path,
// query and end-to-end headers, addressed to host and nothing else. Only a
// POST carries a body, since the policy has already refused a body on
// anything else.
func outboundRequest(ctx context.Context, host string, in *http.Request) *http.Request {
	header := in.Header.Clone()
	removeHopByHop(header)
	header.Del("Host")
	header.Del("Expect")
	if _, ok := header["User-Agent"]; !ok {
		// An empty value makes the transport send no User-Agent, where it
		// would otherwise add its own.
		header["User-Agent"] = []string{""}
	}
	out := &http.Request{
		Method: in.Method,
		URL: &url.URL{
			Scheme:     "https",
			Host:       host,
			Path:       in.URL.Path,
			RawPath:    in.URL.RawPath,
			RawQuery:   in.URL.RawQuery,
			ForceQuery: in.URL.ForceQuery,
		},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
		Host:       host,
	}
	if in.Method == http.MethodPost {
		out.Body = in.Body
		out.ContentLength = in.ContentLength
	}
	return out.WithContext(ctx)
}

// refuseRequest answers the jail with ref and records the denial. req is nil
// when the request never parsed.
func (s *Server) refuseRequest(bw *bufio.Writer, target string, req *http.Request, ref refusal) {
	reason := ref.reason
	if req != nil {
		reason = fmt.Sprintf("%s %q: %s", quotable(req.Method), quotable(req.URL.Path), ref.reason)
	}
	s.recordDenial(target, reason)
	writeRefusal(bw, ref.status, reason)
}

// writeRefusal answers the jail with status and reason and ends the session.
// The reason is also the status line's phrase, which is the part of an error
// response npm and pip print.
func writeRefusal(bw *bufio.Writer, status int, reason string) {
	body := "egress proxy: " + reason + "\n"
	fmt.Fprintf(bw, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nX-Content-Type-Options: nosniff\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, reasonPhrase(reason), len(body), body)
	_ = bw.Flush()
}

// peekRequestHead returns the next request's line and headers, through the
// blank line that ends them, without consuming anything: http.ReadRequest
// reads them next, from the same buffer. The raw head is what
// ambiguousFraming needs, because ReadRequest drops Content-Length when
// Transfer-Encoding is set.
//
// Lines end in LF with an optional CR before it, as net/textproto reads
// them. Each byte is searched once however slowly the head arrives: searched
// is how far the search for a line end has got, and lineStart where the line
// being read began.
func peekRequestHead(br *bufio.Reader) ([]byte, error) {
	lineStart, searched := 0, 0
	for n := 1; ; {
		if _, err := br.Peek(n); err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, errHeadTooLarge
			}
			return nil, err
		}
		buf, _ := br.Peek(br.Buffered())
		for {
			i := bytes.IndexByte(buf[searched:], '\n')
			if i < 0 {
				searched = len(buf)
				break
			}
			end := searched + i
			line := bytes.TrimSuffix(buf[lineStart:end], []byte("\r"))
			lineStart, searched = end+1, end+1
			if len(line) == 0 {
				return buf[:lineStart], nil
			}
		}
		if len(buf) >= br.Size() {
			return nil, errHeadTooLarge
		}
		n = len(buf) + 1
	}
}

// ambiguousFraming refuses a head whose body length two parsers could read
// differently: Content-Length together with Transfer-Encoding, or more than
// one Content-Length. A line starting with a space or tab continues the
// previous header's value, as net/textproto folds it.
func ambiguousFraming(head []byte) *refusal {
	var contentLengths, transferEncodings int
	for i, line := range bytes.Split(head, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if i == 0 || len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		name, _, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		switch {
		case bytes.EqualFold(name, []byte("Content-Length")):
			contentLengths++
		case bytes.EqualFold(name, []byte("Transfer-Encoding")):
			transferEncodings++
		}
	}
	if contentLengths > 0 && transferEncodings > 0 {
		return &refusal{http.StatusBadRequest, "a request with both Content-Length and Transfer-Encoding is refused"}
	}
	if contentLengths > 1 {
		return &refusal{http.StatusBadRequest, "a request with more than one Content-Length is refused"}
	}
	return nil
}

// hostMatches reports whether a request's Host names host, with no port or
// the CONNECT port.
func hostMatches(hostHeader, host string) bool {
	name := hostHeader
	if h, port, err := net.SplitHostPort(hostHeader); err == nil {
		if port != connectPort {
			return false
		}
		name = h
	}
	return normalizeHost(name) == host
}

// methodOverride reports the first method override on req: a header from
// methodOverrideHeaders in any spelling, or a `_method` query parameter.
func methodOverride(req *http.Request) (string, bool) {
	for name := range req.Header {
		normalized := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
		for _, h := range methodOverrideHeaders {
			if normalized == h {
				return "header " + quotable(name), true
			}
		}
	}
	for key := range req.URL.Query() {
		if strings.EqualFold(key, "_method") {
			return "parameter " + quotable(key), true
		}
	}
	return "", false
}

// quotable is a client-supplied value cut to maxQuotedValue bytes for a
// refusal, so one request cannot make its denial record arbitrarily large.
func quotable(v string) string {
	if len(v) <= maxQuotedValue {
		return v
	}
	return strings.ToValidUTF8(v[:maxQuotedValue], "") + "..."
}

// headerHasToken reports whether any of name's comma-separated values is
// token, ignoring case.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// removeHopByHop deletes the connection-scoped headers from h, including any
// named in its Connection header.
func removeHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// bufferedConn is conn with the bytes already buffered in br read first:
// anything the client sent behind its CONNECT before the hijack, which is the
// start of its TLS handshake.
func bufferedConn(conn net.Conn, br *bufio.Reader) net.Conn {
	n := br.Buffered()
	if n == 0 {
		return conn
	}
	pre, _ := br.Peek(n)
	return &prefixedConn{Conn: conn, r: io.MultiReader(bytes.NewReader(append([]byte(nil), pre...)), conn)}
}

type prefixedConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
