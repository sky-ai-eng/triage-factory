import { listJiraIssueTypes, listJiraPriorities, listJiraStatuses } from '../lib/jiraProjects'
import { listLinearLabels, listLinearStates } from '../lib/linearTeams'
import {
  fetchTeamTrackedJiraProjects,
  fetchTeamTrackedLinearTeams,
} from '../pages/settings/teamConfig'

/** CatalogKind is the provider catalog a predicate field's ids come from. */
export type CatalogKind =
  | 'jira_status'
  | 'jira_issue_type'
  | 'jira_priority'
  | 'linear_state'
  | 'linear_label'
  | 'linear_label_group'

// The Jira and Linear predicate fields that name a catalog value by id. The
// server checks every id a save adds against the same catalogs.
const JIRA_CATALOG_FIELDS: Record<string, CatalogKind> = {
  status_id_in: 'jira_status',
  new_status_id_in: 'jira_status',
  old_status_id_in: 'jira_status',
  issue_type_id_in: 'jira_issue_type',
  priority_id_in: 'jira_priority',
  new_priority_id_in: 'jira_priority',
  old_priority_id_in: 'jira_priority',
}
const LINEAR_CATALOG_FIELDS: Record<string, CatalogKind> = {
  status_id_in: 'linear_state',
  new_status_id_in: 'linear_state',
  old_status_id_in: 'linear_state',
  label_id_in: 'linear_label',
  has_label_id_in: 'linear_label',
  label_group_id_in: 'linear_label_group',
}

/** catalogKindFor is the catalog the field of an event type's predicate names
 *  ids from, or null for a field that names none. The same field name means a
 *  Jira status on a Jira event and a Linear state on a Linear one. */
export function catalogKindFor(eventType: string, field: string): CatalogKind | null {
  if (eventType.startsWith('jira:')) return JIRA_CATALOG_FIELDS[field] ?? null
  if (eventType.startsWith('linear:')) return LINEAR_CATALOG_FIELDS[field] ?? null
  return null
}

export interface CatalogOption {
  id: string
  name: string
}

/** One group of options: a tracked Jira project's or Linear team's, or the
 *  Jira instance's for priorities, whose label is ''. */
export interface CatalogGroup {
  key: string
  label: string
  options: CatalogOption[]
}

/** loadCatalog reads the options kind offers the handler team teamId, grouped
 *  by the tracked Jira project or Linear team they come from: a status, an
 *  issue type, a state or a label of each, or the instance's priorities. null
 *  when any read failed, which is not "nothing to offer". */
export async function loadCatalog(
  kind: CatalogKind,
  orgId: string,
  teamId: string,
): Promise<CatalogGroup[] | null> {
  try {
    switch (kind) {
      case 'jira_priority': {
        const priorities = await listJiraPriorities(orgId)
        return [{ key: 'priorities', label: '', options: priorities }]
      }
      case 'jira_status':
      case 'jira_issue_type': {
        const projects = await fetchTeamTrackedJiraProjects(teamId)
        if (projects === null) return null
        // A project the poll has not resolved against Jira has no Jira id to
        // read its catalog by.
        return await Promise.all(
          projects
            .filter((p) => p.jira_id)
            .map(async (p) => ({
              key: p.id,
              label: p.name ? `${p.key} · ${p.name}` : p.key,
              options:
                kind === 'jira_status'
                  ? await listJiraStatuses(orgId, p.jira_id)
                  : await listJiraIssueTypes(orgId, p.jira_id),
            })),
        )
      }
      case 'linear_state':
      case 'linear_label':
      case 'linear_label_group': {
        const teams = await fetchTeamTrackedLinearTeams(teamId)
        if (teams === null) return null
        return await Promise.all(
          teams.map(async (t) => ({
            key: t.id,
            label: t.name ? `${t.key} · ${t.name}` : t.key,
            options: await linearOptions(kind, orgId, t.id),
          })),
        )
      }
    }
  } catch {
    return null
  }
}

async function linearOptions(
  kind: 'linear_state' | 'linear_label' | 'linear_label_group',
  orgId: string,
  linearTeamId: string,
): Promise<CatalogOption[]> {
  if (kind === 'linear_state') {
    const states = await listLinearStates(orgId, linearTeamId)
    return states.map(({ id, name }) => ({ id, name }))
  }
  const labels = await listLinearLabels(orgId, linearTeamId)
  // A group is not a label an issue can carry, and a label is not a group. A
  // label in a group is shown under its group's name, since two groups can
  // each hold a label of the same name.
  return labels
    .filter((l) => l.is_group === (kind === 'linear_label_group'))
    .map((l) => ({ id: l.id, name: l.group ? `${l.group.name} / ${l.name}` : l.name }))
}

/** CATALOG_NOUNS is how the field speaks of one value of each catalog, and of
 *  the source it is offered from. */
export const CATALOG_NOUNS: Record<
  CatalogKind,
  { one: string; many: string; source: 'jira_project' | 'linear_team' | 'instance' }
> = {
  jira_status: { one: 'status', many: 'statuses', source: 'jira_project' },
  jira_issue_type: { one: 'issue type', many: 'issue types', source: 'jira_project' },
  jira_priority: { one: 'priority', many: 'priorities', source: 'instance' },
  linear_state: { one: 'workflow state', many: 'workflow states', source: 'linear_team' },
  linear_label: { one: 'label', many: 'labels', source: 'linear_team' },
  linear_label_group: { one: 'label group', many: 'label groups', source: 'linear_team' },
}
