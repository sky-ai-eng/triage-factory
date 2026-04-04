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
  input: Record<string, any>
}

export type WSEvent =
  | { type: 'agent_run_update'; run_id: string; data: { status: string } }
  | { type: 'agent_message'; run_id: string; data: AgentMessage }
  | { type: 'tasks_updated'; data: Record<string, never> }
  | { type: 'scoring_started'; data: { task_ids: string[] } }
  | { type: 'scoring_completed'; data: { task_ids: string[] } }
