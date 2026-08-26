package gitssh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// The two remote commands git runs over ssh for a fetch and a push. Anything
// else on the org host is refused rather than passed through: passthrough
// would authenticate as the operator, which is the substitution the managed
// channel exists to prevent, and smart HTTP has no equivalent endpoint to
// bridge it onto.
const (
	uploadPackService  = "git-upload-pack"
	receivePackService = "git-receive-pack"
)

// maxErrorBody bounds how much of a non-200 response body is read back for the
// message shown to the agent. The proxy's denials are one short line; a
// misrouted response could be a whole HTML page.
const maxErrorBody = 8 << 10

// bridgeConfig is the dispatcher's whole configuration, read from the run env.
type bridgeConfig struct {
	upstreamHost string
	proxyURL     string
	proxyToken   string
}

// newClient dials the run's proxy directly. The default transport would consult
// HTTP_PROXY/NO_PROXY, and the one address this ever talks to is a loopback
// port on this machine: an inherited proxy setting has no route to it, and
// sending the run's placeholder through a corporate proxy on the way is not a
// thing that should depend on how the operator's shell happens to be
// configured. No client timeout — a fetch's response IS the packfile, and it
// takes as long as the repository is large.
func newClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil}}
}

// bridgeConfigFromEnv reads the run env. ok=false when any part is missing,
// which is how an unmanaged process (an operator's own shell that inherited
// GIT_SSH_COMMAND, a run with no git channel) falls through to plain ssh
// instead of failing: a half-configured bridge has no proxy to reach.
func bridgeConfigFromEnv() (bridgeConfig, bool) {
	cfg := bridgeConfig{
		upstreamHost: strings.TrimSpace(os.Getenv(UpstreamHostEnvVar)),
		proxyURL:     strings.TrimRight(strings.TrimSpace(os.Getenv(ProxyURLEnvVar)), "/"),
		proxyToken:   strings.TrimSpace(os.Getenv(ProxyTokenEnvVar)),
	}
	if cfg.upstreamHost == "" || cfg.proxyURL == "" || cfg.proxyToken == "" {
		return bridgeConfig{}, false
	}
	return cfg, true
}

// session is one bridged git session: where the proxy is, which repository and
// service git asked for, and whether the session runs under protocol v2.
type session struct {
	bridgeConfig
	repoPath string
	service  string
	v2       bool
}

// bridge translates one ssh session onto the run's git proxy. git speaks its
// side of the pkt-line protocol over stdin/stdout throughout; every byte that
// crosses to the proxy does so as an ordinary smart-HTTP request, so the ref
// gate, the credential swap and the push capture all see exactly what they see
// for an HTTPS remote.
func bridge(cfg bridgeConfig, remoteCommand string, stdin io.Reader, stdout io.Writer) error {
	service, repoPath, err := parseRemoteCommand(remoteCommand)
	if err != nil {
		return err
	}
	s := session{bridgeConfig: cfg, repoPath: repoPath, service: service}
	client := newClient()
	switch service {
	case receivePackService:
		return bridgePush(client, s, stdin, stdout)
	case uploadPackService:
		s.v2 = true
		return bridgeFetch(client, s, stdin, stdout)
	}
	return fmt.Errorf("%q is not a git transport command; the managed git channel serves fetch and push only", service)
}

// bridgePush carries a receive-pack session. It is effectively single-round:
// the advertisement comes back from the GET, and git's command block plus its
// packfile become the body of one POST whose response is the report-status.
//
// git downgrades a push to protocol v0 itself (v2 has no push), so this side
// asks for no version and relays what both ends already agree on.
func bridgePush(client *http.Client, s session, stdin io.Reader, stdout io.Writer) error {
	if err := relayAdvertisement(client, s, stdout); err != nil {
		return err
	}

	p := newPktReader(stdin)
	block, packFollows, err := readPushCommands(p)
	if errors.Is(err, io.EOF) {
		// git read the advertisement and closed without sending anything —
		// everything it would have pushed was already there.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read push commands: %w", err)
	}

	var body io.Reader = bytes.NewReader(block)
	streaming := false
	if packFollows {
		// git closes its end of the pipe once pack-objects has written the
		// pack, so reading to EOF is what delimits the request. It does NOT
		// close for a push that carries no pack — it waits for the report —
		// which is why the block alone is the body in that case.
		body = io.MultiReader(bytes.NewReader(block), p.rest())
		streaming = true
	}

	resp, err := post(client, s, body, streaming)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(stdout, resp.Body)
	return err
}

// bridgeFetch carries an upload-pack session under protocol v2, where each of
// the client's commands is one request/response round and the server keeps no
// state between them — which is exactly the stateless-rpc shape smart HTTP
// wants, so a v2 command block maps onto one POST unchanged.
//
// v0 cannot be bridged: its negotiation is stateful, so a stateless-rpc server
// would need the client to resend its wants and haves every round, and a
// client on a stateful transport does not. The spawner pins protocol v2 for
// the run, so this refusal states that precondition rather than naming a path
// a managed run reaches.
func bridgeFetch(client *http.Client, s session, stdin io.Reader, stdout io.Writer) error {
	if !protocolV2Requested() {
		return errors.New("the managed git channel bridges fetch over protocol v2 only; set protocol.version=2")
	}
	if err := relayAdvertisement(client, s, stdout); err != nil {
		return err
	}

	p := newPktReader(stdin)
	for {
		block, err := readRequestBlock(p)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read fetch command: %w", err)
		}
		if block == nil {
			return nil // a bare flush: the client has no further commands
		}
		resp, err := post(client, s, bytes.NewReader(block), false)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(stdout, resp.Body)
		_ = resp.Body.Close()
		if copyErr != nil {
			return copyErr
		}
	}
}

// relayAdvertisement fetches the smart-HTTP ref advertisement and writes it to
// git in the shape its ssh transport expects — which is the same bytes, minus
// the "# service=" header smart HTTP puts in front of them.
func relayAdvertisement(client *http.Client, s session, stdout io.Writer) error {
	u := s.proxyURL + "/" + s.repoPath + ".git/info/refs?service=" + url.QueryEscape(s.service)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/x-"+s.service+"-advertisement")
	prepare(req, s)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("reach the managed git channel: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return err
	}

	p := newPktReader(resp.Body)
	raw, payload, err := firstAdvertisedPacket(p)
	if err != nil {
		return fmt.Errorf("read ref advertisement: %w", err)
	}
	// git has already committed to v2 for this session — it is mid-handshake
	// on our stdout — so an advertisement in another version is not something
	// to adapt to. It is a broken session, named while it can still be named.
	if s.v2 && !bytes.HasPrefix(payload, []byte("version 2")) {
		return errors.New("the managed git channel answered a protocol v2 request with a v0 advertisement")
	}
	if _, err := stdout.Write(raw); err != nil {
		return err
	}
	_, err = io.Copy(stdout, p.rest())
	return err
}

// post sends one service request. A streaming body (a packfile) goes out with
// an unknown length so the transfer is chunked rather than buffered whole.
func post(client *http.Client, s session, body io.Reader, streaming bool) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, s.proxyURL+"/"+s.repoPath+".git/"+s.service, body)
	if err != nil {
		return nil, err
	}
	if streaming {
		req.ContentLength = -1
	}
	req.Header.Set("Content-Type", "application/x-"+s.service+"-request")
	req.Header.Set("Accept", "application/x-"+s.service+"-result")
	prepare(req, s)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the managed git channel: %w", err)
	}
	if err := checkStatus(resp); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// prepare adds the credential the proxy authenticates the local hop with, and
// — for a v2 session — the header that is smart HTTP's spelling of the
// GIT_PROTOCOL variable ssh would have carried.
func prepare(req *http.Request, s session) {
	req.SetBasicAuth(ProxyBasicUser, s.proxyToken)
	if s.v2 {
		req.Header.Set("Git-Protocol", "version=2")
	}
}

// checkStatus turns a non-200 into an error carrying the proxy's own
// explanation. That body is what a refused push or an unauthorized repo says,
// and git surfaces our stderr to the agent verbatim, so relaying it is what
// keeps a ref-gate denial as legible over ssh as it is over HTTPS.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	detail := strings.TrimSpace(string(msg))
	if detail == "" {
		return fmt.Errorf("managed git channel returned %s", resp.Status)
	}
	return fmt.Errorf("managed git channel returned %s: %s", resp.Status, detail)
}

// readPushCommands reads a push's ref-update command block and reports whether
// a packfile follows it. A push that only deletes refs carries no pack, and a
// push carrying none is the one case where reading to EOF would hang.
//
// The block is not only ref updates: a shallow clone prefixes its grafts, and a
// signed push wraps the whole thing in a certificate. Both are relayed with
// everything else and neither is a command, which is why the scan reads what it
// recognizes and keeps the bytes it does not.
//
// Push options, when the client negotiated them, sit between the command block
// and the pack. They ride the packfile stream when there is one; when there is
// not, they have to be read here for the same reason the commands do.
func readPushCommands(p *pktReader) (block []byte, packFollows bool, err error) {
	var buf bytes.Buffer
	var capabilities string
	sawCapabilities := false
	for {
		raw, payload, err := p.readPacket()
		if err != nil {
			return nil, false, err
		}
		buf.Write(raw)
		if bytes.Equal(raw, flushPkt) {
			break
		}
		line := payload
		// Capabilities ride the first line that HAS them, which is the first
		// ref command — not necessarily the first line of the block.
		if i := bytes.IndexByte(line, 0); i >= 0 {
			if !sawCapabilities {
				capabilities = string(line[i+1:])
				sawCapabilities = true
			}
			line = line[:i]
		}
		if fields := strings.Fields(string(line)); len(fields) == 3 && !isZeroOID(fields[1]) {
			packFollows = true
		}
	}
	if !packFollows && hasCapability(capabilities, "push-options") {
		opts, err := readRawBlock(p)
		if err != nil {
			return nil, false, fmt.Errorf("read push options: %w", err)
		}
		buf.Write(opts)
	}
	return buf.Bytes(), packFollows, nil
}

// hasCapability reports whether the capability list names want. Capabilities
// are space-separated and may carry a "=value" suffix.
func hasCapability(capabilities, want string) bool {
	for _, c := range strings.Fields(capabilities) {
		if c == want || strings.HasPrefix(c, want+"=") {
			return true
		}
	}
	return false
}

// protocolV2Requested reports whether git asked for protocol v2 on this
// session. git passes the request in GIT_PROTOCOL — a colon-separated key list
// it would have forwarded to the server over ssh's SendEnv.
func protocolV2Requested() bool {
	for _, part := range strings.Split(os.Getenv("GIT_PROTOCOL"), ":") {
		if strings.TrimSpace(part) == "version=2" {
			return true
		}
	}
	return false
}

// parseRemoteCommand splits the command git asked ssh to run — a service name
// and a shell-quoted repository path — into the service and the "owner/repo"
// the proxy addresses repositories by.
func parseRemoteCommand(remoteCommand string) (service, repoPath string, err error) {
	name, rawPath, found := strings.Cut(strings.TrimSpace(remoteCommand), " ")
	if !found {
		return "", "", fmt.Errorf("unrecognized remote command %q", remoteCommand)
	}
	path, err := unquotePath(strings.TrimSpace(rawPath))
	if err != nil {
		return "", "", fmt.Errorf("remote command %q: %w", remoteCommand, err)
	}
	owner, repo, err := splitOwnerRepo(path)
	if err != nil {
		return "", "", err
	}
	return name, owner + "/" + repo, nil
}

// splitOwnerRepo reduces the path git sends to the owner/repo pair the proxy
// gates on. The ssh:// and scp-like spellings differ only in the leading
// slash, and both may or may not carry the ".git" suffix.
//
// The segments are held to the proxy's own admission rule rather than a looser
// one of our own: this is the side that BUILDS the request path, so a segment
// the proxy would refuse should be refused before it is interpolated into a
// URL — the agent then reads which remote path was wrong instead of a generic
// refusal from the far end of a session it can no longer see.
func splitOwnerRepo(path string) (owner, repo string, err error) {
	trimmed := strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, repo, found := strings.Cut(trimmed, "/")
	if !found || !gitproxy.ValidRepoSegment(owner) || !gitproxy.ValidRepoSegment(repo) {
		return "", "", fmt.Errorf("remote path %q is not an owner/repo on the managed git host", path)
	}
	return owner, repo, nil
}

// unquotePath undoes the shell quoting git applies to the repository path: it
// single-quotes the whole path, and writes an embedded quote by closing the
// quoting, escaping the character, and reopening.
func unquotePath(s string) (string, error) {
	if !strings.HasPrefix(s, "'") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				b.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return "", errors.New("unterminated quote in remote path")
			}
			i++
		case '\\':
			i++
			if i >= len(s) {
				return "", errors.New("trailing escape in remote path")
			}
			b.WriteByte(s[i])
			i++
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), nil
}
