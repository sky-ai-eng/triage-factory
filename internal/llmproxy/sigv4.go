// SigV4 re-signing for the Bedrock AWS-access-key-triple path (credential
// proxy Phase 2).
//
// The bearer providers in proxy.go overwrite one auth header and forward.
// AWS's SigV4 protocol doesn't allow that: the Authorization header carries
// an HMAC over the *canonical request* — method, URI, query, the signed
// header set (including host and the fresh x-amz-date), and the SHA-256 of
// the body — keyed by the access secret. Swapping credentials therefore
// means re-signing the whole outgoing request, not rewriting a header.
//
// Flow for ProviderBedrockSigV4:
//
//  1. The sandbox SDK signs its request with throwaway placeholder creds
//     (the SDK refuses to construct a Bedrock client without *some* creds).
//     The placeholder access-key ID doubles as the per-run IncomingToken —
//     it rides in cleartext in the Authorization header's Credential= scope,
//     exactly as bearer tokens ride their headers, so the token gate can
//     authenticate the caller without verifying the placeholder signature.
//  2. bufferSigV4Body reads the full body into memory (SigV4 hashes the
//     complete payload; Bedrock InvokeModel / InvokeModelWithResponseStream
//     request bodies are bounded and non-streaming — only responses stream).
//  3. rewriteSigV4 strips everything the placeholder signature contributed
//     (Authorization, X-Amz-Date, X-Amz-Security-Token, X-Amz-Content-Sha256),
//     then signs the final outgoing request — post-SetURL, so host and path
//     are the upstream's — with the org's real triple via the AWS SDK's
//     v4.Signer.SignHTTP.
//
// Signing runs inside the Rewrite hook because that is the only point where
// the outgoing request has its final shape: the stdlib removes hop-by-hop
// headers and strips X-Forwarded-* BEFORE Rewrite is called, and the
// transport only adds unsigned headers afterwards (Accept-Encoding when
// absent, User-Agent — both on the signer's ignore list), so the header set
// covered by the signature is exactly what reaches AWS.
//
// Out of scope, per the phase ticket: streaming *request* bodies (no
// current Bedrock text path sends one), mid-run credential refresh (proxy
// lifetime is one run), and SigV4-A (the asymmetric multi-region variant —
// aws-sdk-go-v2's core signer is single-region SigV4 only; Bedrock's
// cross-region inference profiles are addressed by regional endpoints with
// standard SigV4, the routing happens server-side).
package llmproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// ProviderBedrockSigV4 re-signs each request with an AWS access-key
// triple before forwarding to bedrock-runtime.<region>.amazonaws.com.
// Config.SigV4Credentials carries the triple; Config.APIKey is unused.
const ProviderBedrockSigV4 Provider = "bedrock_sigv4"

// bedrockSigningService is the service name in the SigV4 credential
// scope for Bedrock runtime calls. Note it is "bedrock", not
// "bedrock-runtime" — the endpoint host and the signing name differ
// (confirmed against aws-sdk-go-v2/service/bedrockruntime, which sets
// SigningName "bedrock").
const bedrockSigningService = "bedrock"

// maxSigV4BodyBytes caps how much request body the proxy will buffer
// for signing. SigV4 needs the complete payload in hand to hash, so an
// unbounded body would be an unbounded host-side allocation driven by
// the (untrusted) sandbox. Bedrock's own InvokeModel payload limit is
// 25 MB; 32 MiB clears that with headroom while still bounding a
// hostile sender. Requests beyond the cap get 413.
const maxSigV4BodyBytes = 32 << 20

// SigV4Credentials is the real AWS access-key triple for the Bedrock
// SigV4 path. Required when Provider == ProviderBedrockSigV4; ignored
// otherwise. Loaded from the org's secret store at proxy construction;
// like APIKey, it never appears in the agent subprocess's env.
type SigV4Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional (STS temporary credentials / role assumption)

	// Region is the signing region in the credential scope. It must
	// match the region of the Upstream endpoint or AWS rejects the
	// signature with a scope-mismatch 403.
	Region string
}

// validate is the construction-time check New runs for
// ProviderBedrockSigV4, mirroring the fail-loudly-at-boot posture of
// the rest of Config validation.
func (c *SigV4Credentials) validate() error {
	if c == nil {
		return errors.New("llmproxy: SigV4Credentials is required for provider bedrock_sigv4")
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return errors.New("llmproxy: SigV4Credentials requires both AccessKeyID and SecretAccessKey")
	}
	if c.Region == "" {
		return errors.New("llmproxy: SigV4Credentials.Region is required (the SigV4 credential scope is regional)")
	}
	return nil
}

// sigV4Payload carries the buffered request body and its hex-encoded
// SHA-256 from bufferSigV4Body to rewriteSigV4 via the request context.
// Buffering happens in middleware (not the Rewrite hook) because the
// hook has no error path — a client that hangs up mid-body or overruns
// the cap gets a proper 400/413 there instead of an opaque 502.
type sigV4Payload struct {
	body      []byte
	sha256Hex string
}

type sigV4PayloadKey struct{}

// bufferSigV4Body reads the complete request body into memory, hashes
// it, and re-arms r.Body with an in-memory reader before handing off to
// the reverse proxy. Runs after the token gate (no buffering work for
// unauthenticated callers) and before ReverseProxy clones the request,
// so both In and Out start from the buffered body.
func (s *Server) bufferSigV4Body(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSigV4BodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, fmt.Sprintf("request body exceeds the %d-byte SigV4 signing buffer", int64(maxSigV4BodyBytes)), http.StatusRequestEntityTooLarge)
				return
			}
			// Truncated or malformed body (client hung up, bad chunked
			// framing). Nothing was forwarded upstream yet.
			http.Error(w, "llmproxy: read request body for signing", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		payload := &sigV4Payload{body: body, sha256Hex: hex.EncodeToString(sum[:])}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sigV4PayloadKey{}, payload)))
	})
}

// rewriteSigV4 is the ProviderBedrockSigV4 branch of the Rewrite hook.
// It runs after SetURL has pointed pr.Out at the real Bedrock endpoint,
// so the signature it computes covers the final host + path.
func (s *Server) rewriteSigV4(pr *httputil.ProxyRequest) {
	out := pr.Out
	payload, ok := out.Context().Value(sigV4PayloadKey{}).(*sigV4Payload)
	if !ok {
		// New always chains bufferSigV4Body ahead of the proxy for this
		// provider, so a missing payload is a wiring bug. Fail the
		// exchange loudly rather than forwarding unsigned.
		failExchange(out, errors.New("llmproxy: sigv4 payload missing from request context (bufferSigV4Body not in the handler chain?)"))
		return
	}

	// Strip everything the placeholder signature contributed. The
	// placeholder Authorization is useless upstream; X-Amz-Date must be
	// fresh (the signer sets it from the signing time); a stale
	// X-Amz-Security-Token from placeholder creds would be signed as-is
	// and 403 at AWS whenever the real triple has no session token (the
	// signer only overwrites the header when one is present); and the
	// body-hash header is recomputed from the buffered bytes. A literal
	// Content-Length map entry goes too — the field, which we set below,
	// is what both the signer and the transport read.
	out.Header.Del("Authorization")
	out.Header.Del("X-Amz-Date")
	out.Header.Del("X-Amz-Security-Token")
	out.Header.Del("X-Amz-Content-Sha256")
	out.Header.Del("Content-Length")

	if len(payload.body) == 0 {
		// http.NoBody (not a zero-length reader): the transport treats
		// ContentLength==0 with a non-nil generic Body as "length
		// unknown" and switches to chunked framing, which would diverge
		// from the signed (absent) content-length.
		out.Body = http.NoBody
	} else {
		out.Body = io.NopCloser(bytes.NewReader(payload.body))
	}
	out.ContentLength = int64(len(payload.body))

	// Set the body-hash header explicitly so what we sign is also what
	// the upstream can independently verify against the payload. SignHTTP
	// signs whatever headers are present but never adds this one itself.
	out.Header.Set("X-Amz-Content-Sha256", payload.sha256Hex)

	creds := aws.Credentials{
		AccessKeyID:     s.cfg.SigV4Credentials.AccessKeyID,
		SecretAccessKey: s.cfg.SigV4Credentials.SecretAccessKey,
		SessionToken:    s.cfg.SigV4Credentials.SessionToken,
	}
	err := s.signer.SignHTTP(out.Context(), creds, out, payload.sha256Hex,
		bedrockSigningService, s.cfg.SigV4Credentials.Region, time.Now().UTC())
	if err != nil {
		// SignHTTP with static credentials has no realistic failure mode,
		// but if it ever errors the request must not go out half-signed.
		failExchange(out, fmt.Errorf("llmproxy: sigv4 re-sign: %w", err))
	}
}

// failExchange rigs the outgoing request so the transport aborts it:
// the erroring body fails Request.write, RoundTrip returns the error,
// and ReverseProxy's ErrorHandler turns it into a 502 with the message
// logged. This is the only error channel available inside Rewrite,
// which has no return value.
func failExchange(out *http.Request, err error) {
	out.Header.Del("Authorization")
	out.Body = io.NopCloser(errReader{err: err})
	out.ContentLength = -1
}

// errReader always fails with err. See failExchange.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// sigV4AccessKeyID extracts the access-key ID from a SigV4
// Authorization header:
//
//	AWS4-HMAC-SHA256 Credential=<akid>/<date>/<region>/<service>/aws4_request, SignedHeaders=..., Signature=...
//
// Returns "" for anything that doesn't match that shape, which then
// fails the token gate's constant-time compare (fail closed). The AKID
// is the caller-auth value for ProviderBedrockSigV4: the sandbox's
// placeholder access-key ID is the per-run IncomingToken, riding in
// cleartext in the Credential scope exactly as bearer tokens ride their
// headers. The placeholder *signature* is deliberately not verified —
// caller auth needs only "presents this run's capability", same
// strength as the bearer paths on the same trusted-after-auth hop.
func sigV4AccessKeyID(authz string) string {
	rest, ok := strings.CutPrefix(authz, "AWS4-HMAC-SHA256 ")
	if !ok {
		return ""
	}
	idx := strings.Index(rest, "Credential=")
	if idx < 0 {
		return ""
	}
	cred := rest[idx+len("Credential="):]
	akid, _, ok := strings.Cut(cred, "/")
	if !ok {
		return ""
	}
	return akid
}
