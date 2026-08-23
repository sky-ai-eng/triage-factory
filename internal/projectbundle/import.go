package projectbundle

import (
	"archive/zip"
	"context"
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
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// GitHubProbe provides the preflight clone URL lookup used by import.
type GitHubProbe interface {
	CloneURLForRepo(ctx context.Context, owner, repo string) (string, error)
}

const (
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
			"bundle extraction exceeds %d-byte total limit (next entry %s is %d bytes, %d bytes remain): %w",
			b.totalLimit,
			zf.Name,
			declared,
			b.remaining,
			ErrBadBundle,
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
// rollbackTracker removes them on any failure), then the project + repo
// writes run inside ONE claims-bound WithTx — so Postgres RLS gates the
// inserts under the importing user's identity, and the tx never holds
// a claims-bound pool connection through multi-GiB zip extraction.
func Import(
	ctx context.Context,
	txr db.TxRunner,
	kb *kbstore.Store,
	orgID, teamID, userID string,
	readerAt io.ReaderAt,
	size int64,
	probe GitHubProbe,
) (*domain.Project, []ImportWarning, error) {
	if size <= 0 {
		return nil, nil, fmt.Errorf("bundle is empty: %w", ErrBadBundle)
	}
	zr, err := zip.NewReader(readerAt, size)
	if err != nil {
		return nil, nil, fmt.Errorf("open bundle zip (%v): %w", err, ErrBadBundle)
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

	newProjectID := uuid.New().String()
	if _, err := paths.StateRootErr(); err != nil {
		return nil, nil, fmt.Errorf("resolve project root: %w", err)
	}
	projectRoot := paths.ProjectKBDir(orgID, newProjectID)
	kbRoot := filepath.Join(projectRoot, "knowledge-base")
	extractionBudget := newZipExtractionBudget(maxImportExtractBundleBytes, maxImportExtractEntryBytes)

	multiKB := kb != nil && runmode.Current() == runmode.ModeMulti

	cleanup := &rollbackTracker{}
	committed := false
	defer func() {
		if !committed {
			cleanup.Cleanup()
			if multiKB {
				// The KB landed in the store, not on disk — the disk rollback
				// can't reach it, so clear the prefix explicitly. WithoutCancel
				// so a cancelled import ctx still cleans up.
				if err := kb.DeletePrefix(context.WithoutCancel(ctx), orgID, newProjectID); err != nil {
					bundleLog.Warn("rollback: clear imported kb store prefix failed", "project", newProjectID, "error", err)
				}
			}
		}
	}()

	// Files first (see the func doc). The tree is rooted under a
	// fresh uuid, so nothing can observe it before the row commits.
	cleanup.Add(projectRoot)
	if err := os.MkdirAll(kbRoot, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir knowledge root: %w", err)
	}
	// Multi mode: the blob store is the KB source of truth, so stream the
	// bundle's knowledge entries there (the executor materializes them to disk
	// on the next turn). Local mode extracts to the on-disk knowledge-base dir.
	if multiKB {
		if err := uploadKnowledgeToStore(ctx, kb, orgID, newProjectID, entries, extractionBudget); err != nil {
			return nil, nil, err
		}
	} else if err := materializeKnowledge(entries, kbRoot, extractionBudget); err != nil {
		return nil, nil, err
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
			PinnedRepos:      pinned,
			JiraProjectKey:   manifest.Project.JiraProjectKey,
			LinearProjectKey: manifest.Project.LinearProjectKey,
		}); err != nil {
			return fmt.Errorf("insert imported project: %w", err)
		}
		if err := trackImportedRepos(ctx, tx, orgID, teamID, manifest.Project.PinnedRepos, cloneURLs); err != nil {
			return err
		}

		var err error
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
		return nil, fmt.Errorf("%s exceeds %d-byte limit: %w", zf.Name, maxBytes, ErrBadBundle)
	}
	return body, nil
}

func ensureUniqueProjectName(ctx context.Context, projects db.ProjectStore, orgID, incoming string) error {
	incoming = strings.TrimSpace(incoming)
	// Unwindowed (ListOpts zero Limit): a uniqueness check must see every
	// project, not a page of them — a duplicate on page two is still a
	// duplicate.
	rows, _, err := projects.List(ctx, orgID, db.Unwindowed)
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

// trackImportedRepos materializes the imported project's pinned repos —
// the import behaves like a repo-selection save:
//
//   - team_github_repos — the per-team tracking selection that is the
//     source of truth. Without this the router team↔repo gate
//     (TracksRepoSystem) drops the importing team's handlers for the
//     repo, so polled events from it never create tasks until the user
//     re-saves the repo selection. Written via ReplaceForTeam (current
//     set ∪ pinned), which also reconciles the org-shared repositories
//     cache — skeleton rows for newly-tracked repos — atomically inside
//     the caller's WithTx.
//   - repositories.clone_url — pre-seeded with the URL discovered
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
			return fmt.Errorf("invalid pinned repo slug %q: %w", slug, ErrBadBundle)
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
			// The tracked set is org-wide state, so adding to it is an admin
			// write (CLAUDE.md's write-scoping rule). A non-admin importer is
			// refused by RLS — the caller's authorization, not a server fault.
			if isPermissionDenial(err) {
				return fmt.Errorf("tracking the bundle's pinned repos requires team-admin on this team: %w", ErrPermissionDenied)
			}
			return fmt.Errorf("track imported repos: %w", err)
		}
	}
	// The bundle names its repos by slug, so this is the edge that resolves
	// one: ReplaceForTeam above has just brought a registry row into being for
	// every pinned entry, and the seed is keyed on that row's id. A name with
	// no row is a repo the caller was refused tracking on, or one dropped
	// between the two statements — either way there is nothing to warm, and
	// the import carries on rather than failing over a cache hint.
	for _, slug := range pinned {
		cloneURL := cloneURLs[slug]
		if cloneURL == "" {
			continue
		}
		owner, repo, _ := splitOwnerRepo(slug) // validated in the loop above
		row, err := tx.Repos.GetByRef(ctx, orgID, domain.RepoRef{Owner: owner, Repo: repo})
		if err != nil {
			return fmt.Errorf("resolve pinned repo %s: %w", slug, err)
		}
		if row == nil {
			continue
		}
		// The seeded row is not spent here: the import's answer is the project
		// it created, and whether this warm-cache hint landed or was outranked
		// by a URL already on file changes nothing the caller can see.
		if _, err := tx.Repos.SeedCloneURL(ctx, orgID, row.ID, cloneURL); err != nil {
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

// uploadKnowledgeToStore streams the bundle's knowledge entries into the KB
// blob store (multi mode) under the same extraction budget the on-disk path
// enforces. Nested entries — which the flat KB layout never produces — are
// rejected by the store's name validation, so a hand-crafted bundle can't
// nest its way past the flat contract.
func uploadKnowledgeToStore(ctx context.Context, kb *kbstore.Store, orgID, projectID string, entries map[string]*zip.File, extractionBudget *zipExtractionBudget) error {
	kbEntries, err := listEntriesWithPrefix(entries, knowledgePrefix)
	if err != nil {
		return err
	}
	for _, e := range kbEntries {
		rel, err := safeBundleRel(e.Name, knowledgePrefix)
		if err != nil {
			return err
		}
		if err := kbstore.ValidateName(rel); err != nil {
			return fmt.Errorf("bundle knowledge entry %q is not a flat KB file: %w", e.Name, err)
		}
		declared, err := extractionBudget.reserve(e.File)
		if err != nil {
			return err
		}
		rc, err := e.File.Open()
		if err != nil {
			return fmt.Errorf("open bundle entry %s: %w", e.Name, err)
		}
		reader := &countingReader{r: io.LimitReader(rc, declared+1)}
		putErr := kb.Put(ctx, orgID, projectID, rel, reader)
		rc.Close()
		if putErr != nil {
			return fmt.Errorf("upload knowledge %s to store: %w", rel, putErr)
		}
		if err := verifyZipEntryBytes(e.Name, reader.n, declared); err != nil {
			return err
		}
	}
	return nil
}

func clonePinnedRepos(ctx context.Context, pinned []string, cloneURLs map[string]string) []ImportWarning {
	warnings := make([]ImportWarning, 0)
	// Sandboxed (multi) deployments skip the eager clone here: there is no
	// org-scoped credential available at import time, so an ambient-credential
	// clone would just fail and emit a warning per repo. The eager warm-cache
	// clone below is a local-mode convenience.
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
			return nil, fmt.Errorf("invalid bundle path %q: %w", zf.Name, ErrBadBundle)
		}
		clean := path.Clean(name)
		if clean == "." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("invalid bundle path %q: %w", zf.Name, ErrBadBundle)
		}
		if _, exists := out[clean]; exists {
			return nil, fmt.Errorf("duplicate bundle path %q: %w", clean, ErrBadBundle)
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
		return "", fmt.Errorf("bundle path %q does not start with %q: %w", name, prefix, ErrBadBundle)
	}
	rel := strings.TrimPrefix(name, prefix)
	rel = path.Clean(rel)
	if rel == "." || rel == "" {
		return "", fmt.Errorf("bundle path %q has no relative component: %w", name, ErrBadBundle)
	}
	if strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("bundle path %q escapes its prefix: %w", name, ErrBadBundle)
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

func zipEntryDeclaredSize(zf *zip.File, maxBytes int64) (int64, error) {
	declared := zf.UncompressedSize64
	if maxBytes > 0 && declared > uint64(maxBytes) {
		return 0, fmt.Errorf("%s exceeds %d-byte limit: %w", zf.Name, maxBytes, ErrBadBundle)
	}
	if declared > uint64(math.MaxInt64-1) {
		return 0, fmt.Errorf("%s declared uncompressed size is too large: %w", zf.Name, ErrBadBundle)
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
