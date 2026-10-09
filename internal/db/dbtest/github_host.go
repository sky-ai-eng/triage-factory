package dbtest

// TestGitHubHost is the GitHub host the suites write repositories and team
// tracking under, and the host their reads ask about. It is the host an org with
// no GitHub base URL configured resolves to (db.EffectiveGitHubHost), so a
// fixture that seeds an org without one lands on the same host.
const TestGitHubHost = "https://github.com"
