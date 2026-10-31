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

// The create routes. Reads and per-id writes are one surface keyed by `kind`;
// authoring is not — a rule, a trigger and a notify rule need different
// fields, so each has its own route.
export function rulesCreatePath(): string {
  return `${handlersBase()}/rules`
}

export function triggersCreatePath(): string {
  return `${handlersBase()}/triggers`
}

export function notifyCreatePath(): string {
  return `${handlersBase()}/notify`
}

export function blueprintsBase(): string {
  return '/api/blueprints'
}
