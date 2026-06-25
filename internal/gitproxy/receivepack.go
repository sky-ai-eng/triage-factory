package gitproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxReceivePackCapture bounds how much of a git-receive-pack body the
	// backstop buffers to find the ref-update commands. That command block
	// (one ~100-byte pkt-line per ref, then a flush-pkt) always precedes the
	// packfile, so 64 KiB covers hundreds of refs while the pack — which can
	// be gigabytes — is never buffered. A command list longer than this is
	// truncated: refs past the cap are missed, never mis-recorded (best-effort).
	maxReceivePackCapture = 64 << 10

	// recordPushTimeout bounds the artifact write the backstop kicks off after
	// a push completes, mirroring the pre-push hook's own 30s cap so a wedged
	// store can never hold the proxy's request handler open indefinitely.
	recordPushTimeout = 30 * time.Second

	// pktLineLenWidth is the width of a pkt-line's leading hex length field.
	// The 4 hex digits encode the length of the whole line, the 4 bytes
	// included; "0000" is the flush-pkt.
	pktLineLenWidth = 4

	// receivePackSuffix is the smart-HTTP path segment of the POST that
	// carries a push (its ref-update commands + packfile). The GET
	// "/info/refs?service=git-receive-pack" advertisement and all fetch
	// (upload-pack) paths do not end with this and are left untouched.
	receivePackSuffix = "/git-receive-pack"
)

// isReceivePackPush reports whether r is the POST carrying a push's body.
func isReceivePackPush(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, receivePackSuffix)
}

// receivePackRepoPath extracts the "owner/repo" a push targets from the request
// path "/owner/repo[.git]/git-receive-pack". The host is definitionally the
// proxy's configured upstream, so — unlike the pre-push hook, which parses an
// untrusted remote string and must host-gate it — no host check is needed here;
// the owner/repo shape is validated downstream by domain.NewBranchArtifact.
func receivePackRepoPath(p string) string {
	p = strings.TrimSuffix(p, receivePackSuffix)
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return p
}

// serveReceivePack proxies a push while teeing its body, then records a branch
// artifact for each non-delete ref it pushed. The recording runs only after
// ServeHTTP returns — i.e. after the response is fully back to the client — so
// it cannot block, alter, or fail the push. It is gated on a 2xx upstream
// status so a push the upstream refused (401/403/5xx — nothing landed) is not
// recorded.
func (s *Server) serveReceivePack(w http.ResponseWriter, r *http.Request) {
	tee := &receivePackCapture{rc: r.Body, max: maxReceivePackCapture}
	r.Body = tee
	repoPath := receivePackRepoPath(r.URL.Path)

	sr := &statusRecorder{ResponseWriter: w}
	s.proxy.ServeHTTP(sr, r)

	if sr.status >= 200 && sr.status < 300 {
		s.recordReceivePack(repoPath, tee.buf)
	}
}

// recordReceivePack parses the captured command block and invokes RecordPush
// once per non-delete ref, under a context whose deadline mirrors the pre-push
// hook's cap. RecordPush is guaranteed non-nil here (Handler only routes pushes
// through serveReceivePack when it is set).
func (s *Server) recordReceivePack(repoPath string, body []byte) {
	cmds := parseReceivePackCommands(body)
	if len(cmds) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordPushTimeout)
	defer cancel()
	s.dispatchPushes(ctx, repoPath, cmds)
}

// dispatchPushes invokes RecordPush once per ref, stopping as soon as ctx is
// done. Without the early-out, a multi-ref push whose first upsert exhausts the
// bounded window would have every remaining call observe ctx.Err() and fail — a
// burst of guaranteed-failing work and logs. Stopping instead records fewer
// refs for that (pathological) push, which is the best-effort contract.
func (s *Server) dispatchPushes(ctx context.Context, repoPath string, cmds []refUpdate) {
	for _, c := range cmds {
		if ctx.Err() != nil {
			break
		}
		s.cfg.RecordPush(ctx, PushedRef{
			Repo:    repoPath,
			Ref:     c.ref,
			NewSHA:  c.newSHA,
			Created: c.created,
		})
	}
}

// receivePackCapture wraps a git-receive-pack request body, copying its leading
// bytes (the pkt-line command block, which precedes the packfile) into buf as
// the body streams upstream. Capture stops at max bytes, so the packfile is
// never buffered. The bytes returned to the proxy are the underlying body's,
// unchanged — the capture is a side effect that never alters or blocks the
// upstream stream.
type receivePackCapture struct {
	rc  io.ReadCloser
	buf []byte
	max int
}

func (c *receivePackCapture) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 && len(c.buf) < c.max {
		room := c.max - len(c.buf)
		if room > n {
			room = n
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return n, err
}

func (c *receivePackCapture) Close() error { return c.rc.Close() }

// statusRecorder wraps the ResponseWriter for a proxied push to capture the
// upstream's final status code, so the backstop records a branch only for a
// push the upstream accepted (2xx). Flush is passed through so the
// ReverseProxy's response streaming is unaffected; Unwrap lets an
// http.ResponseController reach the underlying writer for anything else.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// refUpdate is one non-delete ref-update command parsed from a receive-pack
// request: the ref and the commit it now points to, plus whether the push
// created the ref (the remote held no prior value for it).
type refUpdate struct {
	ref     string
	newSHA  string
	created bool
}

// parseReceivePackCommands decodes the ref-update commands from the leading
// pkt-line block of a git-receive-pack request body. Each pkt-line is a
// 4-hex-digit length (the 4 bytes included) followed by
// "<old-sha> <new-sha> <ref>" — the first line also carrying
// "\0<capabilities>" — and the block is terminated by a flush-pkt ("0000"),
// after which the packfile follows.
//
// It returns the non-delete commands in order. Delete commands (new-sha all
// zeros) are dropped — no branch was pushed. Best-effort: malformed or
// truncated framing stops the scan and returns what decoded so far, so a parse
// miss can never fail the push.
func parseReceivePackCommands(body []byte) []refUpdate {
	var cmds []refUpdate
	for len(body) >= pktLineLenWidth {
		n, ok := pktLineLen(body[:pktLineLenWidth])
		if !ok || n == 0 {
			break // malformed length, or the flush-pkt ending the command list
		}
		if n < pktLineLenWidth || n > len(body) {
			break // truncated or oversized framing — stop, keep what we have
		}
		if u, ok := parseRefUpdate(body[pktLineLenWidth:n]); ok {
			cmds = append(cmds, u)
		}
		body = body[n:]
	}
	return cmds
}

// parseRefUpdate decodes one "<old-sha> <new-sha> <ref>" command line, dropping
// a trailing LF and the first line's "\0<capabilities>" suffix. It returns
// ok=false for a line that is not a 3-field command or that is a delete
// (new-sha all zeros) — neither records a branch.
func parseRefUpdate(line []byte) (refUpdate, bool) {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	if i := bytes.IndexByte(line, 0); i >= 0 {
		line = line[:i] // strip "\0<capabilities>" (first command line only)
	}
	fields := strings.Fields(string(line))
	if len(fields) != 3 {
		return refUpdate{}, false
	}
	oldSHA, newSHA, ref := fields[0], fields[1], fields[2]
	if isZeroOID(newSHA) {
		return refUpdate{}, false // delete — no branch pushed
	}
	return refUpdate{ref: ref, newSHA: newSHA, created: isZeroOID(oldSHA)}, true
}

// pktLineLen parses a pkt-line's 4-hex-digit length prefix.
func pktLineLen(b []byte) (int, bool) {
	n, err := strconv.ParseUint(string(b), 16, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// isZeroOID reports whether s is git's all-zeros object id — "no such ref". As
// the new-sha it marks a delete; as the old-sha it marks a ref the push
// creates. Matched by all-zeros (not a fixed width) so it covers both SHA-1
// (40) and SHA-256 (64) repositories; an empty string counts as zero too.
func isZeroOID(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
