# Project Bundle Format

Portable project bundles enable local-first team handoff.

- Extension: `.tfproject`
- Container: ZIP archive
- Manifest: `manifest.yaml` with `format_version`

## Compatibility Contract

Bundles are user-portable artifacts. Once a format version ships, importers must
continue to support it. Breaking compatibility requires either:

- a new `format_version` with an explicit migration path, or
- importer-side backward compatibility logic.

`format_version: 1` is the initial public format.

## v1 Layout

```text
manifest.yaml
knowledge-base/<files>
session/
  transcript.jsonl
  subagents/
    agent-<hash>.jsonl
    agent-<hash>.meta.json
  tool-results/
    toolu_<id>.txt
curator/requests.jsonl
curator/messages.jsonl
curator/pending_context.jsonl
```

Notes:

- `knowledge-base/` is copied verbatim from `~/.triagefactory/projects/<id>/knowledge-base/`.
- `session/` is omitted when the source project has no resolvable Curator
  transcript at export time.
- Curator table dumps are JSON Lines (`.jsonl`) for append-only readability and
  easy recovery/debugging.

## Manifest (`manifest.yaml`)

```yaml
format_version: 1
exported_at: 2026-05-03T18:42:00Z
project:
  name: ...
  description: ...
  pinned_repos: [owner/repo, ...]
  jira_project_key: SKY
  linear_project_key: ...
session:
  curator_session_id: <source-session-id>
  resolved_cwd: <source-resolved-project-cwd>
```

## Import Rewrite Rules

Import creates a new project id and (when session payload exists) a new
`curator_session_id`. Session artifacts are rewritten with byte-level substring
replacement:

- `oldSessionID -> newSessionID`
- `oldResolvedCwd -> newResolvedCwd`

Rewrites are applied to:

- `session/transcript.jsonl`
- `session/subagents/*`
- `session/tool-results/*`

This preserves resume fidelity across machines while avoiding JSON schema
coupling.

## Deliberately Excluded from v1

The bundle does **not** include runtime-local Claude Code state:

- `~/.claude/sessions/<pid>.json`
- `~/.claude/session-env/<sessionId>/`
- `~/.claude/todos/<sessionId>-agent-*.json`

Those are process/runtime caches and are regenerated or optional for resume
flows.

