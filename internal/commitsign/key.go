package commitsign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GenerateKey mints a signer's ed25519 key from crypto/rand.
func GenerateKey() (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("commitsign: generate key: %w", err)
	}
	return key, nil
}

// MarshalPrivateKey encodes key as an unencrypted OpenSSH private key, the
// format ssh-keygen writes and reads.
func MarshalPrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errKeySize(key)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return nil, fmt.Errorf("commitsign: marshal private key: %w", err)
	}
	return pem.EncodeToMemory(block), nil
}

// ParsePrivateKey decodes an unencrypted PEM private key, the form
// MarshalPrivateKey writes. A key of any type but ed25519 is refused.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	raw, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("commitsign: parse private key: %w", err)
	}
	switch key := raw.(type) {
	case *ed25519.PrivateKey:
		return *key, nil
	case ed25519.PrivateKey:
		return key, nil
	default:
		return nil, fmt.Errorf("commitsign: private key is %T, want ed25519", raw)
	}
}

// PublicKey returns key's public half as "ssh-ed25519 AAAA…" with no comment:
// the string GitHub's signing-key listing returns for it and an
// allowed-signers line names.
func PublicKey(key ed25519.PrivateKey) (string, error) {
	pub, err := sshPublicKey(key)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(pub)), "\n"), nil
}

func sshPublicKey(key ed25519.PrivateKey) (ssh.PublicKey, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errKeySize(key)
	}
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return nil, fmt.Errorf("commitsign: public key: %w", err)
	}
	return pub, nil
}

func errKeySize(key ed25519.PrivateKey) error {
	return fmt.Errorf("commitsign: ed25519 private key is %d bytes, want %d", len(key), ed25519.PrivateKeySize)
}
