package llmcred

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithy "github.com/aws/smithy-go"
)

// ErrNoAmbientIdentity is returned when the control process has no usable
// AWS identity in its ambient credential chain — no instance role, no
// AWS_* env vars. It is an OPERATOR problem (the org admin can't fix it
// from the UI); the connect probe and the role-setup endpoint surface it
// with the "set AWS_* on the control service or use an instance role"
// remediation, distinct from a trust-policy failure the admin can fix.
var ErrNoAmbientIdentity = errors.New("llmcred: the control service has no AWS identity")

// ErrAssumeRoleDenied is returned when the ambient identity is present but
// AssumeRole was refused — the customer's role trust policy doesn't allow
// this caller, or the External ID doesn't match. It is an ORG-ADMIN
// problem: re-render the trust-policy snippet and check the External ID.
var ErrAssumeRoleDenied = errors.New("llmcred: AssumeRole denied")

// ErrRoleMisconfigured is returned when a role-mode org is missing config
// the mint needs (no External ID stored) or the role's max-session
// duration is below the requested TTL. Distinct from the two AWS-side
// failures above — this is a TF-side/config problem surfaced actionably.
var ErrRoleMisconfigured = errors.New("llmcred: Bedrock role config is incomplete")

// AssumeRoleInput is the neutral shape STSMinter.AssumeRole takes — the
// SDK-agnostic subset the resolver builds. Duration maps to
// sts:AssumeRole DurationSeconds; Policy is the inline session policy JSON
// (the intersection ceiling that scopes the session to bedrock:InvokeModel
// on the org's model, optionally network-bound).
type AssumeRoleInput struct {
	RoleARN         string
	ExternalID      string
	RoleSessionName string
	Policy          string
	Duration        time.Duration
}

// MintedCredentials is the resolver-facing result of a successful
// AssumeRole: the STS session triple plus its expiry. Expiry drives both
// the per-org mint cache half-life and the executor's live-refresh
// re-read of the sealed bundle.
type MintedCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiry          time.Time
}

// STSMinter is the seam the resolver mints through. The concrete
// implementation (awsSTS) assumes the customer's role as whatever AWS
// identity the control process ambiently holds; tests inject a fake so the
// resolver's caching, policy, and error-classification logic is exercised
// without real AWS.
type STSMinter interface {
	// AssumeRole performs sts:AssumeRole and returns the session triple.
	// Returns ErrNoAmbientIdentity when the caller has no AWS identity,
	// ErrAssumeRoleDenied on a trust-policy / External-ID refusal, and a
	// wrapped ErrRoleMisconfigured when the role's max-session duration is
	// below the requested Duration.
	AssumeRole(ctx context.Context, in AssumeRoleInput) (MintedCredentials, error)

	// CallerIdentityARN returns the ARN of the caller's ambient identity
	// (sts:GetCallerIdentity) — the value the role-setup trust-policy
	// snippet fills into the Principal. Returns ErrNoAmbientIdentity when
	// the control process has no AWS identity.
	CallerIdentityARN(ctx context.Context) (string, error)
}

// stsAPI is the narrow slice of the sts client the impl uses — declared so
// the impl's own logic stays testable and the SDK surface stays visible.
type stsAPI interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// awsSTS is the production STSMinter. It resolves the deployment's ambient
// AWS credential chain (instance role, or AWS_* env vars an operator set on
// the control service in .env) once, lazily, and reuses the resulting
// client. There is deliberately no assume-with-org-triple path — the caller
// identity is always the deployment's own, never a customer static key.
type awsSTS struct {
	mu     sync.Mutex
	client stsAPI
	creds  aws.CredentialsProvider
	loaded bool
}

// NewAWSMinter returns the production STSMinter backed by the ambient AWS
// credential chain. Construction is cheap and never fails — the ambient
// identity is resolved lazily on first use, so a deployment with no AWS
// identity constructs fine and surfaces ErrNoAmbientIdentity only when a
// role-mode org actually needs a mint.
func NewAWSMinter() STSMinter {
	return &awsSTS{}
}

// ensure resolves the ambient config + STS client once. Safe to call
// concurrently. A load failure is not cached — a transient region/config
// error can recover on the next call.
func (a *awsSTS) ensure(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return nil
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("llmcred: load ambient AWS config: %w", err)
	}
	// STS needs a region to construct a client even though AssumeRole works
	// from any region; the caller's STS region is independent of the org's
	// Bedrock region. Default to us-east-1 when the ambient chain resolved
	// none — the request still reaches the global STS endpoint.
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	a.creds = cfg.Credentials
	a.client = sts.NewFromConfig(cfg)
	a.loaded = true
	return nil
}

// ambientPresent probes the caller's credential chain so a no-identity
// deployment fails with ErrNoAmbientIdentity (an operator problem) rather
// than an opaque AssumeRole error. A retrieval failure is the definitive
// "no ambient identity" signal.
func (a *awsSTS) ambientPresent(ctx context.Context) error {
	a.mu.Lock()
	creds := a.creds
	a.mu.Unlock()
	if creds == nil {
		return ErrNoAmbientIdentity
	}
	if _, err := creds.Retrieve(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrNoAmbientIdentity, err)
	}
	return nil
}

func (a *awsSTS) AssumeRole(ctx context.Context, in AssumeRoleInput) (MintedCredentials, error) {
	if err := a.ensure(ctx); err != nil {
		return MintedCredentials{}, err
	}
	if err := a.ambientPresent(ctx); err != nil {
		return MintedCredentials{}, err
	}

	req := &sts.AssumeRoleInput{
		RoleArn:         aws.String(in.RoleARN),
		RoleSessionName: aws.String(in.RoleSessionName),
		DurationSeconds: aws.Int32(int32(in.Duration / time.Second)),
	}
	if in.ExternalID != "" {
		req.ExternalId = aws.String(in.ExternalID)
	}
	if in.Policy != "" {
		req.Policy = aws.String(in.Policy)
	}

	out, err := a.client.AssumeRole(ctx, req)
	if err != nil {
		return MintedCredentials{}, classifyAssumeRoleError(err)
	}
	if out.Credentials == nil || out.Credentials.AccessKeyId == nil || out.Credentials.SecretAccessKey == nil {
		return MintedCredentials{}, errors.New("llmcred: AssumeRole returned no credentials")
	}
	c := out.Credentials
	return MintedCredentials{
		AccessKeyID:     aws.ToString(c.AccessKeyId),
		SecretAccessKey: aws.ToString(c.SecretAccessKey),
		SessionToken:    aws.ToString(c.SessionToken),
		Expiry:          aws.ToTime(c.Expiration),
	}, nil
}

func (a *awsSTS) CallerIdentityARN(ctx context.Context) (string, error) {
	if err := a.ensure(ctx); err != nil {
		return "", err
	}
	if err := a.ambientPresent(ctx); err != nil {
		return "", err
	}
	out, err := a.client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		// A GetCallerIdentity failure with a present credential chain is
		// unusual (the call needs no permissions), but classify anyway so a
		// credential-expiry mid-flight still reads as a no-identity operator
		// problem rather than a bare AWS error.
		return "", classifyAssumeRoleError(err)
	}
	return aws.ToString(out.Arn), nil
}

// classifyAssumeRoleError maps a raw SDK error to one of the resolver's
// typed classes. AccessDenied → ErrAssumeRoleDenied (trust policy /
// External ID — an org-admin fix). A duration validation error →
// ErrRoleMisconfigured naming the two knobs. A credential-provider /
// retrieval failure that slipped past ambientPresent → ErrNoAmbientIdentity.
// Anything else is returned wrapped, verbatim.
func classifyAssumeRoleError(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		switch {
		case code == "AccessDenied" || code == "AccessDeniedException":
			return fmt.Errorf("%w: %s", ErrAssumeRoleDenied, apiErr.ErrorMessage())
		case code == "ValidationError" && strings.Contains(strings.ToLower(apiErr.ErrorMessage()), "duration"):
			return fmt.Errorf("%w: the role's maximum session duration is below the requested TF_LLM_CRED_TTL_SEC — raise the role's MaxSessionDuration in IAM or lower TF_LLM_CRED_TTL_SEC (%s)", ErrRoleMisconfigured, apiErr.ErrorMessage())
		case code == "ExpiredToken" || code == "InvalidClientTokenId" || code == "SignatureDoesNotMatch":
			return fmt.Errorf("%w: %s", ErrNoAmbientIdentity, apiErr.ErrorMessage())
		}
	}
	return fmt.Errorf("llmcred: AssumeRole: %w", err)
}
