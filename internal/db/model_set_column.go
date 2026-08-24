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

// UnmarshalModelSetColumn decodes one enable-set column. NULL — and only NULL —
// decodes to nil, the absent set the domain resolvers read as "no preference
// expressed". Any stored text decodes to exactly what it names, `[]` included.
//
// The empty string is therefore a DECODE FAILURE, not a second spelling of
// absent. Nothing writes it (ModelSetColumnValue writes NULL or JSON, and both
// columns are nullable with no default), so a row holding it is corrupt — and
// reading corrupt as absent would resolve it to the whole catalog, quietly
// enabling models nobody chose. column names the column, because that is what a
// reader needs to go fix the row.
func UnmarshalModelSetColumn(v sql.NullString, column string) ([]string, error) {
	if !v.Valid {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", column, err)
	}
	return out, nil
}

// ModelSetColumnValue encodes an enable-set for storage. A nil set writes SQL
// NULL; anything else writes its JSON, so a stored set round-trips as itself —
// an empty non-nil one included, which is what keeps the nil/non-nil
// distinction the domain resolvers read from being lost at the column.
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
