package projectbundle

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// GitHubProbe provides the preflight clone URL lookup used by import.
type GitHubProbe interface {
	CloneURLForRepo(ctx context.Context, owner, repo string) (string, error)
}

const (
	maxImportJSONLEntryBytes    int64 = 64 << 20  // 64 MiB per curator JSONL payload.
	maxImportJSONLRows                = 200_000   // Upper bound per curator JSONL file.
	maxImportExtractEntryBytes  int64 = 512 << 20 // 512 MiB per extracted file.
	maxImportExtractBundleBytes int64 = 2 << 30   // 2 GiB aggregate extracted payload.
)

type zipExtractionBudget struct {
	remaining    int64
	totalLimit   int64
	perFileLimit int64
}

func newZipExtractionBudget(totalLimit, perFileLimit int64) *zipExtractionBudget {
	return &zipExtractionBudget{
		remaining:    totalLimit,
		totalLimit:   totalLimit,
		perFileLimit: perFileLimit,
	}
}

func (b *zipExtractionBudget) reserve(zf *zip.File) (int64, error) {
	declared, err := zipEntryDeclaredSize(zf, b.perFileLimit)
	if err != nil {
		return 0, err
	}
	if declared > b.remaining {
		return 0, fmt.Errorf(
			"bundle extraction exceeds %d-byte total limit (next entry %s is %d bytes, %d bytes remain)",
			b.totalLimit,
			zf.Name,
			declared,
			b.remaining,
		)
	}
	b.remaining -= declared
	return declared, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// Import reads a .tfproject ZIP and materializes it into a new project
// owned by orgID + teamID, with userID stamped as the creator. The
// caller resolves all three from the request context (orgID + userID
// from JWT claims, teamID from TeamsStore.GetDefaultForOrgSystem).
//
// SQLite-only today — the body opens its own *sql.Tx and writes via
// raw `?`-placeholder INSERTs. handleProjectImport mode-gates the
// endpoint to local-mode for that reason; porting to per-dialect store
// methods is a follow-up.
func Import(
	ctx context.Context,
	database *sql.DB,
	projects db.ProjectStore,
	orgID, teamID, userID string,
	readerAt io.ReaderAt,
	size int64,
	probe GitHubProbe,
) (*domain.Project, []ImportWarning, error) {
	if size <= 0 {
		return nil, nil, errors.New("bundle is empty")
	}
	zr, err := zip.NewReader(readerAt, size)
	if err != nil {
		return nil, nil, fmt.Errorf("open bundle zip: %w", err)
	}
	entries, err := indexZipEntries(zr.File)
	if err != nil {
		return nil, nil, err
	}

	manifest, err := readManifest(entries)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureUniqueProjectName(ctx, projects, orgID, manifest.Project.Name); err != nil {
		return nil, nil, err
	}
	cloneURLs, err := preflightPinnedRepos(ctx, manifest.Project.PinnedRepos, probe)
	if err != nil {
		return nil, nil, err
	}

	sessionEntries, err := listEntriesWithPrefix(entries, sessionPrefix)
	if err != nil {
		return nil, nil, err
	}
	hasSession := len(sessionEntries) > 0
	if hasSession {
		if _, ok := entries[sessionTranscriptPath]; !ok {
			return nil, nil, fmt.Errorf("bundle session is missing %s", sessionTranscriptPath)
		}
		if manifest.Session == nil {
			return nil, nil, errors.New("bundle session exists but manifest.session is missing")
		}
		if strings.TrimSpace(manifest.Session.CuratorSessionID) == "" || strings.TrimSpace(manifest.Session.ResolvedCwd) == "" {
			return nil, nil, errors.New("manifest.session requires curator_session_id and resolved_cwd")
		}
	}

	newProjectID := uuid.New().String()
	newSessionID := ""
	if hasSession {
		newSessionID = uuid.New().String()
	}
	projectRoot, err := curator.KnowledgeDir(newProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve project root: %w", err)
	}
	kbRoot := filepath.Join(projectRoot, "knowledge-base")
	extractionBudget := newZipExtractionBudget(maxImportExtractBundleBytes, maxImportExtractEntryBytes)

	cleanup := &rollbackTracker{}
	committed := false
	defer func() {
		if !committed {
			cleanup.Cleanup()
		}
	}()

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin import transaction: %w", err)
	}
	defer tx.Rollback()

	if err := insertImportedProject(tx, newProjectID, newSessionID, orgID, teamID, userID, manifest.Project); err != nil {
		return nil, nil, err
	}

	requestIDMap, err := importCuratorRequests(tx, newProjectID, entries[curatorRequestsPath])
	if err != nil {
		return nil, nil, err
	}
	if err := importCuratorMessages(tx, requestIDMap, entries[curatorMessagesPath]); err != nil {
		return nil, nil, err
	}
	if err := importPendingContext(tx, newProjectID, newSessionID, requestIDMap, entries[curatorPendingContextPath]); err != nil {
		return nil, nil, err
	}
	if err := ensureRepoProfiles(tx, teamID, manifest.Project.PinnedRepos, cloneURLs); err != nil {
		return nil, nil, err
	}

	cleanup.Add(projectRoot)
	if err := os.MkdirAll(kbRoot, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir knowledge root: %w", err)
	}
	if err := materializeKnowledge(entries, kbRoot, extractionBudget); err != nil {
		return nil, nil, err
	}

	if hasSession {
		if err := materializeSession(entries, manifest.Session, projectRoot, newSessionID, extractionBudget, cleanup); err != nil {
			return nil, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit import transaction: %w", err)
	}
	committed = true

	warnings := clonePinnedRepos(ctx, manifest.Project.PinnedRepos, cloneURLs)
	project, err := projects.Get(ctx, orgID, newProjectID)
	if err != nil {
		return nil, warnings, fmt.Errorf("load imported project: %w", err)
	}
	if project == nil {
		return nil, warnings, errors.New("import committed but project row is missing")
	}
	return project, warnings, nil
}

type rollbackTracker struct {
	paths map[string]struct{}
}

func (r *rollbackTracker) Add(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if r.paths == nil {
		r.paths = make(map[string]struct{})
	}
	r.paths[path] = struct{}{}
}

func (r *rollbackTracker) Cleanup() {
	if len(r.paths) == 0 {
		return
	}
	ordered := make([]string, 0, len(r.paths))
	for p := range r.paths {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, p := range ordered {
		_ = os.RemoveAll(p)
	}
}

func readManifest(entries map[string]*zip.File) (*Manifest, error) {
	zf, ok := entries[manifestPath]
	if !ok {
		return nil, ErrManifestMissing
	}
	body, err := readZipFileLimited(zf, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	return decodeManifest(body)
}

func readZipFileLimited(zf *zip.File, maxBytes int64) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", zf.Name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", zf.Name, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", zf.Name, maxBytes)
	}
	return body, nil
}

func ensureUniqueProjectName(ctx context.Context, projects db.ProjectStore, orgID, incoming string) error {
	incoming = strings.TrimSpace(incoming)
	rows, err := projects.List(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	for _, p := range rows {
		if strings.EqualFold(strings.TrimSpace(p.Name), incoming) {
			return &DuplicateNameError{Name: incoming}
		}
	}
	return nil
}

func preflightPinnedRepos(ctx context.Context, pinned []string, probe GitHubProbe) (map[string]string, error) {
	cloneURLs := make(map[string]string, len(pinned))
	missing := make([]MissingRepoError, 0)
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			return nil, fmt.Errorf("invalid pinned repo slug %q", slug)
		}
		if probe == nil {
			missing = append(missing, MissingRepoError{
				Repo:  slug,
				Error: "GitHub is not configured",
			})
			continue
		}
		cloneURL, err := probe.CloneURLForRepo(ctx, owner, repo)
		if err != nil {
			missing = append(missing, MissingRepoError{
				Repo:  slug,
				Error: err.Error(),
			})
			continue
		}
		if strings.TrimSpace(cloneURL) == "" {
			missing = append(missing, MissingRepoError{
				Repo:  slug,
				Error: "missing clone_url",
			})
			continue
		}
		cloneURLs[slug] = strings.TrimSpace(cloneURL)
	}
	if len(missing) > 0 {
		return nil, &MissingReposError{Missing: missing}
	}
	return cloneURLs, nil
}

// insertImportedProject inserts a row into projects from a bundle
// manifest. org_id, team_id, and creator_user_id are bound from the
// request handler's resolved identity (orgID + userID from JWT claims;
// teamID from TeamsStore.GetDefaultForOrgSystem) rather than left to
// SQLite column defaults — that way `projects.List(ctx, orgID)` and
// `Import(..., orgID, ...)` agree on the row's owning tenant even if
// the caller ever passes a non-default value. SQLite-only SQL today;
// see Import's docstring for the porting note.
func insertImportedProject(tx *sql.Tx, projectID, curatorSessionID, orgID, teamID, userID string, manifestProject ManifestProject) error {
	pinned := cloneStrings(manifestProject.PinnedRepos)
	if pinned == nil {
		pinned = []string{}
	}
	pinnedJSON, err := json.Marshal(pinned)
	if err != nil {
		return fmt.Errorf("marshal pinned repos: %w", err)
	}
	now := time.Now().UTC()
	_, err = tx.Exec(`
		INSERT INTO projects (
			id, name, description,
			curator_session_id, pinned_repos, jira_project_key,
			linear_project_key, org_id, team_id, creator_user_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		projectID,
		strings.TrimSpace(manifestProject.Name),
		manifestProject.Description,
		nullIfEmptyString(curatorSessionID),
		string(pinnedJSON),
		nullIfEmptyString(manifestProject.JiraProjectKey),
		nullIfEmptyString(manifestProject.LinearProjectKey),
		orgID,
		teamID,
		userID,
		now,
		now,
	)
	if err != nil {
		return fmt.Errorf("insert imported project: %w", err)
	}
	return nil
}

func importCuratorRequests(tx *sql.Tx, projectID string, zf *zip.File) (map[string]string, error) {
	idMap := make(map[string]string)
	err := decodeZipJSONLines(
		zf,
		maxImportJSONLEntryBytes,
		maxImportJSONLRows,
		func(row domain.CuratorRequest) error {
			oldID := strings.TrimSpace(row.ID)
			if oldID == "" {
				return nil
			}
			newID := idMap[oldID]
			if newID == "" {
				newID = uuid.New().String()
				idMap[oldID] = newID
			}
			_, err := tx.Exec(`
			INSERT INTO curator_requests (
				id, project_id, status, user_input, error_msg,
				cost_usd, duration_ms, num_turns, started_at,
				finished_at, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
				newID,
				projectID,
				row.Status,
				row.UserInput,
				nullIfEmptyString(row.ErrorMsg),
				row.CostUSD,
				row.DurationMs,
				row.NumTurns,
				nullIfNilTime(row.StartedAt),
				nullIfNilTime(row.FinishedAt),
				row.CreatedAt,
			)
			if err != nil {
				return fmt.Errorf("insert curator_request %s: %w", oldID, err)
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", curatorRequestsPath, err)
	}
	return idMap, nil
}

func importCuratorMessages(tx *sql.Tx, requestIDMap map[string]string, zf *zip.File) error {
	err := decodeZipJSONLines(
		zf,
		maxImportJSONLEntryBytes,
		maxImportJSONLRows,
		func(row domain.CuratorMessage) error {
			requestID := requestIDMap[row.RequestID]
			if requestID == "" {
				return fmt.Errorf("curator message references unknown request_id %q", row.RequestID)
			}
			toolCallsJSON, err := marshalNullableJSON(row.ToolCalls)
			if err != nil {
				return fmt.Errorf("marshal tool_calls for request %s: %w", row.RequestID, err)
			}
			metadataJSON, err := marshalNullableJSON(row.Metadata)
			if err != nil {
				return fmt.Errorf("marshal metadata for request %s: %w", row.RequestID, err)
			}
			_, err = tx.Exec(`
			INSERT INTO curator_messages (
				request_id, role, subtype, content, tool_calls, tool_call_id,
				is_error, metadata, model, input_tokens, output_tokens,
				cache_read_tokens, cache_creation_tokens, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
				requestID,
				row.Role,
				row.Subtype,
				row.Content,
				toolCallsJSON,
				nullIfEmptyString(row.ToolCallID),
				row.IsError,
				metadataJSON,
				nullIfEmptyString(row.Model),
				nullIfNilInt(row.InputTokens),
				nullIfNilInt(row.OutputTokens),
				nullIfNilInt(row.CacheReadTokens),
				nullIfNilInt(row.CacheCreationTokens),
				row.CreatedAt,
			)
			if err != nil {
				return fmt.Errorf("insert curator_message for request %s: %w", row.RequestID, err)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("decode %s: %w", curatorMessagesPath, err)
	}
	return nil
}

func importPendingContext(
	tx *sql.Tx,
	projectID string,
	newSessionID string,
	requestIDMap map[string]string,
	zf *zip.File,
) error {
	err := decodeZipJSONLines(
		zf,
		maxImportJSONLEntryBytes,
		maxImportJSONLRows,
		func(row domain.CuratorPendingContext) error {
			if strings.TrimSpace(newSessionID) == "" {
				return errors.New("bundle has pending context rows but no session payload")
			}
			var consumedBy any
			if row.ConsumedByRequestID != "" {
				mapped := requestIDMap[row.ConsumedByRequestID]
				if mapped == "" {
					return fmt.Errorf("pending context references unknown consumed_by_request_id %q", row.ConsumedByRequestID)
				}
				consumedBy = mapped
			}
			_, err := tx.Exec(`
			INSERT INTO curator_pending_context (
				project_id, curator_session_id, change_type, baseline_value,
				consumed_at, consumed_by_request_id, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)
		`,
				projectID,
				newSessionID,
				row.ChangeType,
				row.BaselineValue,
				nullIfNilTime(row.ConsumedAt),
				consumedBy,
				row.CreatedAt,
			)
			if err != nil {
				return fmt.Errorf("insert pending context row: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("decode %s: %w", curatorPendingContextPath, err)
	}
	return nil
}

// ensureRepoProfiles materializes the imported project's pinned repos.
// It writes two rows per repo:
//
//   - team_github_repos — the per-team tracking selection that is the
//     source of truth. Without this the router team↔repo gate
//     (TracksRepoSystem) drops the importing team's handlers for the
//     repo, so polled events from it never create tasks until the user
//     re-saves the repo selection. repo_profiles is a *derived cache* of
//     this table, so the tracking row must exist for the import to behave
//     like a normal repo-selection save.
//   - repo_profiles — the org-wide polled set (the cache), pre-seeded
//     here with the clone_url discovered during preflight so the import's
//     clone step + first poll don't have to re-resolve it.
//
// Import is SQLite-only (N=1, single default team), so teamID is the org
// default team and no cross-team union reconcile is needed — the union is
// trivially this team's rows.
func ensureRepoProfiles(tx *sql.Tx, teamID string, pinned []string, cloneURLs map[string]string) error {
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			return fmt.Errorf("invalid pinned repo slug %q", slug)
		}
		// Track the repo for the importing team (the gate's source of
		// truth). All pinned repos passed preflight, so they're all real.
		if _, err := tx.Exec(`
			INSERT INTO team_github_repos (team_id, owner, repo)
			VALUES (?, ?, ?)
			ON CONFLICT(team_id, owner, repo) DO NOTHING
		`, teamID, owner, repo); err != nil {
			return fmt.Errorf("track imported repo %s: %w", slug, err)
		}
		cloneURL := cloneURLs[slug]
		if cloneURL == "" {
			continue
		}
		_, err := tx.Exec(`
			INSERT INTO repo_profiles (id, owner, repo, clone_url, updated_at)
			VALUES (?, ?, ?, ?, datetime('now'))
			ON CONFLICT(id) DO UPDATE SET
				clone_url = CASE
					WHEN repo_profiles.clone_url IS NULL OR repo_profiles.clone_url = ''
					THEN excluded.clone_url
					ELSE repo_profiles.clone_url
				END,
				updated_at = datetime('now')
		`, slug, owner, repo, cloneURL)
		if err != nil {
			return fmt.Errorf("upsert repo profile %s: %w", slug, err)
		}
	}
	return nil
}

func materializeKnowledge(entries map[string]*zip.File, kbRoot string, extractionBudget *zipExtractionBudget) error {
	kbEntries, err := listEntriesWithPrefix(entries, knowledgePrefix)
	if err != nil {
		return err
	}
	for _, e := range kbEntries {
		rel, err := safeBundleRel(e.Name, knowledgePrefix)
		if err != nil {
			return err
		}
		dest := filepath.Join(kbRoot, filepath.FromSlash(rel))
		if err := ensureUnderRoot(kbRoot, dest); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("mkdir knowledge parent for %s: %w", dest, err)
		}
		if err := copyZipEntryRaw(e.File, dest, 0o644, extractionBudget); err != nil {
			return err
		}
	}
	return nil
}

func materializeSession(
	entries map[string]*zip.File,
	manifestSession *ManifestSession,
	projectRoot string,
	newSessionID string,
	extractionBudget *zipExtractionBudget,
	cleanup *rollbackTracker,
) error {
	newResolvedCwd := worktree.ResolveClaudeProjectCwd(projectRoot)
	newEncoded := worktree.EncodeClaudeProjectDir(newResolvedCwd)
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir for session import: %w", err)
	}
	encodedRoot := filepath.Join(home, ".claude", "projects", newEncoded)
	if err := os.MkdirAll(encodedRoot, 0o700); err != nil {
		return fmt.Errorf("mkdir claude project root: %w", err)
	}

	sessionTreeRoot := filepath.Join(encodedRoot, newSessionID)
	transcriptDest := filepath.Join(encodedRoot, newSessionID+".jsonl")
	cleanup.Add(sessionTreeRoot)
	cleanup.Add(transcriptDest)

	reps := buildSessionReplacements(
		manifestSession.CuratorSessionID,
		newSessionID,
		manifestSession.ResolvedCwd,
		newResolvedCwd,
	)

	transcript, ok := entries[sessionTranscriptPath]
	if !ok {
		return fmt.Errorf("session is missing %s", sessionTranscriptPath)
	}
	if err := copyZipEntryRewritten(transcript, transcriptDest, reps, 0o600, extractionBudget); err != nil {
		return err
	}

	subagentEntries, err := listEntriesWithPrefix(entries, sessionSubagentsPrefix)
	if err != nil {
		return err
	}
	for _, e := range subagentEntries {
		rel, err := safeBundleRel(e.Name, sessionSubagentsPrefix)
		if err != nil {
			return err
		}
		dest := filepath.Join(sessionTreeRoot, "subagents", filepath.FromSlash(rel))
		if err := ensureUnderRoot(filepath.Join(sessionTreeRoot, "subagents"), dest); err != nil {
			return err
		}
		if err := copyZipEntryRewritten(e.File, dest, reps, 0o600, extractionBudget); err != nil {
			return err
		}
	}

	toolEntries, err := listEntriesWithPrefix(entries, sessionToolResultsPrefix)
	if err != nil {
		return err
	}
	for _, e := range toolEntries {
		rel, err := safeBundleRel(e.Name, sessionToolResultsPrefix)
		if err != nil {
			return err
		}
		dest := filepath.Join(sessionTreeRoot, "tool-results", filepath.FromSlash(rel))
		if err := ensureUnderRoot(filepath.Join(sessionTreeRoot, "tool-results"), dest); err != nil {
			return err
		}
		if err := copyZipEntryRewritten(e.File, dest, reps, 0o600, extractionBudget); err != nil {
			return err
		}
	}
	return nil
}

func clonePinnedRepos(ctx context.Context, pinned []string, cloneURLs map[string]string) []ImportWarning {
	warnings := make([]ImportWarning, 0)
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			warnings = append(warnings, ImportWarning{
				Code:    "invalid_repo_slug",
				Repo:    slug,
				Message: "invalid owner/repo slug",
			})
			continue
		}
		cloneURL := cloneURLs[slug]
		if cloneURL == "" {
			continue
		}
		if _, err := worktree.EnsureBareClone(ctx, owner, repo, cloneURL); err != nil {
			warnings = append(warnings, ImportWarning{
				Code:    "clone_failed",
				Repo:    slug,
				Message: err.Error(),
			})
		}
	}
	return warnings
}

type namedZipFile struct {
	Name string
	File *zip.File
}

func indexZipEntries(files []*zip.File) (map[string]*zip.File, error) {
	out := make(map[string]*zip.File, len(files))
	for _, zf := range files {
		if zf.FileInfo().IsDir() {
			continue
		}
		name := strings.TrimPrefix(zf.Name, "/")
		if strings.Contains(name, "\\") {
			return nil, fmt.Errorf("invalid bundle path %q", zf.Name)
		}
		clean := path.Clean(name)
		if clean == "." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("invalid bundle path %q", zf.Name)
		}
		if _, exists := out[clean]; exists {
			return nil, fmt.Errorf("duplicate bundle path %q", clean)
		}
		out[clean] = zf
	}
	return out, nil
}

func listEntriesWithPrefix(entries map[string]*zip.File, prefix string) ([]namedZipFile, error) {
	out := make([]namedZipFile, 0)
	for name, zf := range entries {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, err := safeBundleRel(name, prefix); err != nil {
			return nil, err
		}
		out = append(out, namedZipFile{Name: name, File: zf})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func safeBundleRel(name, prefix string) (string, error) {
	if !strings.HasPrefix(name, prefix) {
		return "", fmt.Errorf("bundle path %q does not start with %q", name, prefix)
	}
	rel := strings.TrimPrefix(name, prefix)
	rel = path.Clean(rel)
	if rel == "." || rel == "" {
		return "", fmt.Errorf("bundle path %q has no relative component", name)
	}
	if strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("bundle path %q escapes its prefix", name)
	}
	return rel, nil
}

func ensureUnderRoot(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved path %q escapes root %q", target, root)
	}
	return nil
}

func copyZipEntryRaw(zf *zip.File, dest string, mode os.FileMode, extractionBudget *zipExtractionBudget) error {
	declared, err := extractionBudget.reserve(zf)
	if err != nil {
		return err
	}
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("open bundle entry %s: %w", zf.Name, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir parent for %s: %w", dest, err)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	reader := &countingReader{r: io.LimitReader(rc, declared+1)}
	if _, err := io.Copy(out, reader); err != nil {
		return fmt.Errorf("copy %s to %s: %w", zf.Name, dest, err)
	}
	if err := verifyZipEntryBytes(zf.Name, reader.n, declared); err != nil {
		return err
	}
	return nil
}

func copyZipEntryRewritten(
	zf *zip.File,
	dest string,
	reps []byteReplacement,
	mode os.FileMode,
	extractionBudget *zipExtractionBudget,
) error {
	declared, err := extractionBudget.reserve(zf)
	if err != nil {
		return err
	}
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("open bundle entry %s: %w", zf.Name, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("mkdir parent for %s: %w", dest, err)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()
	reader := &countingReader{r: io.LimitReader(rc, declared+1)}
	if err := rewriteToFile(out, reader, reps); err != nil {
		return fmt.Errorf("rewrite %s to %s: %w", zf.Name, dest, err)
	}
	if err := verifyZipEntryBytes(zf.Name, reader.n, declared); err != nil {
		return err
	}
	return nil
}

func decodeZipJSONLines[T any](
	zf *zip.File,
	maxBytes int64,
	maxRows int,
	onRow func(T) error,
) error {
	if zf == nil {
		return nil
	}
	declared, err := zipEntryDeclaredSize(zf, maxBytes)
	if err != nil {
		return err
	}
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	reader := &countingReader{r: io.LimitReader(rc, declared+1)}
	dec := json.NewDecoder(reader)
	rows := 0
	for {
		var item T
		if err := dec.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		rows++
		if maxRows > 0 && rows > maxRows {
			return fmt.Errorf("%s exceeds %d-row limit", zf.Name, maxRows)
		}
		if onRow != nil {
			if err := onRow(item); err != nil {
				return err
			}
		}
	}
	return verifyZipEntryBytes(zf.Name, reader.n, declared)
}

func zipEntryDeclaredSize(zf *zip.File, maxBytes int64) (int64, error) {
	declared := zf.UncompressedSize64
	if maxBytes > 0 && declared > uint64(maxBytes) {
		return 0, fmt.Errorf("%s exceeds %d-byte limit", zf.Name, maxBytes)
	}
	if declared > uint64(math.MaxInt64-1) {
		return 0, fmt.Errorf("%s declared uncompressed size is too large", zf.Name)
	}
	return int64(declared), nil
}

func verifyZipEntryBytes(name string, readBytes, declared int64) error {
	switch {
	case readBytes > declared:
		return fmt.Errorf(
			"%s exceeded its declared uncompressed size (%d > %d bytes)",
			name,
			readBytes,
			declared,
		)
	case readBytes < declared:
		return fmt.Errorf(
			"%s ended before its declared uncompressed size (%d < %d bytes)",
			name,
			readBytes,
			declared,
		)
	default:
		return nil
	}
}

func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner = strings.TrimSpace(parts[0])
	repo = strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

func marshalNullableJSON(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []domain.ToolCall:
		if len(t) == 0 {
			return nil, nil
		}
	case map[string]any:
		if len(t) == 0 {
			return nil, nil
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func nullIfEmptyString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func nullIfNilInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullIfNilTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}
