package projectbundle

import (
	"archive/zip"
	"context"
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

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
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
// from JWT claims, teamID via teamscope.ResolveActing).
//
// Ordering: files are extracted to their final org-scoped locations
// FIRST (they're invisible until the project row commits, and the
// rollbackTracker removes them on any failure), then every DB write
// runs inside ONE claims-bound WithTx — so Postgres RLS gates the
// inserts under the importing user's identity, and the tx never holds
// a claims-bound pool connection through multi-GiB zip extraction.
func Import(
	ctx context.Context,
	txr db.TxRunner,
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
	// Cheap duplicate-name preflight before paying extraction. The
	// authoritative check re-runs inside the write tx below — the name
	// could be taken between this read and the commit.
	if err := txr.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		return ensureUniqueProjectName(ctx, tx.Projects, orgID, manifest.Project.Name)
	}); err != nil {
		return nil, nil, err
	}
	// Preflight every pinned repo through the importing org's OWN
	// GitHub client (probe wraps resolver.ClientForRepo(orgID, ...)).
	// This is a tenant-isolation invariant, not just UX: the bare-clone
	// cache is keyed org-globally by (owner, repo), so resolvability
	// under the importer's credentials is what stops an org from
	// pinning its way into a same-named repo it cannot actually access.
	// Network I/O — deliberately before the DB tx.
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
	if _, err := paths.StateRootErr(); err != nil {
		return nil, nil, fmt.Errorf("resolve project root: %w", err)
	}
	projectRoot := paths.ProjectKBDir(orgID, newProjectID)
	kbRoot := filepath.Join(projectRoot, "knowledge-base")
	extractionBudget := newZipExtractionBudget(maxImportExtractBundleBytes, maxImportExtractEntryBytes)

	cleanup := &rollbackTracker{}
	committed := false
	defer func() {
		if !committed {
			cleanup.Cleanup()
		}
	}()

	// Files first (see the func doc). The tree is rooted under a
	// fresh uuid, so nothing can observe it before the row commits.
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

	var project *domain.Project
	if err := txr.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if err := ensureUniqueProjectName(ctx, tx.Projects, orgID, manifest.Project.Name); err != nil {
			return err
		}
		pinned := cloneStrings(manifest.Project.PinnedRepos)
		if _, err := tx.Projects.Create(ctx, orgID, teamID, domain.Project{
			ID:               newProjectID,
			Name:             strings.TrimSpace(manifest.Project.Name),
			Description:      manifest.Project.Description,
			CuratorSessionID: newSessionID,
			PinnedRepos:      pinned,
			JiraProjectKey:   manifest.Project.JiraProjectKey,
			LinearProjectKey: manifest.Project.LinearProjectKey,
		}); err != nil {
			return fmt.Errorf("insert imported project: %w", err)
		}

		requestIDMap, err := importCuratorRequests(ctx, tx.Curator, orgID, newProjectID, entries[curatorRequestsPath])
		if err != nil {
			return err
		}
		if err := importCuratorMessages(ctx, tx.Curator, orgID, requestIDMap, entries[curatorMessagesPath]); err != nil {
			return err
		}
		if err := importPendingContext(ctx, tx.Curator, orgID, newProjectID, newSessionID, requestIDMap, entries[curatorPendingContextPath]); err != nil {
			return err
		}
		if err := trackImportedRepos(ctx, tx, orgID, teamID, manifest.Project.PinnedRepos, cloneURLs); err != nil {
			return err
		}

		project, err = tx.Projects.Get(ctx, orgID, newProjectID)
		if err != nil {
			return fmt.Errorf("load imported project: %w", err)
		}
		if project == nil {
			return errors.New("imported project row is missing inside its own tx")
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	committed = true

	warnings := clonePinnedRepos(ctx, manifest.Project.PinnedRepos, cloneURLs)
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

// importCuratorRequests restores curator history into the new project.
// Runs inside the caller's WithTx: creator_user_id stamps to the
// importing user and team_id snapshots from the destination project row
// (TFAC-476) inside CuratorStore.ImportRequest.
func importCuratorRequests(ctx context.Context, curator db.CuratorStore, orgID, projectID string, zf *zip.File) (map[string]string, error) {
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
			row.ID = newID
			row.ProjectID = projectID
			if err := curator.ImportRequest(ctx, orgID, row); err != nil {
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

func importCuratorMessages(ctx context.Context, curator db.CuratorStore, orgID string, requestIDMap map[string]string, zf *zip.File) error {
	err := decodeZipJSONLines(
		zf,
		maxImportJSONLEntryBytes,
		maxImportJSONLRows,
		func(row domain.CuratorMessage) error {
			requestID := requestIDMap[row.RequestID]
			if requestID == "" {
				return fmt.Errorf("curator message references unknown request_id %q", row.RequestID)
			}
			row.RequestID = requestID
			row.ID = 0 // let the destination DB assign the message id
			if _, err := curator.InsertMessage(ctx, orgID, &row); err != nil {
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
	ctx context.Context,
	curator db.CuratorStore,
	orgID string,
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
			if row.ConsumedByRequestID != "" {
				mapped := requestIDMap[row.ConsumedByRequestID]
				if mapped == "" {
					return fmt.Errorf("pending context references unknown consumed_by_request_id %q", row.ConsumedByRequestID)
				}
				row.ConsumedByRequestID = mapped
			}
			row.ProjectID = projectID
			row.CuratorSessionID = newSessionID
			row.ID = 0
			if err := curator.ImportPendingContext(ctx, orgID, row); err != nil {
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

// trackImportedRepos materializes the imported project's pinned repos —
// the import behaves like a repo-selection save:
//
//   - team_github_repos — the per-team tracking selection that is the
//     source of truth. Without this the router team↔repo gate
//     (TracksRepoSystem) drops the importing team's handlers for the
//     repo, so polled events from it never create tasks until the user
//     re-saves the repo selection. Written via ReplaceForTeam (current
//     set ∪ pinned), which also reconciles the org-shared repo_profiles
//     cache — skeleton rows for newly-tracked repos — atomically inside
//     the caller's WithTx.
//   - repo_profiles.clone_url — pre-seeded with the URL discovered
//     during preflight (SeedCloneURL never clobbers an existing value),
//     so the first poll doesn't have to re-resolve it.
//
// Under Postgres RLS the team-row write is gated like any repo-selection
// save (team admin); a non-admin importing a bundle with pinned repos
// gets that denial surfaced as the import error rather than a silently
// untracked project.
func trackImportedRepos(ctx context.Context, tx db.TxStores, orgID, teamID string, pinned []string, cloneURLs map[string]string) error {
	if len(pinned) == 0 {
		return nil
	}
	existing, err := tx.TeamGitHubRepos.ListForTeam(ctx, teamID)
	if err != nil {
		return fmt.Errorf("list tracked repos for team: %w", err)
	}
	seen := make(map[string]struct{}, len(existing))
	merged := make([]domain.TeamGitHubRepo, 0, len(existing)+len(pinned))
	for _, r := range existing {
		seen[strings.ToLower(r.Owner)+"/"+strings.ToLower(r.Repo)] = struct{}{}
		merged = append(merged, r)
	}
	added := false
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			return fmt.Errorf("invalid pinned repo slug %q", slug)
		}
		key := strings.ToLower(owner) + "/" + strings.ToLower(repo)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, domain.TeamGitHubRepo{Owner: owner, Repo: repo})
		added = true
	}
	if added {
		if err := tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, teamID, merged); err != nil {
			return fmt.Errorf("track imported repos (requires team-admin in multi mode): %w", err)
		}
	}
	for _, slug := range pinned {
		cloneURL := cloneURLs[slug]
		if cloneURL == "" {
			continue
		}
		if err := tx.Repos.SeedCloneURL(ctx, orgID, slug, cloneURL); err != nil {
			return fmt.Errorf("seed clone url for %s: %w", slug, err)
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
	// The session tree goes where the curator that later RESUMES this
	// session will look for it: home-relative for direct (local) runs,
	// inside the org-scoped project root for sandboxed (multi) runs —
	// worktree.ClaudeProjectDir owns that branch. Writing to the
	// orchestrator's $HOME in multi mode would be both a tenant-scoping
	// violation and functionally dead (the sandboxed curator runs with
	// HOME=/work and could never see it).
	encodedRoot, err := worktree.ClaudeProjectDir(newResolvedCwd)
	if err != nil {
		return fmt.Errorf("resolve claude session dir for import: %w", err)
	}
	if err := os.MkdirAll(encodedRoot, 0o700); err != nil {
		return fmt.Errorf("mkdir claude project root: %w", err)
	}

	sessionTreeRoot := filepath.Join(encodedRoot, newSessionID)
	transcriptDest := filepath.Join(encodedRoot, newSessionID+".jsonl")
	cleanup.Add(sessionTreeRoot)
	cleanup.Add(transcriptDest)

	// Rewrite the transcript's embedded cwd strings to the path the
	// resuming agent will actually observe — the host path for direct
	// runs, "/work" for sandboxed runs (AgentVisibleRoot). The
	// manifest's ResolvedCwd is likewise the exporting agent's OBSERVED
	// cwd, so local↔multi round-trips rewrite correctly in both
	// directions.
	reps := buildSessionReplacements(
		manifestSession.CuratorSessionID,
		newSessionID,
		manifestSession.ResolvedCwd,
		agentproc.AgentVisibleRoot(newResolvedCwd),
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
	// Sandboxed (multi) deployments seed bare clones on demand with
	// per-org auth (worktree.EnsureSharedCuratorWorktree passes the
	// org's App token via WithCloneAuth); an eager ambient-credential
	// clone here would just fail and emit a warning per repo. The eager
	// warm-cache clone is a local-mode convenience.
	if agentproc.WillSandbox() {
		return warnings
	}
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
