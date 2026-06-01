package projectbundle

import (
	"bytes"
	"context"
	"database/sql"
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
}

func collectExportState(ctx context.Context, database *sql.DB, projects db.ProjectStore, curatorStore db.CuratorStore, orgID, projectID string) (*exportState, error) {
	project, err := projects.Get(ctx, orgID, projectID)
	if err != nil {
		return nil, fmt.Errorf("load project: %w", err)
	}
	if project == nil {
		return nil, ErrProjectNotFound
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

	sessionIncluded, err := appendSessionArtifacts(resolvedRoot, project.CuratorSessionID, &state.artifacts)
	if err != nil {
		return nil, err
	}
	state.sessionInZip = sessionIncluded
	if sessionIncluded {
		state.manifest.Session = &ManifestSession{
			CuratorSessionID: project.CuratorSessionID,
			ResolvedCwd:      resolvedRoot,
		}
	}

	if err := appendCuratorArtifacts(ctx, database, curatorStore, orgID, project.ID, &state.artifacts); err != nil {
		return nil, err
	}

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

func appendSessionArtifacts(resolvedProjectRoot, curatorSessionID string, out *[]bundleArtifact) (bool, error) {
	if strings.TrimSpace(curatorSessionID) == "" {
		return false, nil
	}
	// ~/.claude/projects is Claude Code SDK session state keyed to the
	// real HOME, not TF state — it stays home-relative even in multi mode
	// (where TF state diverges onto a mounted volume), so it does not
	// route through internal/paths.
	home, err := os.UserHomeDir() //nolint:forbidigo // Claude Code SDK session state, not TF state (see internal/paths doc).
	if err != nil {
		return false, fmt.Errorf("resolve home dir for session export: %w", err)
	}
	encoded := worktree.EncodeClaudeProjectDir(resolvedProjectRoot)
	transcriptPath := filepath.Join(home, ".claude", "projects", encoded, curatorSessionID+".jsonl")
	st, err := os.Stat(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat session transcript: %w", err)
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("session transcript is not a regular file: %s", transcriptPath)
	}
	*out = append(*out, bundleArtifact{
		bundlePath: sessionTranscriptPath,
		size:       st.Size(),
		diskPath:   transcriptPath,
	})

	sessionRoot := filepath.Join(home, ".claude", "projects", encoded, curatorSessionID)
	if err := appendDirArtifacts(filepath.Join(sessionRoot, "subagents"), sessionSubagentsPrefix, out); err != nil {
		return false, fmt.Errorf("collect subagent files: %w", err)
	}
	if err := appendDirArtifacts(filepath.Join(sessionRoot, "tool-results"), sessionToolResultsPrefix, out); err != nil {
		return false, fmt.Errorf("collect tool-result files: %w", err)
	}
	return true, nil
}

func appendCuratorArtifacts(ctx context.Context, database *sql.DB, curatorStore db.CuratorStore, orgID, projectID string, out *[]bundleArtifact) error {
	requests, err := db.ListCuratorRequestsByProject(database, projectID)
	if err != nil {
		return fmt.Errorf("list curator requests: %w", err)
	}
	requestBytes, err := marshalJSONLines(requests)
	if err != nil {
		return fmt.Errorf("encode curator requests: %w", err)
	}
	*out = append(*out, bundleArtifact{
		bundlePath: curatorRequestsPath,
		size:       int64(len(requestBytes)),
		content:    requestBytes,
	})

	requestIDs := make([]string, 0, len(requests))
	for _, req := range requests {
		requestIDs = append(requestIDs, req.ID)
	}
	msgByReq, err := db.ListCuratorMessagesByRequestIDs(database, requestIDs)
	if err != nil {
		return fmt.Errorf("list curator messages: %w", err)
	}
	flatMessages := make([]domain.CuratorMessage, 0)
	for _, reqID := range requestIDs {
		flatMessages = append(flatMessages, msgByReq[reqID]...)
	}
	messageBytes, err := marshalJSONLines(flatMessages)
	if err != nil {
		return fmt.Errorf("encode curator messages: %w", err)
	}
	*out = append(*out, bundleArtifact{
		bundlePath: curatorMessagesPath,
		size:       int64(len(messageBytes)),
		content:    messageBytes,
	})

	pending, err := curatorStore.ListPendingContext(ctx, orgID, projectID)
	if err != nil {
		return fmt.Errorf("list curator pending context: %w", err)
	}
	pendingBytes, err := marshalJSONLines(pending)
	if err != nil {
		return fmt.Errorf("encode curator pending context: %w", err)
	}
	*out = append(*out, bundleArtifact{
		bundlePath: curatorPendingContextPath,
		size:       int64(len(pendingBytes)),
		content:    pendingBytes,
	})

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
