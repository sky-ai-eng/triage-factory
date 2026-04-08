import { useState } from "react";
import Markdown from "react-markdown";

interface Props {
  owner: string;
  repo: string;
  prNumber: number;
  reviewEvent: string;
  reviewBody: string;
  commentCount: number;
  onUpdateBody: (body: string) => void;
  onUpdateEvent: (event: string) => void;
  onSubmit: () => void;
  onDiscard: () => void;
  submitting: boolean;
}

const EVENT_OPTIONS = [
  {
    value: "APPROVE",
    label: "Approve",
    color: "text-claim",
    bg: "bg-claim/10",
    icon: (
      <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
        <polyline points="3 7.5 5.5 10 11 4" />
      </svg>
    ),
  },
  {
    value: "COMMENT",
    label: "Comment",
    color: "text-text-secondary",
    bg: "bg-black/[0.04]",
    icon: (
      <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
        <path d="M2 2.5h10a1 1 0 011 1v6a1 1 0 01-1 1H4l-2.5 2V3.5a1 1 0 011-1z" />
      </svg>
    ),
  },
  {
    value: "REQUEST_CHANGES",
    label: "Request Changes",
    color: "text-dismiss",
    bg: "bg-dismiss/10",
    icon: (
      <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
        <circle cx="7" cy="7" r="5.5" />
        <line x1="5" y1="5" x2="9" y2="9" />
        <line x1="9" y1="5" x2="5" y2="9" />
      </svg>
    ),
  },
];

export default function ReviewSummary({
  owner,
  repo,
  prNumber,
  reviewEvent,
  reviewBody,
  commentCount,
  onUpdateBody,
  onUpdateEvent,
  onSubmit,
  onDiscard,
  submitting,
}: Props) {
  const [editingBody, setEditingBody] = useState(false);
  const [rawView, setRawView] = useState(false);
  const [draft, setDraft] = useState(reviewBody);

  const saveBody = () => {
    onUpdateBody(draft);
    setEditingBody(false);
  };

  const cancelEdit = () => {
    setDraft(reviewBody);
    setEditingBody(false);
  };

  const currentEvent = EVENT_OPTIONS.find((e) => e.value === reviewEvent) ?? EVENT_OPTIONS[1];

  return (
    <div className="backdrop-blur-xl bg-surface-raised/70 border border-border-glass rounded-2xl shadow-sm shadow-black/[0.02] overflow-hidden">
      {/* Header */}
      <div className="px-5 pt-5 pb-4">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h2 className="text-[15px] font-semibold text-text-primary tracking-tight">
              Review Preview
            </h2>
            <p className="text-[12px] text-text-tertiary mt-0.5">
              {owner}/{repo} #{prNumber}
            </p>
          </div>

          {/* Event type selector */}
          <div className="flex items-center gap-1.5 shrink-0">
            {EVENT_OPTIONS.map((opt) => (
              <button
                key={opt.value}
                onClick={() => onUpdateEvent(opt.value)}
                className={`flex items-center gap-1.5 text-[11px] font-medium px-2.5 py-1.5 rounded-lg border transition-all duration-150 ${
                  reviewEvent === opt.value
                    ? `${opt.color} ${opt.bg} border-current/20`
                    : "text-text-tertiary border-transparent hover:bg-black/[0.03]"
                }`}
              >
                {opt.icon}
                {opt.label}
              </button>
            ))}
          </div>
        </div>
      </div>

      {/* Review body */}
      <div className="px-5 pb-4">
        {editingBody ? (
          <div className="space-y-2">
            <textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              className="w-full min-h-[120px] text-[13px] leading-relaxed text-text-primary bg-white/40 border border-border-subtle rounded-xl px-4 py-3 resize-y focus:outline-none focus:border-accent/30 focus:ring-1 focus:ring-accent/10 font-mono"
              placeholder="Review summary..."
              autoFocus
            />
            <div className="flex items-center gap-2 justify-end">
              <button
                onClick={cancelEdit}
                className="text-[11px] text-text-tertiary hover:text-text-secondary px-3 py-1.5 rounded-lg transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={saveBody}
                className="text-[11px] font-medium text-white bg-accent hover:bg-accent/90 px-3 py-1.5 rounded-lg transition-colors"
              >
                Save
              </button>
            </div>
          </div>
        ) : (
          <div className="relative group">
            {/* View toggle + edit button */}
            <div className="absolute top-2 right-2 flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity z-10">
              <button
                onClick={() => setRawView(!rawView)}
                className="text-[10px] text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded bg-white/60 border border-border-subtle transition-colors"
              >
                {rawView ? "Preview" : "Raw"}
              </button>
              <button
                onClick={() => { setDraft(reviewBody); setEditingBody(true); }}
                className="text-[10px] text-text-tertiary hover:text-accent px-1.5 py-0.5 rounded bg-white/60 border border-border-subtle transition-colors"
              >
                Edit
              </button>
            </div>

            <div className="bg-white/30 rounded-xl px-4 py-3 border border-transparent hover:border-border-subtle transition-colors min-h-[48px]">
              {!reviewBody ? (
                <span
                  onClick={() => { setDraft(reviewBody); setEditingBody(true); }}
                  className="text-[13px] text-text-tertiary italic cursor-text"
                >
                  No summary provided
                </span>
              ) : rawView ? (
                <pre className="text-[12.5px] leading-relaxed text-text-secondary font-mono whitespace-pre-wrap">
                  {reviewBody}
                </pre>
              ) : (
                <div className="review-markdown text-[13px] leading-relaxed text-text-secondary">
                  <Markdown>{reviewBody}</Markdown>
                </div>
              )}
            </div>
          </div>
        )}
      </div>

      {/* Footer actions */}
      <div className="px-5 py-3 border-t border-border-subtle flex items-center justify-between">
        <span className="text-[11px] text-text-tertiary">
          {commentCount} inline comment{commentCount !== 1 ? "s" : ""}
        </span>

        <div className="flex items-center gap-2">
          <button
            onClick={onDiscard}
            className="text-[11px] font-medium text-text-tertiary hover:text-dismiss px-3 py-1.5 rounded-lg transition-colors"
          >
            Discard
          </button>
          <button
            onClick={onSubmit}
            disabled={submitting}
            className={`flex items-center gap-1.5 text-[12px] font-semibold px-4 py-2 rounded-xl transition-all duration-150 ${
              submitting
                ? "bg-accent/50 text-white/70 cursor-not-allowed"
                : `text-white ${
                    reviewEvent === "APPROVE"
                      ? "bg-claim hover:bg-claim/90"
                      : reviewEvent === "REQUEST_CHANGES"
                        ? "bg-dismiss hover:bg-dismiss/90"
                        : "bg-accent hover:bg-accent/90"
                  }`
            }`}
          >
            {submitting ? (
              <>
                <span className="inline-block w-3 h-3 border border-white/40 border-t-white rounded-full animate-spin" />
                Submitting...
              </>
            ) : (
              <>
                {currentEvent.icon}
                Submit to GitHub
              </>
            )}
          </button>
        </div>
      </div>
    </div>
  );
}
