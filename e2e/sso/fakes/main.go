// Command fakes runs the test servers the SSO end-to-end suite signs in
// through: two SAML identity providers and a GitHub stand-in, each over HTTPS
// with a certificate from a CA it generates. The `license` subcommand mints an
// Enterprise Edition licence signed by a key generated for the run, for a
// binary built to trust that key.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// users.json, the one place the suite's people are defined.
type fixtures struct {
	GitHub []githubUser             `json:"github"`
	IdPs   map[string][]samlIdPUser `json:"idps"`
}

type githubUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type samlIdPUser struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Email    string `json:"email"`
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "license":
		license(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fakes serve -state DIR -users FILE -github-client-id ID -github-client-secret SECRET")
	fmt.Fprintln(os.Stderr, "       fakes license -state DIR")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	stateDir := fs.String("state", "", "directory holding the CA and leaf certificate (created when absent)")
	usersPath := fs.String("users", "", "path to users.json")
	clientID := fs.String("github-client-id", "", "OAuth client id GoTrue presents to the fake GitHub")
	clientSecret := fs.String("github-client-secret", "", "OAuth client secret GoTrue presents to the fake GitHub")
	_ = fs.Parse(args)
	if *stateDir == "" || *usersPath == "" || *clientID == "" || *clientSecret == "" {
		usage()
	}

	raw, err := os.ReadFile(*usersPath)
	if err != nil {
		log.Fatalf("read users: %v", err)
	}
	var fx fixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		log.Fatalf("parse users: %v", err)
	}

	leaf, err := ensureCertificates(*stateDir, []string{hostGlobex, hostMallory, hostGitHub})
	if err != nil {
		log.Fatalf("certificates: %v", err)
	}

	globex, err := newSAMLIdP("https://"+hostGlobex+":8443", fx.IdPs["globex"])
	if err != nil {
		log.Fatalf("globex idp: %v", err)
	}
	mallory, err := newSAMLIdP("https://"+hostMallory+":8444", fx.IdPs["mallory"])
	if err != nil {
		log.Fatalf("mallory idp: %v", err)
	}
	gh := newFakeGitHub(fx.GitHub, *clientID, *clientSecret)

	servers := []*http.Server{
		listen(":8443", globex, leaf),
		listen(":8444", mallory, leaf),
		listen(":8445", gh, leaf),
	}
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", srv.Addr, err)
			}
		}()
	}
	log.Printf("serving %s:8443, %s:8444, %s:8445", hostGlobex, hostMallory, hostGitHub)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		log.Fatal(err)
	case <-sig:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
}

const (
	hostGlobex  = "idp-globex.test"
	hostMallory = "idp-mallory.test"
	hostGitHub  = "github.test"
)

// listen binds every interface: GoTrue reaches these servers from inside its
// container through the host gateway, not through loopback.
func listen(addr string, h http.Handler, leaf tls.Certificate) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           logRequests(h),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.Host, r.Method, r.URL.Path)
		h.ServeHTTP(w, r)
	})
}

func statePath(dir, name string) string { return filepath.Join(dir, name) }
