package routing

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// jiraRef builds one status ref the way a rule armed through the API carries
// it: the id, which is the identity, plus the display name resolved for it.
// The id is derived from the name so a test can name a status once and get the
// same ref everywhere it appears.
func jiraRef(name string) domain.JiraStatusRef {
	return domain.JiraStatusRef{ID: "st-" + name, Name: name}
}

func jiraRefs(names ...string) []domain.JiraStatusRef {
	refs := make([]domain.JiraStatusRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, jiraRef(n))
	}
	return refs
}
