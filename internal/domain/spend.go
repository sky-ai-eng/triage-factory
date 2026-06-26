package domain

import "time"

// Spend categories — the axis the llm_spend view (TFAC-472) normalizes the
// three source tables onto. A delegated run is 'manual' (per-user) or
// 'autonomous' (event-triggered) by its trigger_type; a curator turn is
// 'curator'; a headless system job (scorer / repo-profiler / classifier) is
// 'system_overhead'. The dashboards + safety cap group on these.
const (
	SpendCategoryManual         = "manual"
	SpendCategoryAutonomous     = "autonomous"
	SpendCategoryCurator        = "curator"
	SpendCategorySystemOverhead = "system_overhead"
)

// Spend sources — which underlying table a SpendRow came from. Mirrors the
// view's `source` discriminator so callers can attribute a row back to runs /
// curator_requests / system_llm_runs without re-querying.
const (
	SpendSourceRun     = "run"
	SpendSourceCurator = "curator"
	SpendSourceSystem  = "system"
)

// SpendRow is one row from the llm_spend view — a single unit of settled LLM
// spend from any of the three sources. Read-only; assembled by db.SpendStore.
//
// Settled-spend semantics: TotalCostUSD and the four token counts are 0 for an
// in-flight (non-terminal) run/curator turn and carry their real values once a
// terminal write lands (normal completion, cancel, infra-failure, or a
// boot-time orphan sweep). System rows are always terminal.
//
// Nullable columns are pointers: TeamID is set only for runs (curator + system
// are org-level); Subtype carries the job name for system rows (NULL
// otherwise); CreatorUserID is the human for runs/curator (NULL for system);
// ActorAgentID is the executing org agent for runs (NULL for curator/system);
// Model is NULL for curator turns.
type SpendRow struct {
	Source              string    `json:"source"`
	SourceID            string    `json:"source_id"`
	OrgID               string    `json:"org_id"`
	TeamID              *string   `json:"team_id"`
	Category            string    `json:"category"`
	Subtype             *string   `json:"subtype"`
	CreatorUserID       *string   `json:"creator_user_id"`
	ActorAgentID        *string   `json:"actor_agent_id"`
	Model               *string   `json:"model"`
	TotalCostUSD        float64   `json:"total_cost_usd"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	CacheReadTokens     int       `json:"cache_read_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	OccurredAt          time.Time `json:"occurred_at"`
}

// SpendBucket is one category's aggregated spend over the queried window —
// the shape db.SpendStore.SpendByCategory returns (one bucket per distinct
// category present). Cost + every token total are summed across the union.
type SpendBucket struct {
	Category            string  `json:"category"`
	TotalCostUSD        float64 `json:"total_cost_usd"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
}

// SpendFilter bounds a db.SpendStore.ListSpend read. Every field is optional:
// a nil pointer / zero time / non-positive Limit drops that clause. TeamID
// narrows to one team's runs (curator/system rows carry a NULL team_id and so
// are excluded by a non-nil TeamID — intended: the team dashboard sees its own
// runs, not org-level overhead). Rows come back newest-first (occurred_at DESC).
type SpendFilter struct {
	TeamID   *string
	Category *string
	Since    time.Time
	Until    time.Time
	Limit    int
}
