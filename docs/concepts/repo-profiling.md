# Repo profiling

Configured repos are automatically profiled on first run using Claude Haiku. The
profiler fetches README.md, CLAUDE.md, and AGENTS.md from each repo and generates
a summary used by the AI scorer and delegation agents.

Profiles are cached for 3 days. The **Re-profile** button on the Repos page
forces an immediate refresh regardless of TTL.
