# Repo profiling

Configured repos are automatically profiled on first run. The profiler fetches
README.md, CLAUDE.md, and AGENTS.md from each repo and generates a summary used
by the AI scorer and delegation agents.

Profiling runs on the org's **background jobs model** (Settings → Background jobs
model), the same setting the AI scorer uses. Without one picked, profiling does
not run — nothing falls back to a model of Triage Factory's choosing.

Profiles are cached for 3 days. The **Re-profile** button on the Repos page
forces an immediate refresh regardless of TTL.
