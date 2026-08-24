package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// The enable-set columns — org_settings.enabled_models and
// team_settings.enabled_models — are nullable TEXT holding a JSON array of
// catalog keys, in BOTH dialects. That is unusual here (a Postgres text[] is
// the house shape for a list) and deliberate: NULL is a value with its own
// meaning, distinct from a set naming nothing, and one encoding across the two
// backends means the absent-value semantics cannot come out different on one of
// them.

// UnmarshalModelSetColumn decodes one enable-set column. NULL decodes to nil —
// the absent set, which the domain resolvers read as "no preference expressed"
// — and any stored text decodes to exactly what it names, empty array included.
// column names the column in a decode failure, which is a corrupt row rather
// than a caller fault and has to say which one.
func UnmarshalModelSetColumn(v sql.NullString, column string) ([]string, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", column, err)
	}
	return out, nil
}

// ModelSetColumnValue encodes an enable-set for storage. A nil set writes SQL
// NULL; anything else writes its JSON, so a stored set round-trips as itself.
//
// No error to return: a []string always marshals, so the discarded error is
// structurally unreachable — which is what lets the dialect stores keep
// building their argument lists as plain value functions.
func ModelSetColumnValue(models []string) any {
	if models == nil {
		return nil
	}
	b, _ := json.Marshal(models)
	return string(b)
}
