package domain

import "time"

// Spend categories — the axis the llm_spend view (TFAC-472) normalizes its
// sources onto. A delegated conversation is 'manual' (per-user) or
// 'autonomous' (event-triggered) by its trigger_type; a headless system job
// (scorer / repo-profiler) is 'system_overhead'. The dashboards
// + safety cap group on these.
const (
	SpendCategoryManual         = "manual"
	SpendCategoryAutonomous     = "autonomous"
	SpendCategorySystemOverhead = "system_overhead"
)

// Spend sources — the view's `source` discriminator, so callers can tell a
// row's origin without re-querying. 'run' rows derive from conversation
// messages (delegated conversations); 'system' rows come from
// system_llm_runs. The values are persisted strings.
const (
	SpendSourceRun    = "run"
	SpendSourceSystem = "system"
)

// SpendRow is one row from the llm_spend view — a single unit of settled LLM
// spend from either source. Read-only; assembled by db.SpendStore.
//
// Settled-spend semantics: TotalCostUSD and the four token counts are 0 for an
// in-flight (non-terminal) delegated conversation and carry their real values
// once a terminal write lands (normal completion, cancel, infra-failure, or a
// boot-time orphan sweep). System rows are always terminal.
//
// Nullable columns are pointers: TeamID is set only for 'run' rows (system
// rows are org-level); Subtype carries the job name for system rows (NULL
// otherwise); CreatorUserID is the human for 'run' rows (NULL for system);
// ActorAgentID is the executing org agent for 'run' rows (NULL for system);
// TriggerID is the event_handler that fired an autonomous conversation (NULL
// for manual conversations and system — TFAC-478).
type SpendRow struct {
	Source              string    `json:"source"`
	SourceID            string    `json:"source_id"`
	OrgID               string    `json:"org_id"`
	TeamID              *string   `json:"team_id"`
	Category            string    `json:"category"`
	Subtype             *string   `json:"subtype"`
	CreatorUserID       *string   `json:"creator_user_id"`
	ActorAgentID        *string   `json:"actor_agent_id"`
	TriggerID           *string   `json:"trigger_id"`
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
// narrows to one team's spend (system rows carry a NULL team_id and so are
// excluded by a non-nil TeamID — intended: the team dashboard sees its own
// runs, not org-level overhead). CreatorUserID narrows to one human's spend
// (the /api/me/usage personal view, TFAC-478): manual runs that human
// created — autonomous/system rows carry a NULL creator and are excluded.
// Rows come back newest-first (occurred_at DESC).
type SpendFilter struct {
	TeamID        *string
	CreatorUserID *string
	Category      *string
	Since         time.Time
	Until         time.Time
	Limit         int
}
