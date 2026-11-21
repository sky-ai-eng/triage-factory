package server

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// oauthAppCredentialMaxLen caps the client ID and the client secret of an
// org's own OAuth app. Atlassian's and Linear's are well under it; the cap only
// refuses a value no OAuth app would issue.
const oauthAppCredentialMaxLen = 256

// oauthAppCredentials is the check every OAuth app store runs on the client ID
// and secret it is handed: both trimmed, both required, and neither longer than
// oauthAppCredentialMaxLen characters. It records every fault on v and returns
// the trimmed values. app names the app in the messages, as in "A Linear OAuth
// app".
func oauthAppCredentials(v *httpx.Validation, app, clientID, clientSecret string) (string, string) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" {
		v.Add(httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: app + " client ID is required.", Field: "client_id"})
	}
	if clientSecret == "" {
		v.Add(httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: app + " client secret is required.", Field: "client_secret"})
	}
	if utf8.RuneCountInString(clientID) > oauthAppCredentialMaxLen {
		v.OutOfRange("client_id", fmt.Sprintf("client_id must be at most %d characters", oauthAppCredentialMaxLen))
	}
	if utf8.RuneCountInString(clientSecret) > oauthAppCredentialMaxLen {
		v.OutOfRange("client_secret", fmt.Sprintf("client_secret must be at most %d characters", oauthAppCredentialMaxLen))
	}
	return clientID, clientSecret
}
