package agenthost

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Wire format: length-prefixed JSON frames.
//
//	┌──────────────┬─────────────┐
//	│ uint32 BE    │ length bytes │
//	│ frame length │ JSON payload │
//	└──────────────┴─────────────┘
//
// One frame in, one frame out per call. The protocol is intentionally
// the smallest thing that works: no streaming, no multiplexing, no
// keep-alive — a fresh connection per RPC. The agenthost socket lives
// for at most the lifetime of one delegated run; serializing on a
// single conn per call avoids any concurrency bookkeeping in the
// daemon and matches what the cmd/exec process actually wants (one
// shell invocation = one RPC).
//
// Choice of length-prefixed JSON over net/rpc + jsonrpc codec: same
// dependency surface (stdlib only), but no method-registration ritual
// and the frame is trivially inspectable in tcpdump / strace. The
// frame length cap below protects the daemon against a malformed
// client sending a 4GB header.
const maxFrameSize = 16 * 1024 * 1024 // 16 MiB; pending review comments etc. easily fit

// request is the envelope for every RPC. Method identifies the
// operation; Args is the method-specific payload (JSON-encoded). The
// daemon dispatches on Method and unmarshals Args into the matching
// argv shape.
//
// Version is the protocol version the client is built for. The daemon
// rejects requests with a mismatching version so an old binary can't
// silently misinterpret a new method's args. Live deployments will
// usually be lock-step but defensive matters because the sandbox
// bind-mounts the host binary — a rolling upgrade where the host
// daemon advanced before the worker's binary refreshes is a real
// scenario.
type request struct {
	Version uint32          `json:"v"`
	Method  string          `json:"m"`
	Args    json.RawMessage `json:"a,omitempty"`
}

// response wraps either a method-specific Result (success) or an
// Error string (failure). The daemon never returns a partially-
// populated response — exactly one of Result / Error is set.
//
// Error is a plain string rather than a typed error code because the
// only consumer is cmd/exec subcommands that surface the message
// verbatim to the agent via stderr; adding code-based routing would
// be premature.
type response struct {
	Result json.RawMessage `json:"r,omitempty"`
	Error  string          `json:"e,omitempty"`
}

// writeFrame serializes msg as a length-prefixed JSON frame on w.
// JSON marshal failures are returned without writing anything — the
// caller can still send a follow-up error frame.
func writeFrame(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("agenthost: marshal frame: %w", err)
	}
	if len(body) > maxFrameSize {
		return fmt.Errorf("agenthost: frame %d bytes exceeds cap %d", len(body), maxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("agenthost: write frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("agenthost: write frame body: %w", err)
	}
	return nil
}

// readFrame reads one length-prefixed JSON frame from r and decodes
// it into dst. EOF on the header read is returned verbatim so callers
// (the daemon's accept loop in particular) can tell a clean connection
// close apart from a malformed frame.
func readFrame(r io.Reader, dst any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		// io.ReadFull returns io.EOF when zero bytes are read and the
		// stream is closed cleanly; io.ErrUnexpectedEOF when some bytes
		// were read first. Surface EOF as-is so the accept-loop can
		// detect graceful close; everything else wraps for context.
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("agenthost: read frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length > maxFrameSize {
		return fmt.Errorf("agenthost: frame %d bytes exceeds cap %d", length, maxFrameSize)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("agenthost: read frame body: %w", err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("agenthost: decode frame: %w", err)
	}
	return nil
}

// --- per-method argv shapes ---
//
// Kept in this file (rather than fanned out per concern) so the wire
// format lives in one place. Adding a method = appending one struct
// here + one case in dispatch.go + one method on IPCClient.

type lookupRunResult struct {
	Info RunInfo `json:"info"`
}

type byIDArgs struct {
	ID string `json:"id"`
}

type pendingReviewResult struct {
	Review *domain.PendingReview `json:"review,omitempty"`
}

type createPendingReviewArgs struct {
	Review domain.PendingReview `json:"review"`
}

type lockReviewSubmissionArgs struct {
	ReviewID string `json:"review_id"`
	Body     string `json:"body"`
	Event    string `json:"event"`
}

type addCommentArgs struct {
	Comment domain.PendingReviewComment `json:"comment"`
}

type updateCommentArgs struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

type listCommentsArgs struct {
	ReviewID string `json:"review_id"`
}

type listCommentsResult struct {
	Comments []domain.PendingReviewComment `json:"comments"`
}

type pendingPRResult struct {
	PR *domain.PendingPR `json:"pr,omitempty"`
}

type createAndLockPendingPRArgs struct {
	Row domain.PendingPR `json:"row"`
}

type lockPendingPRArgs struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type agentRunResult struct {
	Run *domain.AgentRun `json:"run,omitempty"`
}

type getTaskArgs struct {
	TaskID string `json:"task_id"`
}

type taskResult struct {
	Task *domain.Task `json:"task,omitempty"`
}

type reposResult struct {
	Repos []domain.RepoProfile `json:"repos"`
}

type getRepoArgs struct {
	RepoID string `json:"repo_id"`
}

type repoResult struct {
	Repo *domain.RepoProfile `json:"repo,omitempty"`
}

type runWorktreeByRepoArgs struct {
	RepoID string `json:"repo_id"`
}

type runWorktreeResult struct {
	Worktree *domain.RunWorktree `json:"worktree,omitempty"`
}

type runWorktreesResult struct {
	Worktrees []domain.RunWorktree `json:"worktrees"`
}

type insertRunWorktreeArgs struct {
	Row domain.RunWorktree `json:"row"`
}

type insertRunWorktreeResult struct {
	Inserted    bool   `json:"inserted"`
	WinningPath string `json:"winning_path"`
}

type deleteRunWorktreeByRepoArgs struct {
	RepoID string `json:"repo_id"`
}

type buildAgentRunFooterArgs struct {
	Kind string `json:"kind"`
}

type buildAgentRunFooterResult struct {
	Footer string `json:"footer"`
}

// emptyArgs is the args type for methods that take no parameters
// (LookupRun, GetPendingPRByRunID, GetAgentRun, ListRunWorktrees,
// ListRepos). Using an empty struct rather than json.RawMessage(nil)
// lets the daemon-side dispatch use the same json.Unmarshal call shape
// for every method without a nil-check.
type emptyArgs struct{}

type emptyResult struct{}

// methodCallNames are the wire-name constants. Used by both client
// and server so a rename here is the only edit needed to propagate.
const (
	methodLookupRun                  = "LookupRun"
	methodGetPendingReview           = "GetPendingReview"
	methodCreatePendingReview        = "CreatePendingReview"
	methodDeletePendingReview        = "DeletePendingReview"
	methodLockReviewSubmission       = "LockReviewSubmission"
	methodAddPendingReviewComment    = "AddPendingReviewComment"
	methodUpdatePendingReviewComment = "UpdatePendingReviewComment"
	methodDeletePendingReviewComment = "DeletePendingReviewComment"
	methodListPendingReviewComments  = "ListPendingReviewComments"
	methodGetPendingPRByRunID        = "GetPendingPRByRunID"
	methodCreateAndLockPendingPR     = "CreateAndLockPendingPR"
	methodLockPendingPR              = "LockPendingPR"
	methodGetAgentRun                = "GetAgentRun"
	methodGetTask                    = "GetTask"
	methodListRepos                  = "ListRepos"
	methodGetRepo                    = "GetRepo"
	methodGetRunWorktreeByRepo       = "GetRunWorktreeByRepo"
	methodListRunWorktrees           = "ListRunWorktrees"
	methodInsertRunWorktree          = "InsertRunWorktree"
	methodDeleteRunWorktreeByRepo    = "DeleteRunWorktreeByRepo"
	methodBuildAgentRunFooter        = "BuildAgentRunFooter"
)
