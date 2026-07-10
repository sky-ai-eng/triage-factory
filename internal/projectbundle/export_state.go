package projectbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type bundleArtifact struct {
	bundlePath string
	size       int64
	diskPath   string
	content    []byte
}

type exportState struct {
	project      *domain.Project
	manifest     Manifest
	artifacts    []bundleArtifact
	sessionInZip bool
	warnings     []string
}

// collectExportState gathers everything Export/Preview serialize. DB
// reads run inside one claims-bound WithTx so Postgres RLS scopes them
// to the requesting user (the curator select policies are deliberately
// self-only — a multi-mode export carries the exporting user's own chat
// turns, the local single user's export carries everything). Filesystem
// collection happens after the tx ends so disk walks never hold a
// claims-bound connection.
func collectExportState(ctx context.Context, txr db.TxRunner, orgID, userID, projectID string) (*exportState, error) {
	var (
		project  *domain.Project
		requests []domain.CuratorRequest
		msgByReq map[string][]domain.CuratorMessage
		pending  []domain.CuratorPendingContext
	)
	if err := txr.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(ctx, orgID, projectID)
		if e != nil || project == nil {
			return e
		}
		requests, e = tx.Curator.ListRequestsByProject(ctx, orgID, projectID)
		if e != nil {
			return fmt.Errorf("list curator requests: %w", e)
		}
		requestIDs := make([]string, 0, len(requests))
		for _, req := range requests {
			requestIDs = append(requestIDs, req.ID)
		}
		msgByReq, e = tx.Curator.ListMessagesByRequestIDs(ctx, orgID, requestIDs)
		if e != nil {
			return fmt.Errorf("list curator messages: %w", e)
		}
		pending, e = tx.Curator.ListPendingContext(ctx, orgID, projectID)
		if e != nil {
			return fmt.Errorf("list curator pending context: %w", e)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if project == nil {
		return nil, ErrProjectNotFound
	}

	if _, err := paths.StateRootErr(); err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	projectRoot := paths.ProjectKBDir(orgID, project.ID)
	resolvedRoot := projectRoot
	if resolved, err := filepath.EvalSymlinks(projectRoot); err == nil {
		resolvedRoot = resolved
	}

	state := &exportState{
		project: project,
		manifest: Manifest{
			FormatVersion: FormatVersion,
			ExportedAt:    time.Now().UTC(),
			Project: ManifestProject{
				Name:             project.Name,
				Description:      project.Description,
				PinnedRepos:      cloneStrings(project.PinnedRepos),
				JiraProjectKey:   project.JiraProjectKey,
				LinearProjectKey: project.LinearProjectKey,
			},
		},
	}

	if err := appendDirArtifacts(filepath.Join(projectRoot, "knowledge-base"), knowledgePrefix, &state.artifacts); err != nil {
		return nil, fmt.Errorf("collect knowledge files: %w", err)
	}

	sessionIncluded, sessionWarning, err := appendSessionArtifacts(resolvedRoot, project.CuratorSessionID, &state.artifacts)
	if err != nil {
		return nil, err
	}
	if sessionWarning != "" {
		state.warnings = append(state.warnings, sessionWarning)
	}
	state.sessionInZip = sessionIncluded
	if sessionIncluded {
		state.manifest.Session = &ManifestSession{
			CuratorSessionID: project.CuratorSessionID,
			// ResolvedCwd is the cwd AS THE AGENT SAW IT — the value
			// embedded in the transcript JSONL that import's
			// search-replace rewrite must match. For a sandboxed
			// (multi-mode) curator that is "/work", not the host-side
			// project root the tree was located through.
			ResolvedCwd: agentproc.AgentVisibleRoot(resolvedRoot),
		}
	}

	if err := appendCuratorArtifacts(requests, msgByReq, pending, &state.artifacts); err != nil {
		return nil, err
	}

	state.manifest.Warnings = cloneStrings(state.warnings)
	manifestBytes, err := encodeManifest(state.manifest)
	if err != nil {
		return nil, err
	}
	state.artifacts = append(state.artifacts, bundleArtifact{
		bundlePath: manifestPath,
		size:       int64(len(manifestBytes)),
		content:    manifestBytes,
	})

	sort.Slice(state.artifacts, func(i, j int) bool { return state.artifacts[i].bundlePath < state.artifacts[j].bundlePath })
	seen := make(map[string]struct{}, len(state.artifacts))
	for _, a := range state.artifacts {
		if a.bundlePath == "" {
			return nil, errors.New("internal: empty bundle path")
		}
		if _, ok := seen[a.bundlePath]; ok {
			return nil, fmt.Errorf("internal: duplicate bundle path %q", a.bundlePath)
		}
		seen[a.bundlePath] = struct{}{}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return state, nil
}

func appendDirArtifacts(dir, bundlePrefix string, out *[]bundleArtifact) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(dir, func(full string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == "" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		*out = append(*out, bundleArtifact{
			bundlePath: path.Join(bundlePrefix, rel),
			size:       info.Size(),
			diskPath:   full,
		})
		return nil
	})
}

// appendSessionArtifacts locates the curator's Claude Code session tree
// through worktree.ClaudeProjectDir — home-relative for direct (local)
// runs, inside the org-scoped project root for sandboxed (multi) runs.
// A missing transcript returns (false, "", nil) as before (project
// never ran a curator session, or the tree was cleaned). A transcript
// that EXISTS but is unreadable by this process — possible in multi
// mode, where run trees are chowned to the sandbox uid — is surfaced as
// a warning instead of being silently dropped, so a bundle that ships
// without its session says so.
func appendSessionArtifacts(resolvedProjectRoot, curatorSessionID string, out *[]bundleArtifact) (bool, string, error) {
	if strings.TrimSpace(curatorSessionID) == "" {
		return false, "", nil
	}
	sessionDir, err := worktree.ClaudeProjectDir(resolvedProjectRoot)
	if err != nil {
		return false, "", fmt.Errorf("resolve claude session dir for export: %w", err)
	}
	transcriptPath := filepath.Join(sessionDir, curatorSessionID+".jsonl")
	st, err := os.Stat(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		if os.IsPermission(err) {
			// The user-facing warning stays generic — the server-side
			// absolute path is operator information, not something the
			// API should leak into the UI. Log it here instead.
			bundleLog.Warn("session transcript unreadable during export; bundle ships without the session",
				"path", transcriptPath, "session", curatorSessionID, "error", err)
			return false, "the project's curator session transcript exists but is not readable by the server; the bundle was exported without the session (server logs have details)", nil
		}
		return false, "", fmt.Errorf("stat session transcript: %w", err)
	}
	if !st.Mode().IsRegular() {
		return false, "", fmt.Errorf("session transcript is not a regular file: %s", transcriptPath)
	}
	*out = append(*out, bundleArtifact{
		bundlePath: sessionTranscriptPath,
		size:       st.Size(),
		diskPath:   transcriptPath,
	})

	sessionRoot := filepath.Join(sessionDir, curatorSessionID)
	if err := appendDirArtifacts(filepath.Join(sessionRoot, "subagents"), sessionSubagentsPrefix, out); err != nil {
		return false, "", fmt.Errorf("collect subagent files: %w", err)
	}
	if err := appendDirArtifacts(filepath.Join(sessionRoot, "tool-results"), sessionToolResultsPrefix, out); err != nil {
		return false, "", fmt.Errorf("collect tool-result files: %w", err)
	}
	return true, "", nil
}

// appendCuratorArtifacts serializes the already-collected curator rows.
// Pure serialization — every DB read happened inside
// collectExportState's WithTx.
func appendCuratorArtifacts(
	requests []domain.CuratorRequest,
	msgByReq map[string][]domain.CuratorMessage,
	pending []domain.CuratorPendingContext,
	out *[]bundleArtifact,
) error {
	requestBytes, err := marshalJSONLines(requests)
	if err != nil {
		return fmt.Errorf("encode curator requests: %w", err)
	}
	flatMessages := make([]domain.CuratorMessage, 0)
	for _, req := range requests {
		flatMessages = append(flatMessages, msgByReq[req.ID]...)
	}
	messageBytes, err := marshalJSONLines(flatMessages)
	if err != nil {
		return fmt.Errorf("encode curator messages: %w", err)
	}
	pendingBytes, err := marshalJSONLines(pending)
	if err != nil {
		return fmt.Errorf("encode curator pending context: %w", err)
	}
	*out = append(*out,
		bundleArtifact{bundlePath: curatorRequestsPath, size: int64(len(requestBytes)), content: requestBytes},
		bundleArtifact{bundlePath: curatorMessagesPath, size: int64(len(messageBytes)), content: messageBytes},
		bundleArtifact{bundlePath: curatorPendingContextPath, size: int64(len(pendingBytes)), content: pendingBytes},
	)
	return nil
}

func marshalJSONLines[T any](items []T) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, item := range items {
		if err := enc.Encode(item); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
