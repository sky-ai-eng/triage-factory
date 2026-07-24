package projectbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type bundleArtifact struct {
	bundlePath string
	size       int64
	diskPath   string
	content    []byte
	// open streams the artifact's bytes from a non-disk source (the KB blob
	// store in multi mode) so a large KB file need not be buffered whole in
	// memory the way content does. Exactly one of content / diskPath / open is
	// set per artifact.
	open func(context.Context) (io.ReadCloser, error)
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
// to the requesting user (the conversation private-visibility arm is
// deliberately self-only — a multi-mode export carries the exporting user's
// own live curator conversation, the local single user's export carries
// theirs). Filesystem collection happens after the tx ends so disk walks
// never hold a claims-bound connection.
func collectExportState(ctx context.Context, txr db.TxRunner, kb *kbstore.Store, orgID, userID, projectID string) (*exportState, error) {
	var (
		project      *domain.Project
		conversation *domain.Conversation
		claims       []domain.Claim
		messages     []domain.Message
	)
	if err := txr.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(ctx, orgID, projectID)
		if e != nil || project == nil {
			return e
		}
		conversation, e = tx.Curator.GetLiveConversation(ctx, orgID, projectID, userID)
		if e != nil {
			return fmt.Errorf("read curator conversation: %w", e)
		}
		if conversation == nil {
			return nil
		}
		claims, e = tx.Curator.ListClaims(ctx, orgID, conversation.ID)
		if e != nil {
			return fmt.Errorf("list curator claims: %w", e)
		}
		messages, e = tx.Curator.ListConversationMessages(ctx, orgID, conversation.ID)
		if e != nil {
			return fmt.Errorf("list curator messages: %w", e)
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

	// Knowledge base: multi mode collects it from the blob store (control
	// hosts no KB on disk); local mode walks the on-disk knowledge-base dir.
	if kb != nil && runmode.Current() == runmode.ModeMulti {
		if err := appendKBStoreArtifacts(ctx, kb, orgID, project.ID, knowledgePrefix, &state.artifacts); err != nil {
			return nil, fmt.Errorf("collect knowledge files from store: %w", err)
		}
	} else if err := appendDirArtifacts(filepath.Join(projectRoot, "knowledge-base"), knowledgePrefix, &state.artifacts); err != nil {
		return nil, fmt.Errorf("collect knowledge files: %w", err)
	}

	// The SDK resume handle lives on the exporting user's live conversation
	// now, so the session tree ships only when that conversation exists and
	// has one.
	sdkSessionID := ""
	if conversation != nil {
		sdkSessionID = conversation.SessionID
	}
	sessionIncluded, sessionWarning, err := appendSessionArtifacts(resolvedRoot, sdkSessionID, &state.artifacts)
	if err != nil {
		return nil, err
	}
	if sessionWarning != "" {
		state.warnings = append(state.warnings, sessionWarning)
	}
	state.sessionInZip = sessionIncluded
	if sessionIncluded {
		state.manifest.Session = &ManifestSession{
			CuratorSessionID: sdkSessionID,
			// ResolvedCwd is the cwd AS THE AGENT SAW IT — the value
			// embedded in the transcript JSONL that import's
			// search-replace rewrite must match. For a sandboxed
			// (multi-mode) curator that is "/work", not the host-side
			// project root the tree was located through.
			ResolvedCwd: agentproc.AgentVisibleRoot(resolvedRoot),
		}
	}

	if err := appendCuratorArtifacts(conversation, claims, messages, &state.artifacts); err != nil {
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

// appendKBStoreArtifacts collects a project's knowledge base from the blob
// store as bundle artifacts, each with a streaming open() so a large KB file
// (an image or video an agent captured) is copied into the zip without ever
// buffering whole in memory. bundlePath mirrors the on-disk layout
// (knowledge-base/<name>) so an exported bundle is identical across modes.
func appendKBStoreArtifacts(ctx context.Context, kb *kbstore.Store, orgID, projectID, bundlePrefix string, out *[]bundleArtifact) error {
	files, err := kb.List(ctx, orgID, projectID)
	if err != nil {
		return err
	}
	for _, f := range files {
		name := f.Name
		*out = append(*out, bundleArtifact{
			bundlePath: path.Join(bundlePrefix, name),
			size:       f.Size,
			open: func(ctx context.Context) (io.ReadCloser, error) {
				return kb.Get(ctx, orgID, projectID, name)
			},
		})
	}
	return nil
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

// appendCuratorArtifacts serializes the already-collected curator
// conversation state. Pure serialization — every DB read happened inside
// collectExportState's WithTx. A project with no live conversation for the
// exporting user ships no curator artifacts at all.
func appendCuratorArtifacts(
	conversation *domain.Conversation,
	claims []domain.Claim,
	messages []domain.Message,
	out *[]bundleArtifact,
) error {
	if conversation == nil {
		return nil
	}
	convBytes, err := json.Marshal(conversation)
	if err != nil {
		return fmt.Errorf("encode curator conversation: %w", err)
	}
	claimBytes, err := marshalJSONLines(claims)
	if err != nil {
		return fmt.Errorf("encode curator claims: %w", err)
	}
	messageBytes, err := marshalJSONLines(messages)
	if err != nil {
		return fmt.Errorf("encode curator messages: %w", err)
	}
	*out = append(*out,
		bundleArtifact{bundlePath: curatorConversationPath, size: int64(len(convBytes)), content: convBytes},
		bundleArtifact{bundlePath: curatorClaimsPath, size: int64(len(claimBytes)), content: claimBytes},
		bundleArtifact{bundlePath: curatorMessagesPath, size: int64(len(messageBytes)), content: messageBytes},
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
