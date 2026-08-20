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

// The two create routes. Reads and per-id writes are one surface keyed by
// `kind`; authoring is not — a rule and a trigger need different fields and
// default `enabled` in opposite directions, so each has its own route.
export function rulesCreatePath(): string {
  return `${handlersBase()}/rules`
}

export function triggersCreatePath(): string {
  return `${handlersBase()}/triggers`
}

export function blueprintsBase(): string {
  return '/api/blueprints'
}
