package db

import (
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EncodeSystemParts renders a conversation's system parts for the
// system_parts column, nil for parts never recorded. Both dialects store the
// same bytes.
func EncodeSystemParts(parts []domain.SystemPart) ([]byte, error) {
	if parts == nil {
		return nil, nil
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return nil, fmt.Errorf("encode system parts: %w", err)
	}
	return b, nil
}

// DecodeSystemParts reads the system_parts column back, nil for a NULL.
func DecodeSystemParts(raw []byte) ([]domain.SystemPart, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	parts := []domain.SystemPart{}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode system parts: %w", err)
	}
	return parts, nil
}
