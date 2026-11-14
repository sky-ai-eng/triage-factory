package ghinjector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// The repository gate: a request that names a repository outside the run's
// authorized set is refused before it is forwarded. It applies where the
// credential reaches further than the run does, which is every credential but
// a multi-mode App org's, whose token GitHub mints for the set already; the
// RepositorySource says which this is.
//
// A request names a repository in two shapes: a REST path under
// /repos/{owner}/{repo} (ghwrite.NamedRepository), and a GraphQL query's
// `repository(owner:, name:)` at its top level (ghwrite.ParseGraphQLRead). A
// name is matched by GitHub's id for it rather than by its spelling: one the
// set already names is in it, and any other is asked of GitHub once per run,
// with the run's credential, and is in the set when GitHub answers with the id
// of a repository the set holds, so a renamed repository is reached under its
// old name and its new one. That answer also says whether the repository is
// public, and a read of a public repository outside the set is forwarded, as
// an App's token would read it. A REST write to one is refused. A name GitHub
// answers 404 for is forwarded, so the agent gets GitHub's own not-found.
//
// Anything the gate cannot settle refuses: a set that cannot be read, an
// answer from GitHub that cannot be read, a name in a shape no repository
// has, and a query that mentions a repository but cannot be read. A wrong
// refusal costs a retry.
//
// It is courtesy isolation, not a boundary. A request can reach a repository
// it does not name: a GraphQL lookup by node id, a search, a link followed out
// of an allowed object (a cross-reference, a fork, an owner's repositories), a
// repository selected below the top level or through a fragment, and the REST
// endpoints that name no repository in their path. A GraphQL mutation names
// its object by node id, so it is not refused here; `gh`'s own write commands
// read the object by repository first, which is refused for a private
// repository outside the set and forwarded for a public one.

// AuthorizedRepository is one repository of a run's authorized set: the
// owner/repo the set names it by, and GitHub's id for it, empty when the set
// does not know it.
type AuthorizedRepository struct {
	Name string
	ID   string
}

// RepositorySource reads the run's authorized repository set, once for each
// request that names a repository, so a set that widens mid-run (a
// `workspace add`) is in force on the next request. gated is false when no
// gate applies, because the credential is confined to the set already; repos
// is not read then. An error refuses the request.
type RepositorySource func(ctx context.Context) (repos []AuthorizedRepository, gated bool, err error)

// DeniedRepository is one request the repository gate refused.
type DeniedRepository struct {
	// Repository is the repository refused, as the request spelled it. Zero
	// for a request whose repository could not be read at all.
	Repository ghwrite.RepoRef
	// Identity is what GitHub answered the injector for the repository: its
	// id and current name. Zero when the injector did not ask, or GitHub did
	// not answer.
	Identity domain.GitHubIdentity
	// Unresolved reports a refusal because the gate could not tell whether the
	// repository is in the set, rather than because it is not.
	Unresolved bool
	// Method and Path are the request's, the path as an audit row names it
	// (auditPath).
	Method string
	Path   string
}

// Name is the repository's name for a denial row: the one GitHub answered
// with when it answered, else the request's spelling.
func (d DeniedRepository) Name() string {
	if d.Identity.Repository != "" {
		return d.Identity.Repository
	}
	if d.Repository == (ghwrite.RepoRef{}) {
		return ""
	}
	return d.Repository.String()
}

// repositoryRefusalBody is the gate's synthesized response, shaped like the
// write gate's (refusalBody): `message` is where gh reads an API error, and
// the structured member carries the parts a model acts on.
type repositoryRefusalBody struct {
	Message       string              `json:"message"`
	TriageFactory repositoryRefusalTF `json:"triage_factory"`
}

type repositoryRefusalTF struct {
	// Repository is the repository refused, as the request spelled it.
	Repository string `json:"repository,omitempty"`
	// Reason is repositoryNotInRun or repositoryUnresolved.
	Reason   string `json:"reason"`
	Policy   string `json:"policy"`
	NextStep string `json:"next_step"`
}

// The two reasons a refusal gives the agent.
const (
	repositoryNotInRun   = "repository_not_in_run"
	repositoryUnresolved = "repository_unresolved"
)

// repositoryVerdict is the gate's decision on one named repository.
type repositoryVerdict struct {
	allowed  bool
	identity domain.GitHubIdentity
	// public is set when GitHub answered that the repository is public.
	public     bool
	unresolved bool
}

// refusedRepository decides one request under the repository gate and, when
// the decision is to refuse, records the denial and answers it. It reports
// whether the response has been written, in which case the caller must not
// forward. reads is what the GraphQL capture read out of a query.
func (s *Server) refusedRepository(w http.ResponseWriter, r *http.Request, reads ghwrite.AddressedReads) bool {
	if s.cfg.Repositories == nil {
		return false
	}
	named, write, unread := namedRepositories(r, reads)
	if len(named) == 0 && !unread {
		return false
	}
	repos, gated, err := s.cfg.Repositories(r.Context())
	if err == nil && !gated {
		return false
	}
	if err != nil {
		injectorLog.Warn("read the run's repositories failed; refusing a gh request that names one",
			"conversation", s.cfg.ConversationID, "error", err)
		first := ghwrite.RepoRef{}
		if len(named) > 0 {
			first = named[0]
		}
		s.refuseRepository(w, r, first, repositoryVerdict{unresolved: true}, write)
		return true
	}
	if unread {
		s.refuseRepository(w, r, ghwrite.RepoRef{}, repositoryVerdict{unresolved: true}, write)
		return true
	}
	set := newRepositorySet(repos)
	// The agent's request waits on the lookups, so one bound covers every name
	// it carries.
	ctx, cancel := context.WithTimeout(r.Context(), identityReadTimeout)
	defer cancel()
	for _, ref := range named {
		if v := s.decideRepository(ctx, set, ref, write); !v.allowed {
			s.refuseRepository(w, r, ref, v, write)
			return true
		}
	}
	return false
}

// namedRepositories is what one request names: the repository a REST path
// addresses, or the ones a GraphQL query selects at its top level, and
// whether the request is a REST write. unread reports a GraphQL query naming
// a repository whose owner or name could not be read.
func namedRepositories(r *http.Request, reads ghwrite.AddressedReads) (named []ghwrite.RepoRef, write, unread bool) {
	if isGraphQLPath(r.URL.Path) {
		return reads.Repositories, false, reads.UnreadRepository
	}
	ref, ok := ghwrite.NamedRepository(r.URL.Path)
	if !ok {
		return nil, false, false
	}
	return []ghwrite.RepoRef{ref}, writeMethods[r.Method], false
}

// decideRepository settles one named repository against the set: allowed when
// the set names it, or when GitHub answers the name with a repository the set
// holds, a public one a read names, or a 404.
func (s *Server) decideRepository(ctx context.Context, set repositorySet, ref ghwrite.RepoRef, write bool) repositoryVerdict {
	if !ref.Valid() {
		return repositoryVerdict{unresolved: true}
	}
	if set.names[ref.Key()] {
		return repositoryVerdict{allowed: true}
	}
	id, public, err := s.identities.RepositoryVisibility(ctx, s.reads, ref.Owner, ref.Repo)
	switch {
	case ghclient.IsHTTP404(err):
		// No repository of this name the credential can see, so forwarding
		// reaches none either, and GitHub's answer is the honest one.
		return repositoryVerdict{allowed: true}
	case err != nil:
		injectorLog.Warn("read what GitHub calls a named repository failed; refusing",
			"conversation", s.cfg.ConversationID, "repo", ref.String(), "error", err)
		return repositoryVerdict{unresolved: true}
	case set.ids[id.RepositoryID]:
		return repositoryVerdict{allowed: true, identity: id}
	case public && !write:
		return repositoryVerdict{allowed: true, identity: id, public: true}
	}
	return repositoryVerdict{identity: id, public: public}
}

// refuseRepository records one refusal and answers it.
func (s *Server) refuseRepository(w http.ResponseWriter, r *http.Request, ref ghwrite.RepoRef, v repositoryVerdict, write bool) {
	denial := DeniedRepository{
		Repository: ref,
		Identity:   v.identity,
		Unresolved: v.unresolved,
		Method:     r.Method,
		Path:       auditPath(r),
	}
	injectorLog.Info("refused a gh-channel request naming a repository outside the run",
		"conversation", s.cfg.ConversationID, "repo", denial.Name(), "unresolved", v.unresolved,
		"method", r.Method, "path", denial.Path)
	if s.cfg.DenyRepository != nil {
		s.cfg.DenyRepository(r.Context(), denial)
	}

	refusal := explainRepositoryRefusal(ref, v, write)
	body, err := json.Marshal(repositoryRefusalBody{
		Message:       refusal.Policy + " " + refusal.NextStep,
		TriageFactory: refusal,
	})
	if err != nil {
		body = []byte(`{"message":"This request was refused by Triage Factory."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(refusalStatus)
	_, _ = w.Write(body)
}

// explainRepositoryRefusal renders a refusal for the agent that hit it,
// worded like the `exec gh` verbs' own repository refusals.
func explainRepositoryRefusal(ref ghwrite.RepoRef, v repositoryVerdict, write bool) repositoryRefusalTF {
	name := ref.String()
	out := repositoryRefusalTF{Reason: repositoryNotInRun}
	if ref != (ghwrite.RepoRef{}) {
		out.Repository = name
	}
	switch {
	case ref == (ghwrite.RepoRef{}):
		out.Reason = repositoryUnresolved
		out.Policy = "Triage Factory could not read which repository this gh request names, so it refused it: " +
			"gh requests may name only the repositories this run works in."
		out.NextStep = "Name each repository by its owner and name, given as string literals or as variables, then retry."
		return out
	case v.unresolved && !ref.Valid():
		out.Reason = repositoryUnresolved
		out.Policy = "repo " + name + " is not a repository name, so Triage Factory refused this gh request."
		out.NextStep = "Name the repository as owner/repo, then retry."
		return out
	case v.unresolved:
		out.Reason = repositoryUnresolved
		out.Policy = "Triage Factory could not tell whether repo " + name + " is one this run works in, " +
			"so it refused this gh request."
		out.NextStep = "Retry it."
		return out
	case v.public && write:
		out.Policy = "repo " + name + " is not one this run works in. It is public, so this run can read it, " +
			"but Triage Factory refuses gh writes to it."
	default:
		out.Policy = "repo " + name + " is not one this run works in, so Triage Factory refuses gh requests that name it."
	}
	out.NextStep = "If this team tracks it, run 'workspace add " + name + "' for its default branch, or " +
		"'workspace add " + name + " --pr <N>' to check out a pull request head, then retry; a repo the team " +
		"does not track must first be added as a tracked repo in Settings by a team admin."
	return out
}

// repositorySet is the run's authorized set as the gate matches it: by name,
// case folded, and by GitHub's id.
type repositorySet struct {
	names map[string]bool
	ids   map[string]bool
}

func newRepositorySet(repos []AuthorizedRepository) repositorySet {
	set := repositorySet{names: map[string]bool{}, ids: map[string]bool{}}
	for _, repo := range repos {
		if repo.Name != "" {
			set.names[strings.ToLower(repo.Name)] = true
		}
		if repo.ID != "" {
			set.ids[repo.ID] = true
		}
	}
	return set
}
