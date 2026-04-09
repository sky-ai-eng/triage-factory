import { useState, useEffect, useCallback } from "react";
import { motion, AnimatePresence } from "motion/react";
import { parseDiff } from "react-diff-view";
import type { FileData } from "react-diff-view";
import DiffFile from "./DiffFile";
import type { FileComment } from "./DiffFile";
import ReviewSummary from "./ReviewSummary";

interface PendingReview {
  id: string;
  pr_number: number;
  owner: string;
  repo: string;
  commit_sha: string;
  run_id: string;
  review_body: string;
  review_event: string;
  comments: {
    id: string;
    review_id: string;
    path: string;
    line: number;
    start_line?: number;
    body: string;
  }[];
}

interface Props {
  runID: string;
  open: boolean;
  onClose: () => void;
}

export default function ReviewOverlay({ runID, open, onClose }: Props) {
  const [review, setReview] = useState<PendingReview | null>(null);
  const [files, setFiles] = useState<FileData[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // Fetch review data + diff
  useEffect(() => {
    if (!open || !runID) return;
    let cancelled = false;
    setLoading(true);
    setError(null);

    (async () => {
      try {
        // Fetch review metadata + comments
        const reviewRes = await fetch(`/api/agent/runs/${runID}/review`);
        if (!reviewRes.ok) throw new Error("Failed to load review");
        const reviewData: PendingReview = await reviewRes.json();
        if (cancelled) return;
        setReview(reviewData);

        // Fetch diff
        const diffRes = await fetch(`/api/reviews/${reviewData.id}/diff`);
        if (!diffRes.ok) throw new Error("Failed to load diff");
        const diffText = await diffRes.text();
        if (cancelled) return;
        const parsed = parseDiff(diffText);
        setFiles(parsed);
      } catch (err: any) {
        if (!cancelled) setError(err.message);
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();

    return () => { cancelled = true; };
  }, [open, runID]);

  // Comment operations
  const handleUpdateComment = useCallback(async (commentId: string, body: string) => {
    if (!review) return;
    await fetch(`/api/reviews/${review.id}/comments/${commentId}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ body }),
    });
    setReview((prev) =>
      prev
        ? {
            ...prev,
            comments: prev.comments.map((c) =>
              c.id === commentId ? { ...c, body } : c,
            ),
          }
        : prev,
    );
  }, [review?.id]);

  const handleDeleteComment = useCallback(async (commentId: string) => {
    if (!review) return;
    await fetch(`/api/reviews/${review.id}/comments/${commentId}`, {
      method: "DELETE",
    });
    setReview((prev) =>
      prev
        ? { ...prev, comments: prev.comments.filter((c) => c.id !== commentId) }
        : prev,
    );
  }, [review?.id]);

  // Body + event updates — persist to DB
  const handleUpdateBody = useCallback(async (body: string) => {
    if (!review) return;
    setReview((prev) => (prev ? { ...prev, review_body: body } : prev));
    await fetch(`/api/reviews/${review.id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ review_body: body }),
    });
  }, [review?.id]);

  const handleUpdateEvent = useCallback(async (event: string) => {
    if (!review) return;
    setReview((prev) => (prev ? { ...prev, review_event: event } : prev));
    await fetch(`/api/reviews/${review.id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ review_event: event }),
    });
  }, [review?.id]);

  // Submit to GitHub
  const handleSubmit = useCallback(async () => {
    if (!review) return;
    setSubmitting(true);
    try {
      const res = await fetch(`/api/reviews/${review.id}/submit`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
      });
      if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error || "Submit failed");
      }
      onClose();
    } catch (err: any) {
      setError(err.message);
    } finally {
      setSubmitting(false);
    }
  }, [review?.id, onClose]);

  // Discard — just close for now (review stays in DB, can re-open)
  const handleDiscard = useCallback(() => {
    onClose();
  }, [onClose]);

  // Group comments by file path
  const commentsByFile = (review?.comments ?? []).reduce<Record<string, FileComment[]>>(
    (acc, c) => {
      (acc[c.path] ??= []).push({
        id: c.id,
        path: c.path,
        line: c.line,
        startLine: c.start_line,
        body: c.body,
      });
      return acc;
    },
    {},
  );

  // Close on Escape
  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open, onClose]);

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 z-50 bg-black/20 backdrop-blur-sm"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            className="fixed inset-6 z-50 flex flex-col bg-surface/95 backdrop-blur-2xl border border-border-glass rounded-3xl shadow-2xl shadow-black/[0.08] overflow-hidden"
            initial={{ opacity: 0, scale: 0.97, y: 12 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            exit={{ opacity: 0, scale: 0.97, y: 12 }}
            transition={{ type: "spring", damping: 30, stiffness: 350 }}
            onClick={(e) => e.stopPropagation()}
          >
            {/* Top bar */}
            <div className="shrink-0 flex items-center justify-between px-6 py-4 border-b border-border-subtle">
              <div className="flex items-center gap-3">
                <div className="w-2 h-2 rounded-full bg-snooze animate-pulse" />
                <h1 className="text-[15px] font-semibold text-text-primary tracking-tight">
                  Pending Review
                </h1>
                {review && (
                  <span className="text-[12px] text-text-tertiary">
                    {review.owner}/{review.repo} #{review.pr_number}
                  </span>
                )}
              </div>
              <button
                onClick={onClose}
                className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-2 py-1 rounded-lg hover:bg-black/[0.03]"
              >
                &times;
              </button>
            </div>

            {/* Content */}
            <div className="flex-1 overflow-y-auto">
              {loading ? (
                <div className="flex items-center justify-center h-64">
                  <div className="flex flex-col items-center gap-3">
                    <div className="w-5 h-5 border-2 border-accent/30 border-t-accent rounded-full animate-spin" />
                    <span className="text-[12px] text-text-tertiary">
                      Loading review...
                    </span>
                  </div>
                </div>
              ) : error ? (
                <div className="flex items-center justify-center h-64">
                  <div className="text-center">
                    <p className="text-[13px] text-dismiss">{error}</p>
                    <button
                      onClick={onClose}
                      className="text-[12px] text-text-tertiary hover:text-text-secondary mt-2 transition-colors"
                    >
                      Close
                    </button>
                  </div>
                </div>
              ) : review ? (
                <div className="p-6 space-y-4 max-w-5xl mx-auto">
                  {/* Review summary + actions */}
                  <ReviewSummary
                    owner={review.owner}
                    repo={review.repo}
                    prNumber={review.pr_number}
                    reviewEvent={review.review_event}
                    reviewBody={review.review_body}
                    commentCount={review.comments.length}
                    onUpdateBody={handleUpdateBody}
                    onUpdateEvent={handleUpdateEvent}
                    onSubmit={handleSubmit}
                    onDiscard={handleDiscard}
                    submitting={submitting}
                  />

                  {/* Diff files */}
                  <div className="space-y-3">
                    {files.map((file, i) => {
                      const path = file.newPath === "/dev/null" ? file.oldPath : file.newPath;
                      return (
                        <DiffFile
                          key={path + i}
                          file={file}
                          comments={commentsByFile[path] ?? []}
                          defaultCollapsed={files.length > 8}
                          onUpdateComment={handleUpdateComment}
                          onDeleteComment={handleDeleteComment}
                        />
                      );
                    })}
                  </div>

                  {files.length === 0 && (
                    <div className="text-center py-12">
                      <p className="text-[13px] text-text-tertiary">
                        No diff available
                      </p>
                    </div>
                  )}
                </div>
              ) : null}
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  );
}
