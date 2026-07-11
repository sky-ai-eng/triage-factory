// Package credseal implements the "sealed box" construction (TFAC-614): a
// message encrypted to a recipient's X25519 public key by an ephemeral
// sender keypair that is discarded after use. The recipient needs only its
// own static keypair to open a sealed message — the sender needs no
// keypair of its own and never identifies itself, which is exactly the
// brain's position when it seals a run's credential bundle to whichever
// executor happens to have claimed the run.
//
// This is the same construction libsodium calls crypto_box_seal, built
// here from golang.org/x/crypto/nacl/box because the stdlib and our
// existing x/crypto dependency don't expose a ready-made sealed-box
// helper: an ephemeral keypair is generated per Seal call, its public half
// is prefixed to the ciphertext (the recipient needs it to open), and the
// private half is never stored or returned anywhere.
package credseal

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/nacl/box"
)

// KeyPair is an X25519 keypair used to unseal run credential bundles. Each
// executor instance mints one at boot, in-memory only (see
// internal/instance) — it is never persisted, and a restart mints a fresh
// one. The zero value is not valid; use GenerateKeyPair.
type KeyPair struct {
	Public  [32]byte
	private [32]byte
}

// GenerateKeyPair mints a fresh X25519 keypair from crypto/rand.
func GenerateKeyPair() (*KeyPair, error) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("credseal: generate keypair: %w", err)
	}
	return &KeyPair{Public: *pub, private: *priv}, nil
}

// sealedHeaderLen is the ephemeral public key (32 bytes) plus the nonce (24
// bytes) prefixed to every sealed blob.
const sealedHeaderLen = 32 + 24

// Seal encrypts plaintext to recipientPub using a fresh ephemeral sender
// keypair, discarded after this call returns. The output layout is
// ephemeral-public-key(32) || nonce(24) || box(...) — self-describing, so
// Open needs only the recipient's private key.
func Seal(recipientPub [32]byte, plaintext []byte) ([]byte, error) {
	ephPub, ephPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("credseal: generate ephemeral keypair: %w", err)
	}
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("credseal: read nonce: %w", err)
	}
	out := make([]byte, 0, sealedHeaderLen+len(plaintext)+box.Overhead)
	out = append(out, ephPub[:]...)
	out = append(out, nonce[:]...)
	out = box.Seal(out, plaintext, &nonce, &recipientPub, ephPriv)
	return out, nil
}

// ErrSealedBoxInvalid is returned by Open when sealed is too short to
// contain a header, or fails to decrypt — wrong key, tampered ciphertext,
// or a blob sealed to a different recipient entirely.
var ErrSealedBoxInvalid = errors.New("credseal: sealed box is invalid (too short, tampered, or sealed to a different key)")

// Open decrypts a blob produced by Seal for kp.Public. Fails closed on any
// malformed or tampered input — Open must never partially decrypt, since a
// caller that ignores the error and reads a zero-value plaintext could
// otherwise proceed with garbage credentials.
func (kp *KeyPair) Open(sealed []byte) ([]byte, error) {
	if kp == nil {
		return nil, fmt.Errorf("%w: nil keypair", ErrSealedBoxInvalid)
	}
	if len(sealed) < sealedHeaderLen {
		return nil, fmt.Errorf("%w: %d bytes, want at least %d", ErrSealedBoxInvalid, len(sealed), sealedHeaderLen)
	}
	var ephPub [32]byte
	copy(ephPub[:], sealed[:32])
	var nonce [24]byte
	copy(nonce[:], sealed[32:56])
	ciphertext := sealed[56:]

	plaintext, ok := box.Open(nil, ciphertext, &nonce, &ephPub, &kp.private)
	if !ok {
		return nil, ErrSealedBoxInvalid
	}
	return plaintext, nil
}
