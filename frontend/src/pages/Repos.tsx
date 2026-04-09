import { useState, useEffect, useRef, useCallback } from "react";
import RepoPickerModal from "../components/RepoPickerModal";
import { useWebSocket } from "../hooks/useWebSocket";

interface RepoProfile {
  id: string;
  owner: string;
  repo: string;
  description?: string;
  has_readme: boolean;
  has_claude_md: boolean;
  has_agents_md: boolean;
  profile_text?: string;
  default_branch?: string;
  base_branch?: string;
  profiled_at?: string;
}

function BranchInput({
  profile,
  onSave,
}: {
  profile: RepoProfile;
  onSave: (branch: string) => void;
}) {
  const [value, setValue] = useState(profile.base_branch || "");
  const [open, setOpen] = useState(false);
  const [branches, setBranches] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );

  const placeholder = profile.default_branch || "main";

  const fetchBranches = useCallback(
    async (query: string) => {
      setLoading(true);
      try {
        const res = await fetch(
          `/api/repos/${profile.owner}/${profile.repo}/branches?q=${encodeURIComponent(query)}`,
        );
        if (res.ok) {
          const data: string[] = await res.json();
          setBranches(data);
        }
      } catch {
        // non-critical
      } finally {
        setLoading(false);
      }
    },
    [profile.owner, profile.repo],
  );

  const handleFocus = () => {
    setOpen(true);
    fetchBranches(value);
  };

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value;
    setValue(v);
    clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => fetchBranches(v), 200);
  };

  const handleSelect = (branch: string) => {
    setValue(branch);
    setOpen(false);
    onSave(branch);
  };

  const handleBlur = () => {
    // Delay to allow click on dropdown item
    setTimeout(() => {
      setOpen(false);
      const effective = value.trim();
      if (effective !== (profile.base_branch || "")) {
        onSave(effective);
      }
    }, 150);
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter") {
      setOpen(false);
      (e.target as HTMLInputElement).blur();
      const effective = value.trim();
      if (effective !== (profile.base_branch || "")) {
        onSave(effective);
      }
    }
    if (e.key === "Escape") {
      setOpen(false);
      setValue(profile.base_branch || "");
    }
  };

  // Close on outside click
  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node))
        setOpen(false);
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, []);

  return (
    <div ref={ref} className="relative">
      <input
        type="text"
        value={value}
        onChange={handleChange}
        onFocus={handleFocus}
        onBlur={handleBlur}
        onKeyDown={handleKeyDown}
        placeholder={placeholder}
        className="w-40 text-[12px] bg-transparent border border-border-subtle rounded-lg px-2.5 py-1.5 text-text-primary placeholder:text-text-tertiary/50 focus:outline-none focus:border-accent/40 transition-colors"
      />
      {open && (
        <div className="absolute z-10 top-full mt-1 w-56 max-h-48 overflow-y-auto backdrop-blur-xl bg-surface-raised border border-border-glass rounded-xl shadow-lg shadow-black/[0.08]">
          {loading && branches.length === 0 ? (
            <div className="px-3 py-2 text-[11px] text-text-tertiary">
              Loading...
            </div>
          ) : branches.length === 0 ? (
            <div className="px-3 py-2 text-[11px] text-text-tertiary">
              No branches found
            </div>
          ) : (
            branches.map((b) => (
              <button
                key={b}
                type="button"
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => handleSelect(b)}
                className={`w-full text-left px-3 py-1.5 text-[12px] hover:bg-accent/[0.06] transition-colors ${
                  b === placeholder
                    ? "text-text-secondary font-medium"
                    : "text-text-primary"
                }`}
              >
                {b}
                {b === profile.default_branch && (
                  <span className="ml-1.5 text-[10px] text-text-tertiary">
                    default
                  </span>
                )}
              </button>
            ))
          )}
        </div>
      )}
    </div>
  );
}

export default function Repos() {
  const [profiles, setProfiles] = useState<RepoProfile[]>([]);
  const [loading, setLoading] = useState(true);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [selectedRepos, setSelectedRepos] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{
    type: "success" | "error";
    text: string;
  } | null>(null);

  const fetchData = async () => {
    try {
      const res = await fetch("/api/repos");
      if (res.ok) {
        const data: RepoProfile[] = await res.json();
        setProfiles(data);
        setSelectedRepos(data.map((p) => p.id));
      }
    } catch {
      // non-critical
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchData();
  }, []);

  // Live updates from profiling pipeline
  useWebSocket((event) => {
    if (event.type === "repo_docs_updated") {
      const d = event.data as {
        id: string;
        has_readme: boolean;
        has_claude_md: boolean;
        has_agents_md: boolean;
      };
      setProfiles((prev) =>
        prev.map((p) =>
          p.id === d.id
            ? {
                ...p,
                has_readme: d.has_readme,
                has_claude_md: d.has_claude_md,
                has_agents_md: d.has_agents_md,
              }
            : p,
        ),
      );
    }
    if (event.type === "repo_profile_updated") {
      const d = event.data as { id: string; profile_text: string };
      setProfiles((prev) =>
        prev.map((p) =>
          p.id === d.id ? { ...p, profile_text: d.profile_text } : p,
        ),
      );
    }
  });

  const handleSaveRepos = async (repos: string[]) => {
    setSaving(true);
    setMessage(null);
    try {
      const res = await fetch("/api/repos", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ repos }),
      });
      if (!res.ok) {
        const data = await res.json();
        setMessage({ type: "error", text: data.error || "Failed to save" });
      } else {
        setMessage({
          type: "success",
          text: "Repositories updated. Profiling will run shortly.",
        });
        setSelectedRepos(repos);
        // Re-fetch profiles after a delay to catch profiling results
        setTimeout(fetchData, 5000);
      }
    } catch {
      setMessage({ type: "error", text: "Could not connect to server" });
    } finally {
      setSaving(false);
      setPickerOpen(false);
    }
  };

  const handleReprofile = async () => {
    setSaving(true);
    setMessage(null);
    try {
      // Saving the same repos triggers re-profiling via onGitHubChanged
      const res = await fetch("/api/repos", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ repos: selectedRepos }),
      });
      if (res.ok) {
        setMessage({ type: "success", text: "Re-profiling started." });
        setTimeout(fetchData, 8000);
      }
    } catch {
      setMessage({ type: "error", text: "Could not connect to server" });
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center min-h-[50vh]">
        <p className="text-text-tertiary text-[13px]">Loading repos...</p>
      </div>
    );
  }

  return (
    <div className="max-w-3xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Repositories
          </h1>
          <p className="text-[13px] text-text-tertiary mt-1">
            Watched repos appear in your triage queue and are used to match Jira
            tickets for delegation.
          </p>
        </div>
        <div className="flex gap-2">
          <button
            type="button"
            onClick={handleReprofile}
            disabled={saving || profiles.length === 0}
            className="text-[13px] text-text-secondary hover:text-text-primary border border-border-subtle rounded-xl px-4 py-2 transition-colors disabled:opacity-40"
          >
            {saving ? "Working..." : "Re-profile"}
          </button>
          <button
            type="button"
            onClick={() => setPickerOpen(true)}
            className="text-[13px] text-accent hover:text-accent/80 border border-accent/20 rounded-xl px-4 py-2 transition-colors"
          >
            Edit Selection
          </button>
        </div>
      </div>

      {message && (
        <div
          className={`rounded-xl px-4 py-2.5 text-[13px] mb-5 ${
            message.type === "success"
              ? "bg-claim/[0.08] border border-claim/20 text-claim"
              : "bg-dismiss/[0.08] border border-dismiss/20 text-dismiss"
          }`}
        >
          {message.text}
        </div>
      )}

      {profiles.length === 0 ? (
        <div className="backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-12 text-center">
          <p className="text-[13px] text-text-tertiary mb-4">
            No repositories configured yet.
          </p>
          <button
            type="button"
            onClick={() => setPickerOpen(true)}
            className="text-[13px] text-accent hover:text-accent/80 border border-accent/20 rounded-xl px-4 py-2 transition-colors"
          >
            Select Repositories
          </button>
        </div>
      ) : (
        <div className="space-y-3">
          {profiles.map((profile) => (
            <div
              key={profile.id}
              className="backdrop-blur-xl bg-surface-raised/70 border border-border-glass rounded-2xl p-5 shadow-sm shadow-black/[0.02]"
            >
              <div className="flex items-start justify-between gap-4">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2 mb-1">
                    <h3 className="text-[13px] font-semibold text-text-primary truncate">
                      {profile.id}
                    </h3>
                    <div className="flex gap-1">
                      {profile.has_readme && (
                        <span className="text-[9px] text-text-tertiary border border-border-subtle rounded px-1 py-0.5">
                          README
                        </span>
                      )}
                      {profile.has_claude_md && (
                        <span className="text-[9px] text-text-tertiary border border-border-subtle rounded px-1 py-0.5">
                          CLAUDE
                        </span>
                      )}
                      {profile.has_agents_md && (
                        <span className="text-[9px] text-text-tertiary border border-border-subtle rounded px-1 py-0.5">
                          AGENTS
                        </span>
                      )}
                    </div>
                  </div>

                  {profile.profile_text ? (
                    <p className="text-[12px] text-text-secondary leading-relaxed">
                      {profile.profile_text}
                    </p>
                  ) : profile.has_readme ||
                    profile.has_claude_md ||
                    profile.has_agents_md ? (
                    <div className="space-y-1.5 mt-1">
                      <div className="h-3 bg-black/[0.04] rounded-full w-full animate-pulse" />
                      <div className="h-3 bg-black/[0.04] rounded-full w-5/6 animate-pulse" />
                      <div className="h-3 bg-black/[0.04] rounded-full w-4/6 animate-pulse" />
                    </div>
                  ) : (
                    <p className="text-[12px] text-text-tertiary italic">
                      No documentation files found — profile cannot be
                      generated.
                    </p>
                  )}

                  <div className="flex items-center gap-2 mt-3">
                    <span className="text-[11px] text-text-tertiary">
                      Base branch:
                    </span>
                    <BranchInput
                      profile={profile}
                      onSave={async (branch) => {
                        try {
                          await fetch(
                            `/api/repos/${profile.owner}/${profile.repo}`,
                            {
                              method: "PATCH",
                              headers: { "Content-Type": "application/json" },
                              body: JSON.stringify({
                                base_branch: branch || null,
                              }),
                            },
                          );
                          setProfiles((prev) =>
                            prev.map((p) =>
                              p.id === profile.id
                                ? { ...p, base_branch: branch }
                                : p,
                            ),
                          );
                        } catch {
                          // non-critical
                        }
                      }}
                    />
                  </div>
                </div>

                {profile.profiled_at && (
                  <span className="text-[10px] text-text-tertiary shrink-0 whitespace-nowrap">
                    {new Date(profile.profiled_at).toLocaleDateString()}
                  </span>
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      {pickerOpen && (
        <RepoPickerModal
          selected={selectedRepos}
          onSave={handleSaveRepos}
          onClose={() => setPickerOpen(false)}
        />
      )}
    </div>
  );
}
