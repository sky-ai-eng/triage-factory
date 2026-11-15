// The session blob: an SDK conversation's Claude Code session file, kept in
// the blob store under the conversation's own key.
//
// `claude --resume` reads a session from ~/.claude/projects/<run root>, which
// in local mode is outside the run tree, so nothing that keeps or rebuilds the
// tree keeps the session. Its blob is apart from the workspace snapshot for
// the same reason the two have different lifetimes: the snapshot is the
// task's, and retention collects it once the task goes idle, while a session
// is one conversation's, and a person may come back to a chat long after its
// tree and snapshot are gone. An SDK conversation holding its session resumes
// onto any tree, one built from nothing included, as a native one resumes
// from its `messages` rows.
//
// The file is never rebuilt from `messages`. It is Claude Code's private
// format, which changes between versions, and the stream TF records does not
// carry compactions or redacted thinking.
//
// Three things touch a session blob:
//
//   - every SDK ending that snapshots the workspace saves the session first,
//     while its engagement still holds the claim (saveSession);
//   - every launch and resume of an SDK session puts the file back where
//     --resume reads it when it is not there, whatever the tree came from
//     (restoreSession);
//   - the sweep beside the snapshot reaper deletes the blob once its
//     conversation has ended or its row is gone (ReapEndedSessions). It is
//     the only thing that deletes one, so a boundary stamped anywhere, or a
//     purge, leaves no blob behind.

package delegate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// sessionBlobPrefix is the prefix every session blob's key starts with. It is
// outside every org's workspace prefix so the sweep lists the session blobs
// alone, and so nothing that walks an org's snapshot keys meets one.
const sessionBlobPrefix = "sessions/"

// sessionBlobLeafSuffix ends every session blob's key.
const sessionBlobLeafSuffix = ".jsonl"

// sessionBlobKey is the storage key of conversationID's session blob.
func sessionBlobKey(orgID, conversationID string) string {
	return sessionBlobPrefix + orgID + "/" + conversationID + sessionBlobLeafSuffix
}

// sessionBlobOwner is sessionBlobKey read backwards. ok is false for a key of
// any other shape.
func sessionBlobOwner(key string) (orgID, conversationID string, ok bool) {
	rest, ok := strings.CutPrefix(key, sessionBlobPrefix)
	if !ok {
		return "", "", false
	}
	orgID, leaf, ok := strings.Cut(rest, "/")
	if !ok || orgID == "" || strings.Contains(leaf, "/") {
		return "", "", false
	}
	conversationID, ok = strings.CutSuffix(leaf, sessionBlobLeafSuffix)
	if !ok || conversationID == "" {
		return "", "", false
	}
	return orgID, conversationID, true
}

// sessionSaveTimeout bounds one session blob write. The write runs detached
// from the ending's context, which a stop has already cancelled.
const sessionSaveTimeout = detachedWriteDeadline

// saveSession writes the session file sessionID's agent left for cwd to
// conversationID's session blob. A no-op with no blob store, no session or no
// cwd. Best-effort, like the snapshot beside it: a session file this cannot
// read or a write that fails leaves the blob as an earlier ending saved it,
// and is logged.
//
// The caller saves before it lets go of its claim, so no later engagement on
// the conversation can have written a newer copy for this one to overwrite.
// An engagement whose claim was released under it may still get here, as it
// still files its memory; the ending of the engagement that took the
// conversation over saves again.
func (s *Spawner) saveSession(ctx context.Context, orgID, conversationID, cwd, sessionID string) {
	blobs := s.Storage()
	if blobs == nil || sessionID == "" || cwd == "" {
		return
	}
	ctx, span := tracer.Start(ctx, "session.save")
	defer span.End()
	data, ok := worktree.ReadClaudeSessionTranscript(cwd, sessionID)
	if !ok {
		span.SetAttributes(telemetry.Outcome("absent"))
		delegateLog.WarnContext(ctx, "the session file is not on disk at this ending; its blob keeps what an earlier ending saved",
			"conversation", conversationID, "session", sessionID, "cwd", cwd)
		return
	}
	span.SetAttributes(telemetry.SizeBytes(int64(len(data))))
	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSaveTimeout)
	defer cancel()
	if err := blobs.Put(putCtx, sessionBlobKey(orgID, conversationID), bytes.NewReader(data)); err != nil {
		telemetry.RecordSpanError(span, err, "writing the session blob failed")
		delegateLog.WarnContext(ctx, "write the session blob failed; it keeps what an earlier ending saved",
			"conversation", conversationID, "session", sessionID, "error", err)
	}
}

// restoreSession puts conversationID's session file where `claude --resume
// sessionID` reads it for cwd, from the conversation's session blob, when the
// file is not there. A file that is there is never replaced: it is the copy
// the blob was saved from, or one the agent has written to since.
//
// A conversation with no blob is no error, and neither is no session or no
// blob store; the caller's own check of the file then decides. An error is a
// store or disk fault, which a retry may not meet.
func (s *Spawner) restoreSession(ctx context.Context, orgID, conversationID, cwd, sessionID string) error {
	blobs := s.Storage()
	if blobs == nil || sessionID == "" || cwd == "" || sessionTranscriptExists(cwd, sessionID) {
		return nil
	}
	rc, err := blobs.Get(ctx, sessionBlobKey(orgID, conversationID))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the session blob: %w", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read the session blob: %w", err)
	}
	if err := writeSessionFile(cwd, sessionID, data); err != nil {
		return err
	}
	delegateLog.InfoContext(ctx, "session file restored from its blob", "conversation", conversationID, "session", sessionID, "cwd", cwd)
	return nil
}

// sessionSaved reports whether conversationID has a session blob.
func (s *Spawner) sessionSaved(ctx context.Context, orgID, conversationID string) (bool, error) {
	blobs := s.Storage()
	if blobs == nil {
		return false, nil
	}
	return blobs.Exists(ctx, sessionBlobKey(orgID, conversationID))
}

// writeSessionFile writes data as sessionID's session file for cwd, through a
// rename, so the file --resume reads is whole or absent: restoreSession takes
// a file that exists to be the session, and a torn write would pass for one.
func writeSessionFile(cwd, sessionID string, data []byte) error {
	p, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(cwd), sessionID)
	if err != nil {
		return fmt.Errorf("session path: %w", err)
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir session dir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // a no-op once renamed into place
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write session file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("flush session file: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("install session file: %w", err)
	}
	return nil
}

// liveSessionsSaved reports whether every session in root's project directory
// that a conversation on taskID can still resume has a blob, which is what
// removing that directory with the tree needs: a top-level SDK conversation
// that has not ended resumes from its session, and the directory holds the
// session files of every conversation that ran in the task's tree. A session
// file that is not on disk needs no blob for the directory's sake.
func (s *Spawner) liveSessionsSaved(ctx context.Context, orgID, taskID, root string) (bool, error) {
	if s.conversations == nil {
		return true, nil
	}
	convs, err := s.conversations.ListForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		return false, fmt.Errorf("list the task's conversations: %w", err)
	}
	for _, c := range convs {
		if c.Runtime != domain.ConversationRuntimeSDK || c.SessionID == "" || c.EndedAt != nil ||
			c.ParentConversationID != "" || !sessionTranscriptExists(root, c.SessionID) {
			continue
		}
		saved, err := s.sessionSaved(ctx, orgID, c.ID)
		if err != nil {
			return false, fmt.Errorf("read conversation %s's session blob: %w", c.ID, err)
		}
		if !saved {
			return false, nil
		}
	}
	return true, nil
}

// ReapEndedSessions deletes the session blob of every conversation that has
// ended, or whose row is gone: neither will be resumed again. It lists the
// store rather than the database, since a purged conversation has no row to
// enumerate. Best-effort: a blob whose conversation cannot be read is kept for
// the next sweep, and every failure is logged.
func (s *Spawner) ReapEndedSessions(ctx context.Context) {
	blobs := s.Storage()
	if blobs == nil || s.conversations == nil {
		return
	}
	objects, err := blobs.List(ctx, sessionBlobPrefix)
	if err != nil {
		delegateLog.Warn("session reaper: list session blobs failed", "error", err)
		return
	}
	deleted := 0
	for _, obj := range objects {
		orgID, conversationID, ok := sessionBlobOwner(obj.Key)
		if !ok {
			continue
		}
		conv, err := s.conversations.GetSystem(ctx, orgID, conversationID)
		if err != nil {
			delegateLog.Warn("session reaper: read the session's conversation failed; keeping its blob",
				"org", orgID, "conversation", conversationID, "error", err)
			continue
		}
		if conv != nil && conv.EndedAt == nil {
			continue
		}
		if err := blobs.Delete(ctx, obj.Key); err != nil {
			delegateLog.Warn("session reaper: delete a session blob failed", "key", obj.Key, "error", err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		delegateLog.Info("session reaper: deleted the session blobs of ended conversations", "count", deleted)
	}
}
