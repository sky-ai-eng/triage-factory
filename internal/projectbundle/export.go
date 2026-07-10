package projectbundle

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Export builds a project bundle and streams it as a ZIP reader. DB
// reads run claims-bound inside one WithTx (see collectExportState for
// the RLS visibility contract); orgID scopes every lookup so a
// multi-mode caller cannot read another tenant's project state.
func Export(ctx context.Context, txr db.TxRunner, orgID, userID, projectID string) (io.ReadCloser, error) {
	state, err := collectExportState(ctx, txr, orgID, userID, projectID)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		err := writeExportZip(ctx, pw, state.artifacts)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	return pr, nil
}

func writeExportZip(ctx context.Context, w io.Writer, artifacts []bundleArtifact) error {
	zw := zip.NewWriter(w)
	for _, artifact := range artifacts {
		select {
		case <-ctx.Done():
			_ = zw.Close()
			return ctx.Err()
		default:
		}
		header := &zip.FileHeader{
			Name:   artifact.bundlePath,
			Method: zip.Deflate,
		}
		header.SetMode(0o644)
		dst, err := zw.CreateHeader(header)
		if err != nil {
			_ = zw.Close()
			return fmt.Errorf("create zip entry %q: %w", artifact.bundlePath, err)
		}
		if len(artifact.content) > 0 {
			if _, err := dst.Write(artifact.content); err != nil {
				_ = zw.Close()
				return fmt.Errorf("write zip entry %q: %w", artifact.bundlePath, err)
			}
			continue
		}
		if artifact.diskPath == "" {
			continue
		}
		src, err := os.Open(artifact.diskPath)
		if err != nil {
			_ = zw.Close()
			return fmt.Errorf("open %s: %w", artifact.diskPath, err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			src.Close()
			_ = zw.Close()
			return fmt.Errorf("copy %s: %w", artifact.diskPath, err)
		}
		src.Close()
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close export zip: %w", err)
	}
	return nil
}
