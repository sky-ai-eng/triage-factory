package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// fakeGitHub implements the calls GoTrue's GitHub provider makes when
// GOTRUE_EXTERNAL_GITHUB_URL points at it: the authorize page, the code
// exchange, and the two API reads, which a custom URL puts under /api/v3.
// Signing in takes a username alone. PUT /users/{login} changes a user's
// email, which no real provider lets a test do.
type fakeGitHub struct {
	clientID, clientSecret string

	mu     sync.Mutex
	users  map[string]*fakeGitHubAccount // by login
	codes  map[string]string             // authorization code → login
	tokens map[string]string             // access token → login
}

type fakeGitHubAccount struct {
	ID    int
	Login string
	Name  string
	Email string
}

func newFakeGitHub(users []githubUser, clientID, clientSecret string) http.Handler {
	g := &fakeGitHub{
		clientID: clientID, clientSecret: clientSecret,
		users:  map[string]*fakeGitHubAccount{},
		codes:  map[string]string{},
		tokens: map[string]string{},
	}
	for i, u := range users {
		g.users[u.Login] = &fakeGitHubAccount{ID: 1000 + i, Login: u.Login, Name: u.Name, Email: u.Email}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", g.authorizeForm)
	mux.HandleFunc("POST /login/oauth/authorize", g.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", g.accessToken)
	mux.HandleFunc("GET /api/v3/user", g.user)
	mux.HandleFunc("GET /api/v3/user/emails", g.userEmails)
	mux.HandleFunc("PUT /users/{login}", g.putUser)
	return mux
}

var authorizeTmpl = template.Must(template.New("authorize").Parse(`<!doctype html>
<html><head><title>Sign in to GitHub (test)</title></head>
<body>
<h1>Sign in to GitHub (test)</h1>
<form method="post" action="/login/oauth/authorize">
<label>Username <input name="login" autofocus></label>
<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="state" value="{{.State}}">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<button type="submit">Authorize</button>
</form>
</body></html>`))

func (g *fakeGitHub) authorizeForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != g.clientID {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = authorizeTmpl.Execute(w, map[string]string{
		"RedirectURI": q.Get("redirect_uri"),
		"State":       q.Get("state"),
		"ClientID":    q.Get("client_id"),
	})
}

func (g *fakeGitHub) authorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	login := strings.TrimSpace(r.PostForm.Get("login"))
	redirect, err := url.Parse(r.PostForm.Get("redirect_uri"))
	if err != nil || redirect.Scheme == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	_, known := g.users[login]
	code := randomHex()
	if known {
		g.codes[code] = login
	}
	g.mu.Unlock()
	if !known {
		http.Error(w, "no such user: "+login, http.StatusNotFound)
		return
	}
	q := redirect.Query()
	q.Set("code", code)
	q.Set("state", r.PostForm.Get("state"))
	redirect.RawQuery = q.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// accessToken accepts the client credentials either way golang.org/x/oauth2
// sends them: HTTP Basic, or form fields when the server refused Basic.
func (g *fakeGitHub) accessToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != g.clientID || secret != g.clientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	code := r.PostForm.Get("code")
	g.mu.Lock()
	login, ok := g.codes[code]
	delete(g.codes, code)
	token := randomHex()
	if ok {
		g.tokens[token] = login
	}
	g.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_verification_code"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "bearer", "scope": "user:email"})
}

func (g *fakeGitHub) account(r *http.Request) (fakeGitHubAccount, bool) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		token, ok = strings.CutPrefix(auth, "token ")
	}
	if !ok {
		return fakeGitHubAccount{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	login, ok := g.tokens[token]
	if !ok {
		return fakeGitHubAccount{}, false
	}
	return *g.users[login], true
}

func (g *fakeGitHub) user(w http.ResponseWriter, r *http.Request) {
	a, ok := g.account(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": a.ID, "login": a.Login, "name": a.Name, "email": a.Email,
		"avatar_url": "https://" + hostGitHub + ":8445/avatars/" + a.Login,
	})
}

func (g *fakeGitHub) userEmails(w http.ResponseWriter, r *http.Request) {
	a, ok := g.account(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	writeJSON(w, http.StatusOK, []map[string]any{{"email": a.Email, "primary": true, "verified": true}})
}

func (g *fakeGitHub) putUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		http.Error(w, "body must be {\"email\": \"...\"}", http.StatusBadRequest)
		return
	}
	login := r.PathValue("login")
	g.mu.Lock()
	a, ok := g.users[login]
	if ok {
		a.Email = body.Email
	}
	g.mu.Unlock()
	if !ok {
		http.Error(w, "no such user", http.StatusNotFound)
		return
	}
	log.Printf("github: %s's email is now %s", login, body.Email)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomHex() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
