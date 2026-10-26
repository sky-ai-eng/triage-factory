package agenthost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// signSettingsOrgs answers the org's settings with one GitHub host; nothing
// else is called.
type signSettingsOrgs struct {
	db.OrgsStore
	base string
}

func (o signSettingsOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{GitHubBaseURL: o.base}, nil
}

// recordingRequester records the request it was handed and answers with res.
type recordingRequester struct {
	got   *commitsigner.Request
	res   commitsigner.Result
	calls int
}

func (r *recordingRequester) Request(_ context.Context, req commitsigner.Request) (commitsigner.Result, error) {
	r.calls++
	r.got = &req
	return r.res, nil
}

func signServer(req *recordingRequester) *RelayServer {
	s := NewRelayServer(db.Stores{Orgs: signSettingsOrgs{base: "https://GitHub.com/"}},
		ConversationInfo{OrgID: "org-1", ConversationID: "conv-1", TeamID: "team-1"}, nil)
	s.SetCommitSigning(CommitSigning{ClaimID: "claim-1", BootEpoch: 7, Requester: req})
	return s
}

func callSignDigest(t *testing.T, s *RelayServer, args any) (agentproc.SignDigestReply, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := s.DispatchCall(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpSignDigest, raw)
	if err != nil {
		return agentproc.SignDigestReply{}, err
	}
	var reply agentproc.SignDigestReply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	return reply, nil
}

// TestRelayServer_SignDigestBindsTheRunFromItsOwnIdentity pins that the wire
// supplies only the digest and the sealed signer: the org, conversation,
// engagement and host the request is queued under are the server's, whatever
// else a sidecar sends.
func TestRelayServer_SignDigestBindsTheRunFromItsOwnIdentity(t *testing.T) {
	digest := commitsign.DigestOf([]byte("tree 4b825dc6\n"))
	req := &recordingRequester{res: commitsigner.Result{
		Outcome: commitsigner.OutcomeRefused, SignatureID: "sig-1",
		Refusal: commitsigner.RefusalOrgMinuteBudget, RetryAfterSeconds: 12,
	}}
	reply, err := callSignDigest(t, signServer(req), map[string]any{
		"digest": digest.String(), "signer": "org", "signer_user_id": 583231,
		// Not fields of the op: a sidecar naming another org, run or
		// engagement is ignored.
		"org_id": "org-2", "conversation_id": "conv-2", "claim_id": "claim-2",
	})
	if err != nil {
		t.Fatalf("DispatchCall: %v", err)
	}
	want := commitsigner.Request{
		OrgID: "org-1", ConversationID: "conv-1", ClaimID: "claim-1", BootEpoch: 7,
		Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: 583231,
		Digest: digest,
	}
	if req.got == nil || *req.got != want {
		t.Errorf("queued %+v, want %+v", req.got, want)
	}
	if reply != (agentproc.SignDigestReply{Outcome: "refused", SignatureID: "sig-1", Reason: "org_minute_budget", RetryAfterSeconds: 12}) {
		t.Errorf("reply = %+v, want the refusal with its retry-after", reply)
	}
}

func TestRelayServer_SignDigestRefusesWhatItCannotQueue(t *testing.T) {
	digest := commitsign.DigestOf([]byte("tree\n")).String()
	for name, c := range map[string]struct {
		args  map[string]any
		wired bool
		want  string
	}{
		"not wired":          {args: map[string]any{"digest": digest, "signer": "org", "signer_user_id": 1}, want: "not wired"},
		"a malformed digest": {args: map[string]any{"digest": "abc", "signer": "org", "signer_user_id": 1}, wired: true, want: "digest"},
		"an uppercase digest": {args: map[string]any{"digest": strings.ToUpper(digest), "signer": "org", "signer_user_id": 1},
			wired: true, want: "lowercase"},
		"no signer":  {args: map[string]any{"digest": digest, "signer": "none", "signer_user_id": 1}, wired: true, want: "names no signer"},
		"no account": {args: map[string]any{"digest": digest, "signer": "org"}, wired: true, want: "names no signer"},
	} {
		t.Run(name, func(t *testing.T) {
			req := &recordingRequester{}
			s := NewRelayServer(db.Stores{Orgs: signSettingsOrgs{base: "https://github.com"}}, ConversationInfo{OrgID: "org-1"}, nil)
			if c.wired {
				s.SetCommitSigning(CommitSigning{ClaimID: "claim-1", BootEpoch: 1, Requester: req})
			}
			_, err := callSignDigest(t, s, c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("DispatchCall = %v, want an error mentioning %q", err, c.want)
			}
			if req.calls != 0 {
				t.Errorf("a refused request was queued %d times, want none", req.calls)
			}
		})
	}
}
