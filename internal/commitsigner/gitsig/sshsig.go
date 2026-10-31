// Package gitsig makes the SSH signatures git attaches to commits and
// reads the commit objects git asks to have signed.
//
// With gpg.format=ssh, git hands its signing program the commit object it is
// about to write, reads back an armored signature, and writes that signature
// into the object as a gpgsig header. The signature is OpenSSH's SSHSIG format
// (PROTOCOL.sshsig), which golang.org/x/crypto does not implement, so this
// package does.
//
// Signing is split at the digest. An SSHSIG signature covers only the SHA-512
// digest of the payload, so the process that holds the key needs only the
// digest (SignDigest) and never parses the commit, while the process that
// holds the commit checks it (ParseCommitter), computes the digest (DigestOf)
// and, once the signature is back, the id git will give the signed commit
// (SignedCommit, CommitID).
package gitsig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/ssh"
)

const (
	// Namespace is the SSHSIG namespace git signs commits in: the -n git it
	// passes its signing program.
	Namespace = "git"
	// HashAlgorithm names the hash the signed data carries the payload's
	// digest under.
	HashAlgorithm = "sha512"
)

const (
	sigMagic   = "SSHSIG"
	sigVersion = 1

	armorBegin = "-----BEGIN SSH SIGNATURE-----\n"
	armorEnd   = "-----END SSH SIGNATURE-----\n"
	// armorWidth is the base64 line length ssh-keygen -Y sign writes, so an
	// armored signature is byte-for-byte what ssh-keygen would produce.
	armorWidth = 70
)

// Digest is the SHA-512 digest of a payload, the only part of it an SSHSIG
// signature covers.
type Digest [sha512.Size]byte

// DigestOf returns payload's digest.
func DigestOf(payload []byte) Digest {
	return sha512.Sum512(payload)
}

// String returns the digest as lowercase hex.
func (d Digest) String() string {
	return hex.EncodeToString(d[:])
}

// ParseDigest parses the lowercase hex form String writes. Anything else,
// uppercase hex included, is refused.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	if len(s) != hex.EncodedLen(len(d)) {
		return Digest{}, fmt.Errorf("gitsig: digest is %d characters, want %d", len(s), hex.EncodedLen(len(d)))
	}
	if _, err := hex.Decode(d[:], []byte(s)); err != nil {
		return Digest{}, fmt.Errorf("gitsig: digest: %w", err)
	}
	if d.String() != s {
		return Digest{}, fmt.Errorf("gitsig: digest is not lowercase hex")
	}
	return d, nil
}

// SignedData returns the bytes an SSHSIG signature over d signs: the SSHSIG
// magic, then the namespace, an empty reserved field, the hash algorithm and
// the digest, each as an SSH string.
func SignedData(d Digest) []byte {
	return append([]byte(sigMagic), ssh.Marshal(struct {
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Hash          []byte
	}{Namespace, "", HashAlgorithm, d[:]})...)
}

// SignDigest signs d with key and returns the armored signature, the bytes
// git reads from <buffer>.sig.
func SignDigest(key ed25519.PrivateKey, d Digest) ([]byte, error) {
	pub, err := sshPublicKey(key)
	if err != nil {
		return nil, err
	}
	sig := ssh.Signature{Format: ssh.KeyAlgoED25519, Blob: ed25519.Sign(key, SignedData(d))}
	blob := append([]byte(sigMagic), ssh.Marshal(struct {
		Version       uint32
		PublicKey     []byte
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Signature     []byte
	}{sigVersion, pub.Marshal(), Namespace, "", HashAlgorithm, ssh.Marshal(sig)})...)
	return armor(blob), nil
}

// Sign signs payload with key: SignDigest over DigestOf(payload).
func Sign(key ed25519.PrivateKey, payload []byte) ([]byte, error) {
	return SignDigest(key, DigestOf(payload))
}

// armor wraps an SSHSIG blob the way ssh-keygen does: base64 in lines of
// armorWidth between the BEGIN and END lines, every line ending in a newline.
func armor(blob []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(blob)
	var out bytes.Buffer
	out.WriteString(armorBegin)
	for len(b64) > armorWidth {
		out.WriteString(b64[:armorWidth])
		out.WriteByte('\n')
		b64 = b64[armorWidth:]
	}
	out.WriteString(b64)
	out.WriteByte('\n')
	out.WriteString(armorEnd)
	return out.Bytes()
}
