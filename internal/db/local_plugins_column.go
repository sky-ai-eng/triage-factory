package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The team_settings.local_plugins column and the claims.plugins_loaded column
// hold JSON the dialect stores encode and decode identically, so the codecs
// live here and the two backends cannot disagree about what an absent value
// is.

// UnmarshalLocalPluginsColumn decodes team_settings.local_plugins. NULL and an
// empty string both decode to the empty list: the column is NOT NULL with a
// '[]' default, so neither is written, and neither names a plugin. The result
// is never nil, so a caller serializes `[]` rather than `null`.
func UnmarshalLocalPluginsColumn(v sql.NullString, column string) ([]domain.LocalPlugin, error) {
	out := []domain.LocalPlugin{}
	if !v.Valid || v.String == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", column, err)
	}
	if out == nil {
		out = []domain.LocalPlugin{}
	}
	return out, nil
}

// MarshalLocalPluginsColumn encodes a plugin list for storage. A nil list
// stores `[]`, never `null`, so what the column holds always decodes as a
// list.
func MarshalLocalPluginsColumn(list []domain.LocalPlugin) (string, error) {
	if list == nil {
		list = []domain.LocalPlugin{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalPluginsLoadedColumn decodes claims.plugins_loaded. NULL decodes to
// nil: the engagement was given no plugin, or never reached Claude Code's
// init. Stored text decodes to exactly what it names, and an unreadable value
// is an error naming the column.
func UnmarshalPluginsLoadedColumn(v sql.NullString, column string) (*domain.PluginsLoaded, error) {
	if !v.Valid {
		return nil, nil
	}
	var out domain.PluginsLoaded
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", column, err)
	}
	return &out, nil
}

// MarshalPluginsLoadedColumn encodes a plugins_loaded report for storage.
func MarshalPluginsLoadedColumn(report domain.PluginsLoaded) (string, error) {
	b, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
