// Binding-graph editor scope (SKY-381). The /prompts page and the org-template
// editor are the *same* editor over different row sets: a team's prompts +
// handlers, or the org's template. The scope discriminator picks the endpoint
// set; everything downstream (nodes, drag-wire, drawers) is identical.
export type BindingScope = { kind: 'team'; teamId: string } | { kind: 'template' }

// isTemplate is the single source of truth for "which endpoint family" — the
// drawers take it as a plain boolean (templateScope), BindingGraph derives it
// from its scope prop.
export function isTemplateScope(scope: BindingScope): boolean {
  return scope.kind === 'template'
}

// promptsBase / handlersBase return the REST root for the scope. Template scope
// is org-scoped (no team_id); team scope keys off team_id (empty = solo/local,
// where the server resolves the sole team).
export function promptsBase(template: boolean): string {
  return template ? '/api/org-template/prompts' : '/api/prompts'
}

export function handlersBase(template: boolean): string {
  return template ? '/api/org-template/event-handlers' : '/api/event-handlers'
}

// blueprintsBase returns the REST root for blueprints. Both scopes are
// symmetric: a template trigger fires a template blueprint, a team trigger fires
// a team blueprint. Template scope hits the org-scoped /api/org-template/blueprints
// family; team scope hits the dedicated /api/blueprints endpoint.
export function blueprintsBase(template: boolean): string {
  return template ? '/api/org-template/blueprints' : '/api/blueprints'
}
