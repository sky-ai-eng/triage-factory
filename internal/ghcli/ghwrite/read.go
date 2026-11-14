package ghwrite

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Addressed reads. A run's touched entities are the pull requests and issues
// it addressed, and a read through the real-`gh` channel addresses one in one
// of three ways: by the repository and number the request spells, through a
// comment on it the request names by the comment's own id, or by its node id.
// This file names what the request addressed for the two transports, and
// nothing more. It decides nothing and records nothing.
//
// What happens to each differs by what can be checked. An object named by
// number, or through a comment, is one the injector can ask GitHub about by
// number, so it reads what GitHub calls the object before reporting it, and a
// request that names an object that does not exist, or one the run's
// credential cannot read, touches nothing. A node id is reported as the
// request spelled it: the injector reads by number and has no read that takes
// one, so the orchestrator checks it instead, against the objects TF already
// holds, and a node id naming none of them touches nothing.
//
// The rule is the exec verbs': addressed, touch; returned in a set, never. A
// list (`gh pr list`), a search, and a lookup by branch name
// (`pullRequests(headRefName:)`, which is how `gh pr view` with no number
// finds its pull request) return sets, and name no object here.
//
// TODO(TFAC-1415): `gh pr view` with no number reads the pull request of the
// current branch and touches nothing, because the request names a branch.
//
// An agent can address an object without reading it, by sending a request
// that only spells it. For an object named by number or through a comment
// that gains nothing a real read would not: the object is linked only once
// GitHub has answered for it under the run's own credential, which is what
// reading it takes. A node id is not checked against GitHub, so spelling one
// links an object TF holds without reading it; in multi mode the link is made
// only in a repository the run's credential reads, which is where the read it
// stands in for would have succeeded.

// ObjectRef is the pull request or issue one read addressed, as the request
// spelled it. GitHub numbers the two in one sequence, so the number alone does
// not say which it is.
type ObjectRef struct {
	Owner  string
	Repo   string
	Number int
}

// Key identifies the object among the ones a run has addressed. It folds case:
// GitHub's names are case-insensitive.
func (r ObjectRef) Key() string {
	return strings.ToLower(r.Owner+"/"+r.Repo) + "#" + strconv.Itoa(r.Number)
}

// CommentRef is a comment one REST read addressed by its own id, as the path
// spelled it: its repository, the numbering it is in (domain.GitHubCommentSpace),
// and its number. The comment is on one pull request or issue, which the path
// does not name.
type CommentRef struct {
	Owner string
	Repo  string
	Space domain.GitHubCommentSpace
	ID    int64
}

// ReadObservation is one addressed read as the injector saw it: the object the
// request spelled, and what GitHub answered the injector for it, its node id,
// which of the two it is, and its repository's id and current name
// (domain.GitHubIdentity). The run's touched entity is resolved from the
// identity alone; the spelling is a display key. A read through a comment
// carries the object the comment is on, as GitHub answered it, spelled under
// the repository the request named.
//
// A read by node id carries the node id in Identity and nothing else: no
// spelling, no kind, and no repository, since nothing was read from GitHub
// for it. The orchestrator only looks such an id up, among the pull requests
// and issues TF holds and the comments its runs posted, and creates nothing
// from it.
//
// The sidecar fills this in and the orchestrator records the touch, the split
// Observation keeps for a write.
type ReadObservation struct {
	ObjectRef
	Identity domain.GitHubIdentity
}

// NodeRead is the observation of a read that addressed an object by node id
// alone.
func NodeRead(nodeID string) ReadObservation {
	return ReadObservation{Identity: domain.GitHubIdentity{NodeID: nodeID}}
}

// maxAddressedReads bounds how many objects one GraphQL request may address. A
// porcelain query addresses one; the bound keeps a crafted document from
// spending the injector's reads on a list it spelled out.
const maxAddressedReads = 16

// ClassifyRead names the pull request or issue a REST read addresses: a GET of
// the object itself, or of anything under it (its files, its comments, its
// reviews, its timeline), which are attributes of that object. ok is false for
// every other request, including a collection and a comment addressed by its
// own id, whose path names no pull request or issue (ClassifyCommentRead names
// that comment).
func ClassifyRead(method, path string) (ObjectRef, bool) {
	if method != "GET" {
		return ObjectRef{}, false
	}
	owner, repo, rest, ok := splitRepoPath(path)
	if !ok || len(rest) < 2 || (rest[0] != "pulls" && rest[0] != "issues") {
		return ObjectRef{}, false
	}
	n, err := strconv.Atoi(rest[1])
	if err != nil || n <= 0 || !validRepoPart(owner) || !validRepoPart(repo) {
		return ObjectRef{}, false
	}
	return ObjectRef{Owner: owner, Repo: repo, Number: n}, true
}

// commentCollections maps the collection a comment's path enters to the
// numbering its comments are in: /issues/comments/{id} is a conversation
// comment, /pulls/comments/{id} a review comment.
var commentCollections = map[string]domain.GitHubCommentSpace{
	"issues": domain.GitHubIssueComment,
	"pulls":  domain.GitHubReviewComment,
}

// ClassifyCommentRead names the comment a REST read addresses by its own id: a
// GET of /repos/{o}/{r}/issues/comments/{id} or /pulls/comments/{id}, or of
// anything under it (its reactions). ok is false for every other request,
// including a repository's list of comments. A comment is an attribute of the
// pull request or issue it is on, so the read addresses that object; the path
// does not say which it is, and the injector reads the comment to find out.
func ClassifyCommentRead(method, path string) (CommentRef, bool) {
	if method != "GET" {
		return CommentRef{}, false
	}
	owner, repo, rest, ok := splitRepoPath(path)
	if !ok || len(rest) < 3 || rest[1] != "comments" {
		return CommentRef{}, false
	}
	space, ok := commentCollections[rest[0]]
	if !ok {
		return CommentRef{}, false
	}
	id, err := strconv.ParseInt(rest[2], 10, 64)
	if err != nil || id <= 0 || !validRepoPart(owner) || !validRepoPart(repo) {
		return CommentRef{}, false
	}
	return CommentRef{Owner: owner, Repo: repo, Space: space, ID: id}, true
}

// The fields a GraphQL read addresses an object through: the repository at the
// query's top level, by owner and name, and under it the pull request or
// issue, by number, which are the three finders `gh` sends and the shape a
// hand-written query takes; and the node at the query's top level, by id.
const (
	readRepositoryField = "repository"
	readOwnerArgument   = "owner"
	readNameArgument    = "name"
	readNumberArgument  = "number"
	readNodeField       = "node"
	readIDArgument      = "id"
)

// readTopLevelArguments are the top-level fields whose arguments the read walk
// reads, and which of them; readObjectArguments are the same for the fields
// selected directly under a repository. Every other field's arguments are
// skipped as balanced punctuation.
var (
	readTopLevelArguments = map[string][]string{
		readRepositoryField: {readOwnerArgument, readNameArgument},
		readNodeField:       {readIDArgument},
	}
	readObjectArguments = map[string][]string{
		"pullRequest":        {readNumberArgument},
		"issue":              {readNumberArgument},
		"issueOrPullRequest": {readNumberArgument},
	}
)

// GraphQLRead is what one GraphQL query addresses: the pull requests and
// issues it selects by number under a repository it names (Objects), and the
// objects it selects by node id (Nodes), each once and in the order the
// document gives them. Together they hold at most maxAddressedReads.
type GraphQLRead struct {
	Objects []ObjectRef
	Nodes   []string
}

// Empty reports that the query addressed nothing.
func (r GraphQLRead) Empty() bool {
	return len(r.Objects) == 0 && len(r.Nodes) == 0
}

// ParseGraphQLRead names what one GraphQL request addresses: each
// `repository(owner:, name:)` at the top level of the query that runs, and each
// `pullRequest`, `issue` or `issueOrPullRequest(number:)` selected directly
// under it, and each `node(id:)` at that top level, with aliases resolved.
// Empty for a mutation (the write audit's), for a query that addresses
// nothing, and for a document the reader cannot walk.
//
// It reads four argument values and no others: the repository's owner and
// name, the object's number and the node's id, each either a variable,
// resolved from the request's variables, or a literal. A value is accepted only
// in the shape of what it names (validRepoPart, a positive integer,
// validNodeID) and is otherwise dropped with the object it would have named.
// Every other argument, every nested selection and every fragment is skipped
// as balanced punctuation, exactly as the mutation reader skips them
// (gqldoc.go).
func ParseGraphQLRead(body []byte) GraphQLRead {
	envelope, ok := decodeGraphQLEnvelope(body)
	if !ok || envelope.Query == "" {
		return GraphQLRead{}
	}
	// Most reads that address nothing never mention a repository or a node,
	// and stop here.
	if !strings.Contains(envelope.Query, readRepositoryField) && !strings.Contains(envelope.Query, readNodeField) {
		return GraphQLRead{}
	}
	ops, walked := parseReadOperations(envelope.Query)
	if !walked {
		return GraphQLRead{}
	}
	op, selected := selectReadOperation(ops, envelope.OperationName)
	if !selected || op.typ != gqlOperationQuery || (len(op.reads) == 0 && len(op.nodes) == 0) {
		return GraphQLRead{}
	}

	var vars map[string]json.RawMessage
	if len(envelope.Variables) > 0 {
		// Variables that are not an object resolve no variable, which costs
		// only the objects a variable named.
		_ = json.Unmarshal(envelope.Variables, &vars)
	}
	var out GraphQLRead
	seen := map[string]bool{}
	for _, read := range op.reads {
		owner, ok := read.owner.repoPart(vars)
		if !ok {
			continue
		}
		repo, ok := read.name.repoPart(vars)
		if !ok {
			continue
		}
		number, ok := read.number.objectNumber(vars)
		if !ok {
			continue
		}
		ref := ObjectRef{Owner: owner, Repo: repo, Number: number}
		if seen[ref.Key()] {
			continue
		}
		seen[ref.Key()] = true
		out.Objects = append(out.Objects, ref)
	}
	seenNodes := map[string]bool{}
	for _, node := range op.nodes {
		id, ok := node.nodeID(vars)
		if !ok || seenNodes[id] {
			continue
		}
		seenNodes[id] = true
		out.Nodes = append(out.Nodes, id)
	}
	return out
}

// gqlArgument is one argument's value as the read walk keeps it: the name of
// the variable it is given, or the literal token. The zero value is an
// argument the field did not carry, or carried as something that is neither.
type gqlArgument struct {
	variable string
	literal  gqlToken
}

// stringValue resolves the argument to a string: a string variable or a
// single-line string literal. A literal's characters are as written, escapes
// undecoded, so a caller that admits no backslash refuses one spelled with an
// escape rather than decoding it.
func (a gqlArgument) stringValue(vars map[string]json.RawMessage) (string, bool) {
	var value string
	switch {
	case a.variable != "":
		if err := json.Unmarshal(vars[a.variable], &value); err != nil {
			return "", false
		}
	case a.literal.kind == gqlKindValue && a.literal.quoted:
		value = a.literal.text
	default:
		return "", false
	}
	return value, true
}

// repoPart resolves the argument to a repository owner or name, in the shape
// validRepoPart admits.
func (a gqlArgument) repoPart(vars map[string]json.RawMessage) (string, bool) {
	value, ok := a.stringValue(vars)
	return value, ok && validRepoPart(value)
}

// nodeID resolves the argument to a node id, in the shape validNodeID admits.
func (a gqlArgument) nodeID(vars map[string]json.RawMessage) (string, bool) {
	value, ok := a.stringValue(vars)
	return value, ok && validNodeID(value)
}

// objectNumber resolves the argument to a pull request or issue number: an
// integer variable or an integer literal, positive.
func (a gqlArgument) objectNumber(vars map[string]json.RawMessage) (int, bool) {
	var text string
	switch {
	case a.variable != "":
		text = strings.TrimSpace(string(vars[a.variable]))
	case a.literal.kind == gqlKindValue && !a.literal.quoted:
		text = a.literal.text
	default:
		return 0, false
	}
	n, err := strconv.Atoi(text)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// maxRepoPartLength is past any owner or repository name GitHub admits (39 and
// 100 characters).
const maxRepoPartLength = 100

// validRepoPart reports whether s has the shape of a repository's owner or
// name: letters, digits, '.', '_' and '-', and not a path's "." or "..". The
// value goes into the path of a request the injector makes with the run's
// credential, so nothing that could address another path is admitted.
func validRepoPart(s string) bool {
	if s == "" || len(s) > maxRepoPartLength || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isNameContinue(c) && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// gqlAddressed is one object a query addresses by number, before its arguments
// are resolved against the request's variables.
type gqlAddressed struct {
	owner, name, number gqlArgument
}

// gqlReadOperation is one operation definition as the read walk sees it: which
// kind it is, its name, and what its top level addresses, by number under a
// repository (reads) and by node id (nodes). Only a query's are collected, and
// the two hold at most maxAddressedReads together.
type gqlReadOperation struct {
	typ   string
	name  string
	reads []gqlAddressed
	nodes []gqlArgument
}

// selectReadOperation resolves which operation of a document runs, by the rule
// selectOperation applies to the mutation reader's.
func selectReadOperation(ops []gqlReadOperation, operationName string) (gqlReadOperation, bool) {
	if operationName != "" {
		for _, op := range ops {
			if op.name == operationName {
				return op, true
			}
		}
		return gqlReadOperation{}, false
	}
	if len(ops) == 1 {
		return ops[0], true
	}
	return gqlReadOperation{}, false
}

// parseReadOperations walks a document's definitions for what each query
// addresses. ok is false for anything it cannot walk exactly, which names
// nothing.
func parseReadOperations(doc string) ([]gqlReadOperation, bool) {
	sc := &gqlScanner{src: doc}
	var ops []gqlReadOperation
	for {
		tok, more := sc.next()
		if !more {
			break
		}
		switch {
		case tok.punct("{"):
			// The shorthand form: a bare selection set is an anonymous query.
			op := gqlReadOperation{typ: gqlOperationQuery}
			if !readQuerySelection(sc, &op) {
				return nil, false
			}
			ops = append(ops, op)
		case tok.name("fragment"):
			if _, _, walked := parseFragmentDefinition(sc); !walked {
				return nil, false
			}
		case tok.name(gqlOperationQuery), tok.name(gqlOperationMutation), tok.name(gqlOperationSubscription):
			op, walked := parseReadOperation(sc, tok.text)
			if !walked {
				return nil, false
			}
			ops = append(ops, op)
		default:
			return nil, false
		}
	}
	if sc.bad {
		return nil, false
	}
	return ops, true
}

// parseReadOperation reads one operation definition with its keyword already
// consumed. A query's selection is walked for what it addresses; any other
// operation's is skipped whole.
func parseReadOperation(sc *gqlScanner, typ string) (gqlReadOperation, bool) {
	op := gqlReadOperation{typ: typ}
	if tok, ok := sc.peek(); ok && tok.kind == gqlKindName {
		sc.next()
		op.name = tok.text
	}
	if tok, ok := sc.peek(); ok && tok.punct("(") {
		sc.next()
		if !sc.skipBalanced("(", ")") {
			return op, false
		}
	}
	if !skipDirectives(sc) {
		return op, false
	}
	tok, ok := sc.next()
	if !ok || !tok.punct("{") {
		return op, false
	}
	if typ != gqlOperationQuery {
		return op, sc.skipBalanced("{", "}")
	}
	return op, readQuerySelection(sc, &op)
}

// addressed reports how many objects op addresses so far, by number and by
// node id together.
func (op *gqlReadOperation) addressed() int {
	return len(op.reads) + len(op.nodes)
}

// readQuerySelection walks a query's top-level selection set, its opening
// brace already consumed, into op: each repository it selects by owner and
// name with the objects addressed under it, and each node it selects by id.
func readQuerySelection(sc *gqlScanner, op *gqlReadOperation) bool {
	for {
		field, args, selects, done, ok := readSelectedField(sc, readTopLevelArguments)
		if !ok {
			return false
		}
		if done {
			return true
		}
		if field == readNodeField {
			if id := args[readIDArgument]; id != (gqlArgument{}) && op.addressed() < maxAddressedReads {
				op.nodes = append(op.nodes, id)
			}
			if selects && !sc.skipBalanced("{", "}") {
				return false
			}
			continue
		}
		if !selects {
			continue
		}
		owner, name := args[readOwnerArgument], args[readNameArgument]
		if field != readRepositoryField || owner == (gqlArgument{}) || name == (gqlArgument{}) {
			if !sc.skipBalanced("{", "}") {
				return false
			}
			continue
		}
		numbers, walked := readRepositorySelection(sc)
		if !walked {
			return false
		}
		for _, number := range numbers {
			if op.addressed() >= maxAddressedReads {
				break
			}
			op.reads = append(op.reads, gqlAddressed{owner: owner, name: name, number: number})
		}
	}
}

// readRepositorySelection walks a repository's selection set, its opening
// brace already consumed, for the number of each pull request or issue it
// selects directly. Everything under those fields is skipped.
func readRepositorySelection(sc *gqlScanner) ([]gqlArgument, bool) {
	var numbers []gqlArgument
	for {
		_, args, selects, done, ok := readSelectedField(sc, readObjectArguments)
		if !ok {
			return nil, false
		}
		if done {
			return numbers, true
		}
		if number := args[readNumberArgument]; number != (gqlArgument{}) && len(numbers) < maxAddressedReads {
			numbers = append(numbers, number)
		}
		if selects && !sc.skipBalanced("{", "}") {
			return nil, false
		}
	}
}

// readSelectedField consumes one member of a selection set: a field with its
// alias, arguments and directives, or a fragment, which is skipped whole and
// reported as a field with no name. done reports the set's closing brace.
// selects reports that the field opens a selection set, whose opening brace is
// consumed and whose contents are the caller's to walk or skip.
//
// Arguments are read only for a field wanted names, and only the ones it lists
// for that field; every other field's are skipped as balanced punctuation.
func readSelectedField(sc *gqlScanner, wanted map[string][]string) (field string, args map[string]gqlArgument, selects, done, ok bool) {
	tok, more := sc.next()
	if !more {
		return "", nil, false, false, false
	}
	if tok.punct("}") {
		return "", nil, false, true, true
	}
	if tok.punct("...") {
		return "", nil, false, false, skipFragmentSelection(sc)
	}
	if tok.kind != gqlKindName {
		return "", nil, false, false, false
	}
	field = tok.text
	if next, more := sc.peek(); more && next.punct(":") {
		sc.next()
		real, more := sc.next()
		if !more || real.kind != gqlKindName {
			return "", nil, false, false, false
		}
		field = real.text
	}
	if next, more := sc.peek(); more && next.punct("(") {
		sc.next()
		if want, read := wanted[field]; read {
			if args, ok = readArguments(sc, want); !ok {
				return "", nil, false, false, false
			}
		} else if !sc.skipBalanced("(", ")") {
			return "", nil, false, false, false
		}
	}
	if !skipDirectives(sc) {
		return "", nil, false, false, false
	}
	if next, more := sc.peek(); more && next.punct("{") {
		sc.next()
		selects = true
	}
	return field, args, selects, false, true
}

// skipFragmentSelection consumes what follows a `...` in a selection set: a
// named spread, or an inline fragment with its selection. Neither is read: a
// repository or an object selected through a fragment names nothing here.
func skipFragmentSelection(sc *gqlScanner) bool {
	if tok, ok := sc.peek(); ok && tok.name("on") {
		sc.next()
		if cond, more := sc.next(); !more || cond.kind != gqlKindName {
			return false
		}
	} else if ok && tok.kind == gqlKindName {
		sc.next()
		return skipDirectives(sc)
	}
	if !skipDirectives(sc) {
		return false
	}
	if tok, ok := sc.peek(); ok && tok.punct("{") {
		sc.next()
		return sc.skipBalanced("{", "}")
	}
	return true
}

// readArguments reads an argument list, its opening parenthesis already
// consumed, keeping the ones named in want whose value is a variable or a
// scalar literal. A list or an object value is skipped as balanced
// punctuation, and an enum, a boolean or a null is passed over.
func readArguments(sc *gqlScanner, want []string) (map[string]gqlArgument, bool) {
	args := map[string]gqlArgument{}
	for {
		tok, more := sc.next()
		if !more {
			return nil, false
		}
		if tok.punct(")") {
			return args, true
		}
		if tok.kind != gqlKindName {
			return nil, false
		}
		if colon, more := sc.next(); !more || !colon.punct(":") {
			return nil, false
		}
		value, more := sc.next()
		if !more {
			return nil, false
		}
		var arg gqlArgument
		switch {
		case value.punct("$"):
			name, more := sc.next()
			if !more || name.kind != gqlKindName {
				return nil, false
			}
			arg.variable = name.text
		case value.punct("["):
			if !sc.skipBalanced("[", "]") {
				return nil, false
			}
		case value.punct("{"):
			if !sc.skipBalanced("{", "}") {
				return nil, false
			}
		case value.kind == gqlKindValue:
			arg.literal = value
		case value.kind == gqlKindName:
		default:
			return nil, false
		}
		for _, name := range want {
			if tok.text == name {
				args[name] = arg
			}
		}
	}
}
