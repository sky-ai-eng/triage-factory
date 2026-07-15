package projectbundle

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
)

// Preview returns the exact file list and aggregate size that Export would
// include for the given project, plus any non-fatal warnings (content that
// exists but could not be included). DB reads run claims-bound inside one
// WithTx — see collectExportState for the RLS visibility contract.
func Preview(ctx context.Context, txr db.TxRunner, kb *kbstore.Store, orgID, userID, projectID string) (*ExportPreview, error) {
	state, err := collectExportState(ctx, txr, kb, orgID, userID, projectID)
	if err != nil {
		return nil, err
	}
	out := &ExportPreview{
		Files:    make([]ExportPreviewFile, 0, len(state.artifacts)),
		Warnings: cloneStrings(state.warnings),
	}
	for _, a := range state.artifacts {
		out.Files = append(out.Files, ExportPreviewFile{
			Path:      a.bundlePath,
			SizeBytes: a.size,
		})
		out.TotalSize += a.size
	}
	return out, nil
}
