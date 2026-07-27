package gitproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// serveReceivePack proxies a push while teeing its body, then reports each
// non-delete ref it carried — together with the upstream's final status — to
// RecordPush. The recording runs only after ServeHTTP returns — the upstream
// response is fully written to the client by then — so it can neither block,
// alter, nor fail the push or its response. The handler goroutine does stay
// alive until recording finishes, so a graceful Shutdown waits for it (bounded
// by recordPushTimeout); the integration test relies on exactly that to
// synchronize. Every final status is reported, success and failure alike: the
// wiring gates artifact creation on 2xx and records the refused attempt
// (401/403/5xx — nothing landed) as an audit-only failure row, so the external
// action log never silently omits a push attempt.
func (s *Server) serveReceivePack(w http.ResponseWriter, r *http.Request) {
	tee := &receivePackCapture{rc: r.Body, max: maxReceivePackCapture}
	r.Body = tee
	repoPath := receivePackRepoPath(r.URL.Path)

	sr := &statusRecorder{ResponseWriter: w}
	s.proxy.ServeHTTP(sr, r)

	s.recordReceivePack(repoPath, tee.buf, sr.status)
}

// recordReceivePack parses the captured command block and invokes RecordPush
// once per non-delete ref (stamping the upstream's final status on each), under
// a context whose deadline mirrors the pre-push hook's cap. Self-guarding on a
// nil RecordPush (no-op) so the safety is local rather than an invariant every
// caller must uphold — the Handler does route pushes here only when RecordPush
// is wired (a nil-RecordPush observe push takes the plain proxy path, skipping
// the tee entirely), but that routing lives a file away and shouldn't be the
// only thing standing between a future caller and a panic.
func (s *Server) recordReceivePack(repoPath string, body []byte, status int) {
	if s.cfg.RecordPush == nil {
		return
	}
	cmds := parseReceivePackCommands(body)
	if len(cmds) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordPushTimeout)
	defer cancel()
	s.dispatchPushes(ctx, repoPath, cmds, status)
}

// dispatchPushes invokes RecordPush once per ref, stopping as soon as ctx is
// done. Without the early-out, a multi-ref push whose first upsert exhausts the
// bounded window would have every remaining call observe ctx.Err() and fail — a
// burst of guaranteed-failing work and logs. Stopping instead records fewer
// refs for that (pathological) push, which is the best-effort contract.
func (s *Server) dispatchPushes(ctx context.Context, repoPath string, cmds []refUpdate, status int) {
	for _, c := range cmds {
		if ctx.Err() != nil {
			break
		}
		s.cfg.RecordPush(ctx, PushedRef{
			Repo:    repoPath,
			Ref:     c.ref,
			NewSHA:  c.newSHA,
			Created: c.created,
			Status:  status,
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
	// Capture the first FINAL status only. A backend 1xx interim response —
	// chiefly 100 Continue, which a git client elicits with Expect:
	// 100-continue and httputil.ReverseProxy forwards by calling WriteHeader
	// with the 1xx code on this very writer — must not latch the 2xx gate; the
	// real 2xx/4xx/5xx arrives in a later call. Every code is still passed
	// through so the informational response reaches the client unchanged.
	if w.status == 0 && code >= 200 {
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

// serveReceivePackGated is the enforcing receive-pack path (multi mode, an
// Authorize gate wired). Unlike the observe-only serveReceivePack, it buffers
// the leading pkt-line command block and validates every ref update BEFORE
// forwarding: a delete, or a ref outside allowedRefs, rejects the WHOLE push
// (single stream — can't partially forward) with a 403 and no upstream call.
// A command block that overflows the capture cap before its flush-pkt fails
// closed. On success it reconstructs the body (buffered block + the still-
// unread packfile) and proxies it, then reports each ref + the upstream's
// final status to RecordPush exactly like the observe path (the wiring gates
// artifacts on 2xx and records a refused push as an audit failure row).
func (s *Server) serveReceivePackGated(w http.ResponseWriter, r *http.Request, owner, repo string, decision Decision) {
	allowedRefs := decision.AllowedRefs
	block, sawFlush, err := readCommandBlock(r.Body, maxReceivePackCapture)
	if err != nil {
		http.Error(w, "gitproxy: malformed receive-pack request", http.StatusBadRequest)
		return
	}
	if !sawFlush {
		// The command list didn't terminate within the cap — we can't see all
		// the refs being pushed, so fail closed rather than forward blind.
		s.recordDenial(DeniedGitOp{Owner: owner, Repo: repo, Op: gitOpPush, Reason: "command-block-too-large"})
		http.Error(w, "gitproxy: receive-pack command block too large", http.StatusForbidden)
		return
	}

	allowed := refSet(allowedRefs)
	for _, c := range parseGateCommands(block) {
		switch {
		case c.isDelete:
			s.recordDenial(DeniedGitOp{Owner: owner, Repo: repo, Ref: c.ref, Op: gitOpPush, Reason: "ref-delete"})
			http.Error(w, "gitproxy: ref delete not allowed", http.StatusForbidden)
			return
		case !allowed[c.ref]:
			reason, msg := refDenial(c.ref, owner+"/"+repo, allowedRefs, decision.ProtectedRefs)
			s.recordDenial(DeniedGitOp{Owner: owner, Repo: repo, Ref: c.ref, Op: gitOpPush, Reason: reason})
			http.Error(w, msg, http.StatusForbidden)
			return
		}
	}

	// Every command authorized (or the push had no commands — a capability
	// probe). Reconstruct the body: the buffered command block followed by the
	// untouched packfile still sitting in r.Body. Byte-for-byte the original
	// stream, so Content-Length is unchanged.
	orig := r.Body
	r.Body = &reconstructedBody{r: io.MultiReader(bytes.NewReader(block), orig), c: orig}

	// owner/repo were already parsed + charset-validated by parseGitPath, so use
	// them directly rather than re-deriving from the URL (receivePackRepoPath
	// stays the legacy observe-path's helper).
	repoPath := owner + "/" + repo
	sr := &statusRecorder{ResponseWriter: w}
	s.proxy.ServeHTTP(sr, r)

	// Outcome record (success artifact / failure audit — the wiring branches
	// on the status), same contract as the observe path. The command block is
	// already in hand, so re-parse the non-delete refs from it rather than
	// re-reading the (consumed) body. recordReceivePack no-ops on a nil
	// RecordPush (the gated path runs regardless, since the ref gate is
	// enforcement, not recording).
	s.recordReceivePack(repoPath, block, sr.status)
}

// refDenial builds the audit reason + 403 body for one rejected ref. Three
// situations reach this point and they have three different remedies, so they
// get three messages rather than the one flat string every refusal used to
// collapse to:
//
//   - The ref is protected — the repo's base/default branch, which the team's
//     base-branch push policy refuses. Name it, and name both ways out: the
//     branch-plus-PR path the agent can take right now, and the policy a team
//     admin can change.
//   - The run has no pushable branch here at all (empty allowlist): the repo is
//     authorized for read but no worktree of it is recorded for this run, so
//     push authority hasn't been earned yet.
//   - The ref simply isn't the branch this run's worktree is on.
//
// Wording mirrors the repo-level deny builders in internal/delegate
// (gitDenyNotTracked and friends) — same "gitproxy: " prefix, same
// "what happened / what to do" shape — so an agent reading git's remote output
// gets one consistent voice. Keep them in sync.
func refDenial(ref, repoID string, allowedRefs, protectedRefs []string) (reason, msg string) {
	if slices.Contains(protectedRefs, ref) {
		return "ref-protected", fmt.Sprintf(
			"gitproxy: %s is a protected branch on %s (its base/default branch) and this team's base-branch push policy refuses pushes to it; commit to a new branch and open a pull request instead. A team admin can change the policy in Settings → Team.",
			ref, repoID)
	}
	if len(allowedRefs) == 0 {
		return "ref-no-pushable-branch", fmt.Sprintf(
			"gitproxy: no branch of %s is pushable in this run: push authority comes from a worktree TF materialized for the run, and none is recorded. If you just ran 'workspace add %s', retry once the checkout has landed.",
			repoID, repoID)
	}
	return "ref-not-allowed", fmt.Sprintf(
		"gitproxy: ref %s is not pushable in %s: this run may push only %s (the branch its worktree is checked out on). Push from that checkout, or create your branch there first.",
		ref, repoID, strings.Join(allowedRefs, ", "))
}

// readCommandBlock reads the leading pkt-line command block of a receive-pack
// body up to and including the terminating flush-pkt ("0000"), bounded by max.
// Returns the raw bytes read, whether the flush-pkt was seen (false ⇒ the cap
// was hit first — the caller fails closed), and a transport error on a short
// read. The packfile that follows the flush-pkt is left unread in r.
func readCommandBlock(r io.Reader, max int) (block []byte, sawFlush bool, err error) {
	var buf []byte
	hdr := make([]byte, pktLineLenWidth)
	for {
		if _, rerr := io.ReadFull(r, hdr); rerr != nil {
			// EOF/short read before a flush-pkt: malformed (a well-formed
			// command list always ends with one).
			return buf, false, rerr
		}
		buf = append(buf, hdr...)
		n, ok := pktLineLen(hdr)
		if !ok {
			return buf, false, fmt.Errorf("gitproxy: malformed pkt-line length")
		}
		if n == 0 {
			return buf, true, nil // flush-pkt: command list complete
		}
		if n < pktLineLenWidth {
			return buf, false, fmt.Errorf("gitproxy: invalid pkt-line length %d", n)
		}
		payloadLen := n - pktLineLenWidth
		if len(buf)+payloadLen > max {
			return buf, false, nil // would exceed the cap before flush — fail closed
		}
		payload := make([]byte, payloadLen)
		if _, rerr := io.ReadFull(r, payload); rerr != nil {
			return buf, false, rerr
		}
		buf = append(buf, payload...)
	}
}

// gateCommand is one ref-update command parsed for enforcement — including
// deletes (which parseReceivePackCommands drops), since the gate must reject
// them.
type gateCommand struct {
	ref      string
	isDelete bool
}

// parseGateCommands decodes every ref-update command from the pkt-line block,
// deletes included. Mirrors parseReceivePackCommands' framing but keeps
// zero-new-sha (delete) lines so the gate can reject them.
func parseGateCommands(body []byte) []gateCommand {
	var cmds []gateCommand
	for len(body) >= pktLineLenWidth {
		n, ok := pktLineLen(body[:pktLineLenWidth])
		if !ok || n == 0 {
			break
		}
		if n < pktLineLenWidth || n > len(body) {
			break
		}
		if c, ok := parseGateRefUpdate(body[pktLineLenWidth:n]); ok {
			cmds = append(cmds, c)
		}
		body = body[n:]
	}
	return cmds
}

// parseGateRefUpdate decodes one "<old-sha> <new-sha> <ref>" line into a
// gateCommand, flagging a delete (new-sha all zeros). ok=false for a line that
// isn't a 3-field command.
func parseGateRefUpdate(line []byte) (gateCommand, bool) {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	if i := bytes.IndexByte(line, 0); i >= 0 {
		line = line[:i] // strip "\0<capabilities>" (first command line only)
	}
	fields := strings.Fields(string(line))
	if len(fields) != 3 {
		return gateCommand{}, false
	}
	return gateCommand{ref: fields[2], isDelete: isZeroOID(fields[1])}, true
}

// refSet builds a lookup set from a slice of full refs.
func refSet(refs []string) map[string]bool {
	m := make(map[string]bool, len(refs))
	for _, ref := range refs {
		m[ref] = true
	}
	return m
}

// reconstructedBody re-presents a receive-pack body whose command block was
// buffered for the gate: Read serves the buffered block then the original
// body's remaining packfile (via an io.MultiReader); Close closes the original
// body.
type reconstructedBody struct {
	r io.Reader
	c io.Closer
}

func (b *reconstructedBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *reconstructedBody) Close() error               { return b.c.Close() }
