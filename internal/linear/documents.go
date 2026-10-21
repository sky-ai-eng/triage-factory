package linear

// issueFieldsFragment is the one selection of an issue. Every document that
// returns an issue spreads it, so a field added here reaches every caller.
//
// Linear has no comment count on an issue; the newest comment
// (comments(last: 1)) is the comment signal, and issueCommentsQuery reads the
// comments before it when more than one is new.
//
// The labels and sub-issues come as a first page of 25 with hasNextPage. Linear
// prices a query by its connections' page sizes and refuses one over its
// complexity ceiling, and a 50-issue search spreading this fragment with
// Linear's default page of 50 for both would sit right at that ceiling. An
// issue with more than 25 of either is completed by issueLabelsQuery or
// issueChildrenQuery, so nothing is cut short.
const issueFieldsFragment = `
fragment IssueFields on Issue {
  id identifier title description url priority priorityLabel
  createdAt updatedAt completedAt canceledAt archivedAt trashed
  state { id name type position }
  assignee { id name displayName email active admin guest isMe }
  creator { id name displayName email }
  parent { id identifier }
  team { id key name private }
  labels(first: 25) { nodes { id name } pageInfo { hasNextPage } }
  comments(last: 1) { nodes { ` + commentFields + ` } }
  children(first: 25) {
    nodes { id identifier state { id name type position } }
    pageInfo { hasNextPage }
  }
}`

// commentFields is the one selection of a comment, shared by
// issueFieldsFragment and issueCommentsQuery so both decode into Comment.
const commentFields = `id createdAt user { id name displayName } botActor { name }`

const pageInfoFields = `pageInfo { hasNextPage endCursor }`

const viewerQuery = `
query Viewer {
  viewer { id name displayName email active admin guest isMe app }
}`

const organizationQuery = `
query Organization {
  organization { id name urlKey }
}`

const whoamiQuery = `
query Whoami {
  viewer { id name displayName email active admin guest isMe app }
  organization { id name urlKey }
}`

const teamsQuery = `
query Teams($filter: TeamFilter, $first: Int!, $after: String) {
  teams(filter: $filter, first: $first, after: $after) {
    nodes { id key name private }
    ` + pageInfoFields + `
  }
}`

const teamQuery = `
query Team($id: String!) {
  team(id: $id) { id key name private }
}`

// teamStatesQuery reads a team's states through the team rather than as a
// filtered workflowStates listing, so a team the credential cannot see comes
// back as a missing team instead of an empty list.
const teamStatesQuery = `
query TeamStates($id: String!, $first: Int!, $after: String) {
  team(id: $id) {
    states(first: $first, after: $after) {
      nodes { id name type position }
      ` + pageInfoFields + `
    }
  }
}`

const workflowStateQuery = `
query WorkflowState($id: String!) {
  workflowState(id: $id) { id name type position team { id } }
}`

const issueQuery = `
query Issue($id: String!) {
  issue(id: $id) { ...IssueFields }
}` + issueFieldsFragment

const issueLabelsQuery = `
query IssueLabels($id: String!, $first: Int!, $after: String) {
  issue(id: $id) {
    labels(first: $first, after: $after) {
      nodes { id name }
      ` + pageInfoFields + `
    }
  }
}`

const issueChildrenQuery = `
query IssueChildren($id: String!, $first: Int!, $after: String) {
  issue(id: $id) {
    children(first: $first, after: $after) {
      nodes { id identifier state { id name type position } }
      ` + pageInfoFields + `
    }
  }
}`

// issueCommentsQuery pages through an issue's comments, narrowed by filter
// (a CommentFilter, or null for every comment).
const issueCommentsQuery = `
query IssueComments($id: String!, $filter: CommentFilter, $first: Int!, $after: String) {
  issue(id: $id) {
    comments(filter: $filter, first: $first, after: $after) {
      nodes { ` + commentFields + ` }
      ` + pageInfoFields + `
    }
  }
}`

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

// labelsQuery reads the labels an issue in a team can carry, each with the
// group it sits in. The group is read here and not on an issue's labels, where
// it would add to the batch read's complexity for every issue.
const labelsQuery = `
query Labels($teamID: ID!, $first: Int!, $after: String) {
  issueLabels(
    filter: { or: [{ team: { id: { eq: $teamID } } }, { team: { null: true } }] }
    first: $first
    after: $after
  ) {
    nodes { id name parent { name } }
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
