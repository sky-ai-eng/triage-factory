package gitssh

import (
	"bytes"
	"strings"
	"testing"
)

// The dispatcher routes on the host alone, so every spelling of the org host
// git can hand it — scp-like, ssh:// with a user, an explicit port stripped by
// git before it builds the argv, a differently-cased hostname — has to reach
// the bridge, and any other host has to reach ssh.
func TestBridgedCommand_RoutesOnHost(t *testing.T) {
	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: "http://127.0.0.1:1", proxyToken: "tok"}
	const cmd = "git-receive-pack '/acme/widgets.git'"

	bridging := map[string][]string{
		"scp-like":       {"git@ghe.example.com", cmd},
		"ssh url":        {"-o", "SendEnv=GIT_PROTOCOL", "git@ghe.example.com", cmd},
		"explicit port":  {"-p", "2222", "git@ghe.example.com", cmd},
		"no user":        {"ghe.example.com", cmd},
		"mixed case":     {"git@GHE.Example.COM", cmd},
		"ipv4 preferred": {"-4", "-p", "22", "git@ghe.example.com", cmd},
	}
	for name, args := range bridging {
		t.Run(name, func(t *testing.T) {
			got, ok := bridgedCommand(args, cfg, true)
			if !ok {
				t.Fatalf("bridgedCommand(%q) = not bridged, want bridged", args)
			}
			if got != cmd {
				t.Fatalf("remote command = %q, want %q", got, cmd)
			}
		})
	}

	passthrough := map[string][]string{
		"foreign host":    {"git@gitlab.com", cmd},
		"host suffix":     {"git@evil-ghe.example.com.attacker.test", cmd},
		"too few args":    {"git@ghe.example.com"},
		"no args at all":  {},
		"foreign with -p": {"-p", "2222", "git@bitbucket.org", cmd},
	}
	for name, args := range passthrough {
		t.Run(name, func(t *testing.T) {
			if _, ok := bridgedCommand(args, cfg, true); ok {
				t.Fatalf("bridgedCommand(%q) = bridged, want passthrough", args)
			}
		})
	}

	t.Run("unconfigured env", func(t *testing.T) {
		if _, ok := bridgedCommand([]string{"git@ghe.example.com", cmd}, bridgeConfig{}, false); ok {
			t.Fatal("bridged with no bridge configuration, want passthrough")
		}
	})
}

// git probes an ssh command it does not recognize with -G before trusting it
// with OpenSSH options. Answering that probe successfully is what keeps an
// explicit port working, so it must not fall through to the routing arms.
func TestRun_AnswersVariantProbe(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-G", "-p", "22", "127.0.0.1"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("probe wrote output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestParseRemoteCommand(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		service string
		repo    string
		wantErr bool
	}{
		{name: "ssh url push", in: "git-receive-pack '/acme/widgets.git'", service: receivePackService, repo: "acme/widgets"},
		{name: "scp-like fetch", in: "git-upload-pack 'acme/widgets.git'", service: uploadPackService, repo: "acme/widgets"},
		{name: "no dot-git suffix", in: "git-upload-pack '/acme/widgets'", service: uploadPackService, repo: "acme/widgets"},
		{name: "unquoted path", in: "git-upload-pack /acme/widgets.git", service: uploadPackService, repo: "acme/widgets"},
		{name: "no path", in: "git-upload-pack", wantErr: true},
		{name: "not owner/repo", in: "git-upload-pack '/widgets.git'", wantErr: true},
		{name: "nested path", in: "git-upload-pack '/acme/team/widgets.git'", wantErr: true},
		// The proxy admits one alphabet for a repository segment, and this is
		// the side that builds the path it will parse, so anything outside it
		// is refused here rather than interpolated into a URL.
		{name: "traversal", in: "git-upload-pack '/acme/../etc.git'", wantErr: true},
		{name: "percent escape", in: "git-upload-pack '/acme/wid%2fgets.git'", wantErr: true},
		{name: "quote in segment", in: `git-upload-pack '/acme/wid'\''gets.git'`, wantErr: true},
		{name: "space in segment", in: "git-upload-pack '/acme/wid gets.git'", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, repo, err := parseRemoteCommand(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRemoteCommand(%q) = (%q, %q, nil), want an error", tc.in, service, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRemoteCommand(%q): %v", tc.in, err)
			}
			if service != tc.service || repo != tc.repo {
				t.Fatalf("parseRemoteCommand(%q) = (%q, %q), want (%q, %q)", tc.in, service, repo, tc.service, tc.repo)
			}
		})
	}
}

// A command the proxy has no smart-HTTP endpoint for must fail rather than
// reach the org host some other way: falling back to ssh there is exactly the
// operator-credential push this dispatcher exists to prevent.
func TestBridge_RefusesNonTransportCommand(t *testing.T) {
	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: "http://127.0.0.1:1", proxyToken: "tok"}
	err := bridge(cfg, "git-upload-archive '/acme/widgets.git'", strings.NewReader(""), &bytes.Buffer{})
	if err == nil {
		t.Fatal("bridge accepted git-upload-archive, want a refusal")
	}
	if !strings.Contains(err.Error(), "fetch and push only") {
		t.Fatalf("error = %v, want it to name the supported services", err)
	}
}

// git single-quotes the repository path and writes an embedded quote by
// closing the quoting, escaping the character and reopening — a shape the
// segment alphabet then refuses, but one the unquoting has to get right to
// refuse the real path rather than a mangled one.
func TestUnquotePath(t *testing.T) {
	cases := map[string]string{
		"'/acme/widgets.git'":     "/acme/widgets.git",
		"'acme/widgets.git'":      "acme/widgets.git",
		"/acme/widgets.git":       "/acme/widgets.git",
		`'/acme/wid'\''gets.git'`: "/acme/wid'gets.git",
	}
	for in, want := range cases {
		got, err := unquotePath(in)
		if err != nil {
			t.Errorf("unquotePath(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("unquotePath(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := unquotePath("'/acme/widgets.git"); err == nil {
		t.Error("unquotePath accepted an unterminated quote, want an error")
	}
}
