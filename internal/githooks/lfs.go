package githooks

// LFSConfigPairs is the git-lfs filter configuration, as `git lfs install`
// writes it, for a run's git to carry process-scoped beside core.hooksPath.
// An agent's git does not read the operator's global config in either sandbox
// (multi's jail has none; local's masks ~/.gitconfig), and without these a
// checkout of an LFS repository could neither turn pointers into content on
// request nor turn changed content back into pointers on `git add`, which
// would commit the content itself.
//
// Callers add them only where git-lfs is installed: with
// filter.lfs.required set, a missing git-lfs fails every checkout of an LFS
// repository instead of leaving its pointers.
func LFSConfigPairs() [][2]string {
	return [][2]string{
		{"filter.lfs.clean", "git-lfs clean -- %f"},
		{"filter.lfs.smudge", "git-lfs smudge -- %f"},
		{"filter.lfs.process", "git-lfs filter-process"},
		{"filter.lfs.required", "true"},
	}
}

// LFSSkipSmudgeEnvVar, set to "1", makes git-lfs check every LFS file out as
// its pointer. TF sets it for its own checkouts and for every agent's git, so
// a run downloads LFS content only when it asks for it (`git lfs pull
// --include=<path>`), never as a side effect of a checkout: a repository's
// LFS content can be far larger than anything a task needs, and each run's
// checkout keeps its own copy.
const LFSSkipSmudgeEnvVar = "GIT_LFS_SKIP_SMUDGE"
