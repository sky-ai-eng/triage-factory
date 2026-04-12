export interface Task {
  id: string
  source: string
  source_id: string
  source_url: string
  title: string
  description?: string
  repo?: string
  author?: string
  labels: string[]
  severity?: string
  diff_additions?: number
  diff_deletions?: number
  files_changed?: number
  ci_status?: string
  relevance_reason?: string
  event_type?: string
  scoring_status: string
  created_at: string
  status: string
  priority_score: number | null
  ai_summary?: string
  priority_reasoning?: string
  agent_confidence: number | null
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
  ResultLink: string
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
  max_iterations: number
  cooldown_seconds: number
  enabled: boolean
  created_at: string
  updated_at: string
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
