export type TaskSource = 'github' | 'jira'
export type EntityKind = 'pr' | 'issue' | 'epic' | 'message'

export interface Task {
  id: string
  entity_id: string
  source: TaskSource
  source_id: string
  source_url: string
  title: string
  entity_kind: EntityKind
  event_type: string
  dedup_key?: string
  severity?: string
  relevance_reason?: string
  scoring_status: string
  created_at: string
  status: string
  priority_score: number | null
  autonomy_suitability: number | null
  ai_summary?: string
  priority_reasoning?: string
  close_reason?: string
  // Non-zero when the Jira entity has open subtasks (status not in
  // Done.Members). UI surfaces a "consider decomposing" hint when set —
  // the task was created before subtasks appeared, or the user added them
  // after starting work. Always 0 for GitHub tasks.
  open_subtask_count?: number
}

export interface AgentRun {
  ID: string
  TaskID: string
  Status: string
  Model: string
  StartedAt: string
  CompletedAt?: string
  TotalCostUSD?: number
  DurationMs?: number
  NumTurns?: number
  StopReason?: string
  ResultSummary: string
}

export interface AgentMessage {
  ID: number
  RunID: string
  Role: string
  Content: string
  Subtype: string
  ToolCalls?: ToolCall[]
  ToolCallID: string
  IsError: boolean
  Model: string
  InputTokens?: number
  OutputTokens?: number
  CreatedAt: string
}

export interface ToolCall {
  id: string
  name: string
  input: Record<string, unknown>
}

export interface TriageEvent {
  id?: number
  event_type: string
  task_id: string
  source_id: string
  metadata: string
  created_at: string
}

export interface Prompt {
  id: string
  name: string
  body: string
  source: string
  usage_count: number
  created_at: string
  updated_at: string
}

export interface EventType {
  id: string
  source: string
  category: string
  label: string
  description: string
  default_priority: number
  enabled: boolean
  sort_order: number
}

export interface PromptTrigger {
  id: string
  prompt_id: string
  trigger_type: string
  event_type: string
  scope_predicate_json: string | null
  breaker_threshold: number
  min_autonomy_suitability: number
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface TaskRule {
  id: string
  event_type: string
  scope_predicate_json: string | null
  enabled: boolean
  name: string
  default_priority: number
  sort_order: number
  source: 'system' | 'user'
  created_at: string
  updated_at: string
}

export interface FieldSchema {
  name: string
  type: 'bool' | 'string' | 'int' | 'string_list'
  enum_values?: string[]
  description?: string
}

export interface EventSchema {
  event_type: string
  fields: FieldSchema[]
}

export interface ToastPayload {
  id: string
  level: 'info' | 'success' | 'warning' | 'error'
  title?: string
  body: string
}

export interface FactoryRecentEvent {
  event_type: string
  at: string
}

export interface FactoryEntity {
  id: string
  source: string
  source_id: string
  kind: string
  title: string
  url: string
  mine: boolean
  current_event_type?: string
  last_event_at?: string
  /** Last ~10 events for this entity, oldest first. The factory reconciler
   * walks this as an animation chain — a poll cycle that emitted two
   * events in sequence (new_commits → ci_passed) shows both transitions
   * rather than teleporting to the latest. */
  recent_events?: FactoryRecentEvent[]
  // GitHub PR fields.
  number?: number
  repo?: string
  author?: string
  additions?: number
  deletions?: number
  // Jira fields.
  status?: string
  priority?: string
  assignee?: string
}

export interface FactoryStation {
  event_type: string
  items_24h: number
  triggered_24h: number
  active_runs: number
  runs: Array<{
    run: AgentRun
    task: Task
    mine: boolean
  }>
}

export interface FactorySnapshot {
  stations: Record<string, FactoryStation>
  entities: FactoryEntity[]
}

export type WSEvent =
  | { type: 'agent_run_update'; run_id: string; data: { status: string } }
  | { type: 'agent_message'; run_id: string; data: AgentMessage }
  | { type: 'event'; data: TriageEvent }
  | { type: 'tasks_updated'; data: Record<string, never> }
  | { type: 'scoring_started'; data: { task_ids: string[] } }
  | { type: 'scoring_completed'; data: { task_ids: string[] } }
  | {
      type: 'repo_docs_updated'
      data: { id: string; has_readme: boolean; has_claude_md: boolean; has_agents_md: boolean }
    }
  | { type: 'repo_profile_updated'; data: { id: string; profile_text: string } }
  | { type: 'toast'; data: ToastPayload }
