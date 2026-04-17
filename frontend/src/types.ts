export interface Task {
  id: string
  source: string
  source_id: string
  source_url: string
  title: string
  entity_kind: string
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
  breaker_threshold: number
  cooldown_seconds: number
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
