package gitssh

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestReadPacket(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		raw     string
		payload string
		wantErr bool
	}{
		{name: "flush", in: "0000", raw: "0000"},
		{name: "delim", in: "0001", raw: "0001"},
		{name: "response end", in: "0002", raw: "0002"},
		{name: "empty line", in: "0004", raw: "0004"},
		{name: "payload", in: "000ahello\n", raw: "000ahello\n", payload: "hello\n"},
		// 0003 is not an assigned control packet. Reading it as one would
		// frame every packet after it against the wrong offset.
		{name: "undefined control packet", in: "0003", wantErr: true},
		{name: "non-hex length", in: "zzzz", wantErr: true},
		{name: "truncated body", in: "0010short", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, payload, err := newPktReader(strings.NewReader(tc.in)).readPacket()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("readPacket(%q) = (%q, %q, nil), want an error", tc.in, raw, payload)
				}
				return
			}
			if err != nil {
				t.Fatalf("readPacket(%q): %v", tc.in, err)
			}
			if string(raw) != tc.raw || string(payload) != tc.payload {
				t.Fatalf("readPacket(%q) = (%q, %q), want (%q, %q)", tc.in, raw, payload, tc.raw, tc.payload)
			}
		})
	}

	t.Run("closed stream", func(t *testing.T) {
		// A stream that ends between packets is an ordinary end of session,
		// which the bridge distinguishes from a framing fault by the error.
		if _, _, err := newPktReader(strings.NewReader("")).readPacket(); !errors.Is(err, io.EOF) {
			t.Fatalf("readPacket on a closed stream = %v, want io.EOF", err)
		}
	})
}

func TestReadBlocks(t *testing.T) {
	block := pkt("command=ls-refs\n") + "0001" + pkt("peel\n") + flush

	// A delim-pkt is part of a v2 command, not a terminator.
	got, err := readRawBlock(newPktReader(strings.NewReader(block + "trailing")))
	if err != nil {
		t.Fatalf("readRawBlock: %v", err)
	}
	if string(got) != block {
		t.Fatalf("readRawBlock = %q, want the block through its flush", got)
	}

	// A block that is nothing but its flush carries no request: it is how a v2
	// client says it has no further commands.
	got, err = readRequestBlock(newPktReader(strings.NewReader(flush)))
	if err != nil {
		t.Fatalf("readRequestBlock: %v", err)
	}
	if got != nil {
		t.Fatalf("readRequestBlock on a bare flush = %q, want nil", got)
	}
}

// The bridge only ever talks to a loopback port on this machine, so it dials
// directly rather than through whatever HTTP_PROXY the operator's shell
// exported — which has no route there and no business seeing the run's
// placeholder on the way.
func TestNewClient_DialsDirect(t *testing.T) {
	transport, ok := newClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want an explicit *http.Transport", newClient().Transport)
	}
	if transport.Proxy != nil {
		t.Error("client resolves a proxy from the environment, want a direct dial")
	}
}
