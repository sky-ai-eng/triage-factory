package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// MarshalSystemParts is conversations.system_parts as both dialects bind it:
// the parts' JSON, or nil (NULL) for none.
func MarshalSystemParts(parts *domain.SystemParts) (any, error) {
	if parts == nil {
		return nil, nil
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return nil, fmt.Errorf("marshal system parts: %w", err)
	}
	return string(b), nil
}

// UnmarshalSystemParts reads conversations.system_parts back: nil for NULL.
func UnmarshalSystemParts(raw sql.NullString) (*domain.SystemParts, error) {
	if !raw.Valid {
		return nil, nil
	}
	var parts domain.SystemParts
	if err := json.Unmarshal([]byte(raw.String), &parts); err != nil {
		return nil, fmt.Errorf("unmarshal system parts: %w", err)
	}
	return &parts, nil
}
