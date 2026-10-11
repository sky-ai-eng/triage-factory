package dbtest

// TestGitHubHost is the GitHub host the suites write repositories and team
// tracking under, and the host their reads ask about. A suite that reads the
// org's current host sets it as the org's GitHub URL first: an org with none is
// on no GitHub.
const TestGitHubHost = "https://github.com"

// TestOtherGitHubHost is a second GitHub host, for the cases that pin host
// scoping: the same owner/repo, provider id or GitHub team written on both
// hosts must stay two independent things.
const TestOtherGitHubHost = "https://ghe.example.com"
