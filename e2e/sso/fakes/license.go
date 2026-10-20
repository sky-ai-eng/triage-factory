package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

// licenseClaims mirrors the payload ee/license verifies. It is restated here
// rather than imported because this module never depends on product code.
type licenseClaims struct {
	Org      string   `json:"org"`
	Features []string `json:"features"`
	IssuedAt int64    `json:"iat"`
	Expires  int64    `json:"exp"`
}

// license writes the public half of a fresh P-256 key to
// <state>/license-pub.b64, as the standard base64 of its SPKI DER (the form
// the binary's publicKeyB64 takes), and prints a token signed by the private
// half, which is discarded.
func license(args []string) {
	fs := flag.NewFlagSet("license", flag.ExitOnError)
	stateDir := fs.String("state", "", "directory to write license-pub.b64 into")
	_ = fs.Parse(args)
	if *stateDir == "" {
		usage()
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		log.Fatal(err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(statePath(*stateDir, "license-pub.b64"), []byte(base64.StdEncoding.EncodeToString(spki)), 0o644); err != nil {
		log.Fatal(err)
	}

	now := time.Now()
	payload, err := json.Marshal(licenseClaims{
		Org:      "sso-e2e",
		Features: []string{"sso", "fleet"},
		IssuedAt: now.Unix(),
		Expires:  now.Add(24 * time.Hour).Unix(),
	})
	if err != nil {
		log.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		log.Fatal(err)
	}
	b64 := base64.RawURLEncoding
	fmt.Println(b64.EncodeToString(payload) + "." + b64.EncodeToString(sig))
}
