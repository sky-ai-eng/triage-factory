// The native runtime's pre-dispatch matcher on three `gh` commands, which get
// two different kinds of answer for two different reasons.
//
// Posting a review and creating a repository are REFUSED, and refused again —
// for real — at the credential injector every request on this channel passes
// through (internal/ghinjector, keyed on the shared classifier in
// internal/ghwrite). For those two this file is early UX in front of an
// enforced policy: the same answer arrives better from a matcher than from a
// proxy, since a refusal here costs one tool call and names the redirect in the
// model's own terms, where the injector's arrives as a 403 partway through a
// `gh` invocation, after the model has committed to a plan built around the
// command succeeding.
//
// Merging gets a QUESTION, and nothing downstream enforces an answer. Refusing
// it outright would mean deciding an intent the runtime cannot know — some
// missions are for landing work — and unlike the other two there is no
// dominating alternative to redirect to. So the merge attempt is interrupted
// once and asked to quote the line of its mission that authorizes it; a model
// that re-issues proceeds. The value is the forced restatement of intent
// against the mission, not the obstruction: a model asked to quote its
// authorization and unable to find one frequently abandons the action.
//
// That leaves three limitations, and which of them matter now depends on which
// answer you are looking at:
//
//   - Injection-blind. The same hostile context that induced the action will
//     satisfy a self-attestation just as easily.
//   - Matcher-evadable. Any construction this matcher does not recognize —
//     `gh api graphql` with an inline mutation being the obvious one — passes
//     straight through, as does any client that is not `bash` and any run on
//     the SDK runtime, which has no matcher seam at all.
//   - Self-attestation only. Nothing checks the answer to the merge question.
//
// For the two refusals none of that is load-bearing: the injector catches what
// this misses. For the merge question all three stand, and the question is the
// only control there is. What every one of these texts must be is TRUE — a
// model that discovers a stated rule is false has cause to doubt the rest — so
// the two refusals say "refused" because the act genuinely does not happen, and
// the merge question asks rather than claiming a refusal it cannot deliver.

package delegate

import (
	"context"
	"path"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewRefusal answers `gh pr review` every time. It is deliberately plain
// text rather than a marked note: the model should read it as an ordinary
// tool error, and re-run the call it should have made. Repetition is not
// badgering when the answer never changes and the redirect is actionable.
//
// The verbs it names are the ones the native harness prompt documents; the
// two must stay in step, since a redirect to a command that does not exist is
// worse than no redirect at all.
//
// The justification is verb dominance, and only that. It used to also say such
// a review was invisible to Triage Factory, which stopped being true when the
// channel's writes started landing audit rows — the review verbs win because a
// review posted raw cannot anchor to a line, carry a severity badge, be revised
// as a draft, or go through the approval path, not because nobody would know.
const reviewRefusal = "This command was not run, and the same call made another way will be refused too. " +
	"`gh pr review` cannot attach comments to specific lines, and it posts straight to GitHub — " +
	"no draft to revise, no severity badges, no approval path. " +
	"Use `tfac gh pr start-review`, then `add-review-comment` for each finding, then `finalize-review`."

// mergeGateTag opens the merge question and is how a later call recognizes
// that it has already been put. The identity lives in the tag, not in the
// prose, so the wording can be changed without silently re-arming the question
// on every conversation mid-flight.
const mergeGateTag = `<system-note kind="merge-gate">`

// mergeGateQuestion is what a merge attempt is asked before it runs.
//
// It points at the opening message rather than at "what the user asked for":
// there is no user present, and a model's sense of what was asked is a vibe,
// while the opening turn is a specific artifact in its context that
// demonstrably does or does not carry the instruction — and demonstrably does
// not carry one that arrived in a PR comment.
//
// Unlike the two refusals, this text promises nothing it cannot deliver: it
// says the command was not run, which is true, and it does not say a re-issue
// will fail, because it will not.
const mergeGateQuestion = mergeGateTag + "\n" +
	"This command was not run. Merging is not reversible and no human is watching this run. " +
	"Before you do it: does your mission — the opening message of this conversation — actually tell you to merge? " +
	"If it does, quote that line before merging. If you cannot find one, do not merge — finish your work and say " +
	"in your final message that you believe the branch is ready to merge, and let a human do it.\n" +
	"</system-note>"

// repoCreateRefusal answers `gh repo create` every time.
//
// It names no replacement command, unlike the review refusal, because there is
// no `tfac exec gh repo` verb to name — and the file comment above holds that a
// redirect to a command that does not exist is worse than no redirect at all.
// So it redirects to the only thing that actually works: saying so, and letting
// a human do it.
//
// TODO(TFAC-793): when a governed repo-provisioning verb exists, name it here.
// That ticket also carries the prior question of whether an agent should be able
// to ask for a repository at all, in which case this text stands permanently.
const repoCreateRefusal = "This command was not run, and the same call made another way will be refused too. " +
	"Creating repositories is not something a run does — Triage Factory scopes polling, task routing, " +
	"and this run's own credential to a set of repositories chosen before the run started, and a new one " +
	"belongs to none of it. " +
	"If your mission genuinely needs a repository that does not exist, say so in your final message and stop there."

// ghCommandGate builds the native loop's BeforeToolCall hook.
//
// A denial is a synthetic is_error result the model reads in-band, so neither
// answer ever ends a run: the refusals redirect and the question is answerable
// by re-issuing. Nothing but the three matched shapes is looked at, so a run
// that attempts none of them sees no evidence this exists — and only a matched
// merge pays for the transcript read.
func (s *Spawner) ghCommandGate(orgID, conversationID string) func(context.Context, domain.ToolCall) string {
	return func(ctx context.Context, call domain.ToolCall) string {
		switch classifyGHCommand(bashCommand(call)) {
		case ghActionReview:
			return reviewRefusal
		case ghActionRepoCreate:
			return repoCreateRefusal
		case ghActionMerge:
			if s.mergeAlreadyQuestioned(ctx, orgID, conversationID) {
				return ""
			}
			return mergeGateQuestion
		}
		return ""
	}
}

// mergeAlreadyQuestioned reads the question's state off the transcript.
//
// A read failure asks again, which is the cheap way to be wrong: the model
// answers a question it has already answered and re-issues, costing one turn.
// Staying quiet would instead let the one call this exists for through on the
// strength of a failed read.
func (s *Spawner) mergeAlreadyQuestioned(ctx context.Context, orgID, conversationID string) bool {
	rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read transcript for the merge question failed; asking again", "conversation", conversationID, "error", err)
		return false
	}
	return askedAboutMergeAlready(rows)
}

// askedAboutMergeAlready reports whether the merge question has been put with
// no human input since.
//
// The state is the transcript, never process state, so a crash and a re-claim
// behave identically to the engagement that asked. Walking back from the
// newest row: the gate's own note means the question stands and the model has
// already restated its intent against it, so asking again would be
// obstruction rather than reconsideration. Genuine human input newer than the
// note means a person spoke since — the premise changed, and the same question
// about that new work has not been put. Everything else the system wrote on
// the agent's behalf speaks for no one and is skipped, using the same closed
// human set the engine's drain keys on.
//
// The note check comes first, because the row it looks for is system-authored
// and the human filter below would skip it.
func askedAboutMergeAlready(rows []domain.Message) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if isMergeGateNote(r) {
			return true
		}
		if r.Role == "user" && agentloop.IsHumanInput(r) {
			return false
		}
	}
	return false
}

// isMergeGateNote reports whether a row is one this gate itself wrote.
//
// The tag has to open an is_error tool result, not merely appear somewhere in
// a row: it is a string that exists in this repository and in every transcript
// the note lands in, so a run that greps its own source, cats a log, or reads
// a conversation would otherwise hand itself a row that spends the question it
// was never asked. Anchoring it leaves everything after the tag free to
// change, which is the whole reason the identity is a tag and not the prose.
func isMergeGateNote(r domain.Message) bool {
	return r.Role == "tool" && r.IsError && strings.HasPrefix(r.Content, mergeGateTag)
}

// bashCommand extracts the shell command a call would run, or "" for any call
// that is not a shell at all.
func bashCommand(call domain.ToolCall) string {
	if call.Name != "bash" {
		return ""
	}
	cmd, _ := call.Input["command"].(string)
	return cmd
}

// ghAction is the gated action a shell command was recognized as reaching for.
type ghAction int

const (
	ghActionNone ghAction = iota
	ghActionMerge
	ghActionReview
	ghActionRepoCreate
)

// classifyGHCommand reports which gated action, if any, a shell command
// reaches for.
//
// The shape matched is a `gh` invocation whose subcommand chain is `pr merge`
// or `pr review`, plus a `gh api` call naming a `/merge` endpoint. Leading
// environment assignments, an absolute path and flags interleaved with the
// subcommand chain are tolerated because they are what a model writes; the
// scan crosses shell separators for the same reason, since `cd /work && gh pr
// merge 7` is one bash call. It is not exhaustive and is not trying to be —
// see the file comment.
func classifyGHCommand(command string) ghAction {
	for _, words := range shellSegments(command) {
		if a := classifyGHSegment(words); a != ghActionNone {
			return a
		}
	}
	return ghActionNone
}

// classifyGHSegment classifies one simple command.
func classifyGHSegment(words []string) ghAction {
	i := 0
	for i < len(words) && isEnvAssignment(words[i]) {
		i++
	}
	// `env FOO=bar gh …` reaches the same place by a different spelling.
	if i < len(words) && words[i] == "env" {
		i++
		for i < len(words) && isEnvAssignment(words[i]) {
			i++
		}
	}
	if i >= len(words) || path.Base(words[i]) != "gh" {
		return ghActionNone
	}
	args := words[i+1:]
	chain := subcommandChain(args)
	switch {
	case len(chain) == 0:
		return ghActionNone
	case chain[0] == "api":
		if namesMergeEndpoint(args) {
			return ghActionMerge
		}
		if namesRepoCreate(args) {
			return ghActionRepoCreate
		}
	case chain[0] == "pr" && len(chain) > 1:
		switch chain[1] {
		case "merge":
			return ghActionMerge
		case "review":
			return ghActionReview
		}
	case chain[0] == "repo" && len(chain) > 1:
		if chain[1] == "create" {
			return ghActionRepoCreate
		}
	}
	return ghActionNone
}

// subcommandChain returns the first two positional words of a gh invocation —
// its subcommand chain.
//
// A word following a bare flag is skipped as that flag's value. Which flags
// take one is not knowable here, and reading the value of `--repo owner/repo`
// as a subcommand would miss the shape it appears in; skipping one word too
// many can only cost a match, never invent one.
func subcommandChain(words []string) []string {
	var chain []string
	for j := 0; j < len(words) && len(chain) < 2; j++ {
		w := words[j]
		if strings.HasPrefix(w, "-") && w != "-" {
			if !strings.Contains(w, "=") {
				j++
			}
			continue
		}
		chain = append(chain, w)
	}
	return chain
}

// namesMergeEndpoint reports whether any argument names a REST path whose
// last resource is `merge`.
//
// A whole path segment, not a substring: `/merges` merges two branches and
// `/merge-upstream` syncs a fork, neither of which is the irreversible thing
// this gate is about, and a field value that happens to contain the word is
// not an endpoint at all.
func namesMergeEndpoint(words []string) bool {
	for _, w := range words {
		p, _, _ := strings.Cut(w, "?")
		p, _, _ = strings.Cut(p, "#")
		for i, seg := range strings.Split(p, "/") {
			// i > 0 keeps this to a path: the segment must be reached
			// through a `/`, so a bare word `merge` is not an endpoint.
			if i > 0 && seg == "merge" {
				return true
			}
		}
	}
	return false
}

// repoCreateMutations are the two ways GitHub's schema makes a repository. Both
// are what `gh repo create` itself sends, so a hand-written call is reaching for
// the same act by a spelling the subcommand match does not see.
var repoCreateMutations = []string{"createRepository", "cloneTemplateRepository"}

// namesRepoCreate reports whether a `gh api` call reaches for repository
// creation — the REST collections that mint one, or a GraphQL document naming a
// mutation that does.
//
// Unlike the merge endpoint, both shapes here are addressed by paths that are
// ALSO ordinary reads: `/user/repos` lists your repositories, and a schema query
// may mention the mutation by name. Refusing those would make the control cost
// more than it saves, so this half reads the method too — which for `gh api`
// means the explicit flag when there is one and otherwise whether any field was
// passed, since that is when gh switches from GET to POST on its own.
//
// Still substring-shaped and still evadable, which is what an accident control
// is; see the file comment.
func namesRepoCreate(words []string) bool {
	if !apiPosts(words) {
		return false
	}
	for _, w := range words {
		if namesRepoCreateMutation(w) || mintsRepoByPath(w) {
			return true
		}
	}
	return false
}

// namesRepoCreateMutation reports whether a word is the GraphQL `query` field
// carrying a document that makes a repository.
//
// Scoped to that one field for the reason namesMergeEndpoint scopes itself to
// path segments: a field value that happens to contain the word is not the act.
// `gh api …/comments -f body='…createRepository…'` is a comment about repository
// creation, and refusing it would be refusing ordinary work — a live risk in
// this repository in particular, where a pull request discussing this very gate
// contains those identifiers as prose.
//
// A document passed by file (`-F query=@doc.graphql`, `--input doc.json`) is
// invisible here and always was. It no longer needs chasing: the injector reads
// the request body itself, so a document this scan cannot see is refused where
// it actually arrives.
func namesRepoCreateMutation(word string) bool {
	for _, prefix := range []string{"--field=", "--raw-field="} {
		word = strings.TrimPrefix(word, prefix)
	}
	document, isQuery := strings.CutPrefix(word, "query=")
	if !isQuery {
		return false
	}
	for _, name := range repoCreateMutations {
		if strings.Contains(document, name) {
			return true
		}
	}
	return false
}

// mintsRepoByPath reports whether a word is a REST path that creates a
// repository. Three do: the two collections a repository can be posted to, and
// the template-instantiation endpoint, which addresses the TEMPLATE and makes a
// new repository out of it.
func mintsRepoByPath(word string) bool {
	p, _, _ := strings.Cut(word, "?")
	p, _, _ = strings.Cut(p, "#")
	segs := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	// A repo-scoped path also ends in a segment pair, so the leading collection
	// is what tells /user/repos apart from repos/{owner}/{repo}.
	case len(segs) == 2 && segs[0] == "user" && segs[1] == "repos":
		return true
	case len(segs) == 3 && segs[0] == "orgs" && segs[2] == "repos":
		return true
	case len(segs) == 4 && segs[0] == "repos" && segs[3] == "generate":
		return true
	}
	return false
}

// apiPosts reports whether a `gh api` invocation issues a POST: the method flag
// says so, or no method flag is present and something was passed that makes gh
// switch off GET on its own — a field, or a body file.
//
// `--input` counts for the same reason a field does. gh documents its own
// ruleset-creation example as `gh api …/rulesets --input file.json` with no
// method flag, which only works because supplying a body implies the POST.
//
// An explicit non-POST method loses, even alongside a body — `-X GET -f q=x`
// sends a GET with a query string, and refusing that would be refusing a read.
//
// A REPEATED method flag resolves to the last one, which is what gh's own flag
// parsing does with it. Reading the first instead would disagree with the
// command that actually runs in both directions: `-X GET … -X POST` sends a
// write this would call a read, and `-X POST … -X GET` sends a read this would
// call a write. Neither spelling is one a model writes on purpose, and the
// injector decides the real request either way; agreeing with gh is just the
// cheapest way for this matcher to describe what it claims to describe.
func apiPosts(words []string) bool {
	body := false
	method, methodSet := "", false
	setMethod := func(value string) {
		method, methodSet = value, true
	}
	for i, w := range words {
		switch {
		case w == "-X" || w == "--method":
			// A dangling flag with no value is a command gh rejects outright;
			// leaving methodSet alone lets the body test below decide, which
			// errs toward recognizing the act.
			if i+1 < len(words) {
				setMethod(words[i+1])
			}
		case strings.HasPrefix(w, "--method="):
			setMethod(strings.TrimPrefix(w, "--method="))
		case strings.HasPrefix(w, "-X") && len(w) > 2:
			setMethod(w[2:])
		case w == "-f", w == "-F", w == "--field", w == "--raw-field", w == "--input",
			strings.HasPrefix(w, "--field="), strings.HasPrefix(w, "--raw-field="),
			strings.HasPrefix(w, "--input="):
			body = true
		}
	}
	if methodSet {
		return strings.EqualFold(method, "POST")
	}
	return body
}

// isEnvAssignment reports whether a word is a leading `NAME=VALUE` assignment
// rather than the command itself.
func isEnvAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// shellSegments splits a command line into simple commands, each already
// word-split with quotes stripped.
//
// It is a reader, not a shell: quoting decides where words end, unquoted
// separators decide where commands end, and nothing is expanded. Text inside
// quotes therefore stays one word, which is what keeps `--body "gh pr merge is
// refused"` from reading as an invocation — and, symmetrically, is why
// `bash -c "gh pr merge 7"` is not seen as one.
func shellSegments(command string) [][]string {
	var (
		segs  [][]string
		words []string
		cur   []rune
		open  bool // a word is being built — possibly an empty one, from ""
		quote rune
	)
	endWord := func() {
		if open {
			words = append(words, string(cur))
			cur = cur[:0]
			open = false
		}
	}
	endSegment := func() {
		endWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}

	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if quote != 0 {
			switch {
			case c == quote:
				quote = 0
			case quote == '"' && c == '\\' && i+1 < len(rs):
				i++
				cur = append(cur, rs[i])
			default:
				cur = append(cur, c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			open = true
		case '\\':
			if i+1 < len(rs) {
				i++
				// A line continuation joins what follows onto this command;
				// any other escaped rune is a literal character in the word.
				if rs[i] != '\n' {
					cur = append(cur, rs[i])
					open = true
				}
			}
		case ' ', '\t', '\r':
			endWord()
		case ';', '&', '|', '\n', '(', ')', '{', '}', '`':
			endSegment()
		default:
			cur = append(cur, c)
			open = true
		}
	}
	endSegment()
	return segs
}
