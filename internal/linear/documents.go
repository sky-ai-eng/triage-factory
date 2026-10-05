package linear

// issueFieldsFragment is the one selection of an issue. Every document that
// returns an issue spreads it, so a field added here reaches every caller.
//
// Linear has no comment count on an issue; the newest comment
// (comments(last: 1)) is the comment signal.
const issueFieldsFragment = `
fragment IssueFields on Issue {
  id identifier title description url priority priorityLabel
  createdAt updatedAt completedAt canceledAt archivedAt trashed
  state { id name type position }
  assignee { id name displayName email active admin guest isMe }
  creator { id name displayName email }
  parent { id identifier }
  team { id key name private }
  labels { nodes { name } }
  comments(last: 1) { nodes { id createdAt } }
  children { nodes { id identifier state { id name type position } } }
}`

const pageInfoFields = `pageInfo { hasNextPage endCursor }`

const viewerQuery = `
query Viewer {
  viewer { id name displayName email active admin guest isMe }
}`

const organizationQuery = `
query Organization {
  organization { id name urlKey }
}`

const whoamiQuery = `
query Whoami {
  viewer { id name displayName email active admin guest isMe }
  organization { id name urlKey }
}`

const teamsQuery = `
query Teams($filter: TeamFilter, $first: Int!, $after: String) {
  teams(filter: $filter, first: $first, after: $after) {
    nodes { id key name private }
    ` + pageInfoFields + `
  }
}`

const workflowStatesQuery = `
query WorkflowStates($teamID: ID!, $first: Int!, $after: String) {
  workflowStates(filter: { team: { id: { eq: $teamID } } }, first: $first, after: $after) {
    nodes { id name type position }
    ` + pageInfoFields + `
  }
}`

const issueQuery = `
query Issue($id: String!) {
  issue(id: $id) { ...IssueFields }
}` + issueFieldsFragment

const issuesByIDQuery = `
query IssuesByID($ids: [ID!]!, $first: Int!) {
  issues(filter: { id: { in: $ids } }, first: $first, includeArchived: true) {
    nodes { ...IssueFields }
  }
}` + issueFieldsFragment

const searchIssuesQuery = `
query SearchIssues($filter: IssueFilter, $first: Int!, $after: String) {
  issues(filter: $filter, first: $first, after: $after, orderBy: updatedAt) {
    nodes { ...IssueFields }
    ` + pageInfoFields + `
  }
}` + issueFieldsFragment

const labelsQuery = `
query Labels($teamID: ID!, $first: Int!, $after: String) {
  issueLabels(
    filter: { or: [{ team: { id: { eq: $teamID } } }, { team: { null: true } }] }
    first: $first
    after: $after
  ) {
    nodes { id name }
    ` + pageInfoFields + `
  }
}`

const issueUpdateMutation = `
mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    success
    issue { ...IssueFields }
  }
}` + issueFieldsFragment

const issueCreateMutation = `
mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { ...IssueFields }
  }
}` + issueFieldsFragment

const commentCreateMutation = `
mutation CommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
    comment { id }
  }
}`
