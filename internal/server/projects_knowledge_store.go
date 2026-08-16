package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// kbChanged broadcasts the panel-refresh event and rings the cross-pod
// doorbell after a multi-mode KB mutation. op is "" for an upload/delete (the
// home executor materializes the write into a live session) and
// "project_deleted" for project teardown (the home executor drops its stale
// materialized dir). Both halves are best-effort: the broadcast reaches every
// browser via the WS backplane, the doorbell only optimizes executor-side
// latency, so a nil hub / unwired doorbell degrades gracefully.
func (s *Server) kbChanged(op, orgID, projectID string) {
	if s.ws != nil {
		s.ws.Broadcast(websocket.Event{
			Type:      "project_knowledge_updated",
			OrgID:     orgID,
			ProjectID: projectID,
		})
	}
	if s.kbChangedDoorbell != nil {
		s.kbChangedDoorbell(op, orgID, projectID)
	}
}

// listKnowledgeFromStore is the multi-mode counterpart of readKnowledgeFiles:
// it lists a project's KB from the blob store and maps it to the same
// knowledgeFile shape, inlining text content for files under the inline cap.
// The store lister already returns only flat, valid names, so this mirrors the
// local sanitize filter purely for exact parity with the on-disk path.
func (s *Server) listKnowledgeFromStore(ctx context.Context, orgID, projectID string) ([]knowledgeFile, error) {
	files, err := s.kb.List(ctx, orgID, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]knowledgeFile, 0, len(files))
	for _, f := range files {
		if _, errMsg := sanitizeKnowledgeFilename(f.Name); errMsg != "" {
			continue
		}
		mimeType := detectMimeType(f.Name)
		entry := knowledgeFile{
			Path:      f.Name,
			MimeType:  mimeType,
			UpdatedAt: f.ModTime.UTC().Format("2006-01-02T15:04:05Z07:00"),
			SizeBytes: f.Size,
		}
		if isTextMime(mimeType) && f.Size <= knowledgeInlineMaxBytes {
			if body, err := s.readInlineFromStore(ctx, orgID, projectID, f.Name); err == nil {
				entry.Content = body
			}
			// On a read error leave Content empty — the panel lazy-fetches it
			// from the raw endpoint, exactly as it does for large files.
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// readInlineFromStore reads a text KB file's inline content, bounded to the
// same cap the listing promises (defense in depth against a file that grew
// between List and read — mirrors readKnowledgeFiles).
func (s *Server) readInlineFromStore(ctx context.Context, orgID, projectID, name string) (string, error) {
	rc, err := s.kb.Get(ctx, orgID, projectID, name)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, knowledgeInlineMaxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > knowledgeInlineMaxBytes {
		body = body[:knowledgeInlineMaxBytes]
	}
	return string(body), nil
}

// serveKnowledgeFromStore streams a single KB file from the blob store,
// honoring a single HTTP byte range so video scrubbing works. It reads the
// object's size first (needed for Content-Range/Content-Length), then serves
// either a 206 for a satisfiable single range or a 200 full stream for an
// absent / malformed / multi range. If-Range is deliberately ignored (RFC
// permits serving the full representation), and the Content-Type / nosniff /
// inline-disposition headers match the on-disk path.
func (s *Server) serveKnowledgeFromStore(w http.ResponseWriter, r *http.Request, orgID, projectID, name string) {
	ctx := r.Context()
	info, err := s.kb.Stat(ctx, orgID, projectID, name)
	if err != nil {
		if errors.Is(err, kbstore.ErrNotFound) {
			notFound(w, "file")
			return
		}
		internalError(w, "projects", fmt.Errorf("stat knowledge file %s in store: %w", name, err))
		return
	}

	w.Header().Set("Content-Type", detectMimeType(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(name))
	w.Header().Set("Accept-Ranges", "bytes")
	if !info.ModTime.IsZero() {
		w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}

	if start, length, ok := parseSingleByteRange(r.Header.Get("Range"), info.Size); ok {
		rc, err := s.kb.GetRange(ctx, orgID, projectID, name, start, length)
		if err != nil {
			if errors.Is(err, kbstore.ErrNotFound) {
				notFound(w, "file")
				return
			}
			internalError(w, "projects", fmt.Errorf("read knowledge file range %s from store: %w", name, err))
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, info.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
		if _, err := io.Copy(w, rc); err != nil {
			projectsLog.Warn("knowledge fetch: stream range failed", "project", projectID, "file", name, "error", err)
		}
		return
	}

	rc, err := s.kb.Get(ctx, orgID, projectID, name)
	if err != nil {
		if errors.Is(err, kbstore.ErrNotFound) {
			notFound(w, "file")
			return
		}
		internalError(w, "projects", fmt.Errorf("read knowledge file %s from store: %w", name, err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if _, err := io.Copy(w, rc); err != nil {
		projectsLog.Warn("knowledge fetch: stream failed", "project", projectID, "file", name, "error", err)
	}
}

// parseSingleByteRange parses a single "Range: bytes=..." header against a
// known object size. It returns (start, length, true) for a satisfiable single
// range and (0, 0, false) for an absent, malformed, multi-range, or
// unsatisfiable header — the caller then serves the full 200. Supported forms:
// "bytes=start-" (to end), "bytes=start-end" (inclusive end, clamped to size),
// and "bytes=-suffix" (last suffix bytes).
func parseSingleByteRange(header string, size int64) (int64, int64, bool) {
	const prefix = "bytes="
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, prefix) || size <= 0 {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false // multi-range: fall back to the full 200
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])

	if startStr == "" {
		// Suffix range: the last N bytes.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if endStr == "" {
		return start, size - start, true // to end
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end - start + 1, true
}

// putUploadedKB streams one uploaded multipart file to the blob store,
// enforcing the same per-file byte cap the on-disk path does. It returns an
// empty string on success or a per-file error message. The conflict check
// (Exists) is the caller's, under the per-project lock; oversize detection here
// is belt-and-suspenders on top of the caller's declared-size check, matching
// writeUploadedFile: a stream that lies past the cap is removed rather than
// left half-written.
func (s *Server) putUploadedKB(ctx context.Context, orgID, projectID, name string, fh *multipart.FileHeader) string {
	src, err := fh.Open()
	if err != nil {
		return "open upload: " + err.Error()
	}
	defer src.Close()
	counter := &countingReader{r: io.LimitReader(src, knowledgeMaxUploadBytes+1)}
	if err := s.kb.Put(ctx, orgID, projectID, name, counter); err != nil {
		return "publish file: " + err.Error()
	}
	if counter.n > knowledgeMaxUploadBytes {
		if delErr := s.kb.Delete(ctx, orgID, projectID, name); delErr != nil {
			projectsLog.Warn("knowledge upload: remove oversize store blob failed", "project", projectID, "file", name, "error", delErr)
		}
		return fmt.Sprintf("file exceeds %d-byte limit", knowledgeMaxUploadBytes)
	}
	return ""
}

// countingReader tracks how many bytes were read through it so putUploadedKB
// can detect an upload that streamed past the per-file cap.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
