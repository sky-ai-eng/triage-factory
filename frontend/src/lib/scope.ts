// Binding-graph editor scope: a team's prompts + handlers. teamId '' means
// solo/local (the server resolves the sole team).
export type BindingScope = { teamId: string }

// promptsBase / handlersBase / blueprintsBase return the REST root for the
// binding-graph editor.
export function promptsBase(): string {
  return '/api/prompts'
}

export function handlersBase(): string {
  return '/api/event-handlers'
}

export function blueprintsBase(): string {
  return '/api/blueprints'
}
