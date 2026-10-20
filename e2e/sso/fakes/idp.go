package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlidp"
	"golang.org/x/crypto/bcrypt"
)

// newSAMLIdP builds a crewjam IdP at baseURL with an in-memory store holding
// users. Its metadata is served at /metadata, sign-in starts at /sso, and the
// suite registers GoTrue as a service provider through PUT /services/{id}.
// The signing key is minted per process: GoTrue reads it from the metadata at
// registration, which happens after this process starts.
func newSAMLIdP(baseURL string, users []samlIdPUser) (http.Handler, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: u.Hostname() + " signing"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	store := &samlidp.MemoryStore{}
	for _, usr := range users {
		hash, err := bcrypt.GenerateFromPassword([]byte(usr.Password), bcrypt.MinCost)
		if err != nil {
			return nil, err
		}
		if err := store.Put("/users/"+usr.User, samlidp.User{
			Name:           usr.User,
			HashedPassword: hash,
			Email:          usr.Email,
		}); err != nil {
			return nil, fmt.Errorf("store user %s: %w", usr.User, err)
		}
	}

	srv, err := samlidp.New(samlidp.Options{URL: *u, Key: key, Certificate: cert, Store: store})
	if err != nil {
		return nil, err
	}
	srv.IDP.SessionProvider = persistentSubject{srv}
	// The login form posts to LoginURL along with the pending SAMLRequest and
	// RelayState. crewjam's /login only creates an IdP session and prints it;
	// /sso takes the same fields, signs the user in and answers the request.
	srv.IDP.LoginURL = srv.IDP.SSOURL
	return srv, nil
}

// persistentSubject issues each assertion's NameID as persistent. crewjam
// defaults to a transient NameID, which GoTrue refuses as a user identifier.
type persistentSubject struct{ next saml.SessionProvider }

func (p persistentSubject) GetSession(w http.ResponseWriter, r *http.Request, req *saml.IdpAuthnRequest) *saml.Session {
	s := p.next.GetSession(w, r, req)
	if s != nil {
		s.NameIDFormat = string(saml.PersistentNameIDFormat)
	}
	return s
}
