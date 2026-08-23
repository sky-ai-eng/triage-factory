package projectbundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
	project   *domain.Project
	manifest  Manifest
	artifacts []bundleArtifact
	warnings  []string
}

// collectExportState gathers everything Export/Preview serialize. The
// project read runs claims-bound inside one WithTx so Postgres RLS scopes it
// to the requesting user; filesystem collection happens after the tx ends so
// disk walks never hold a claims-bound connection.
func collectExportState(ctx context.Context, txr db.TxRunner, kb *kbstore.Store, orgID, userID, projectID string) (*exportState, error) {
	var project *domain.Project
	if err := txr.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(ctx, orgID, projectID)
		return e
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

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
