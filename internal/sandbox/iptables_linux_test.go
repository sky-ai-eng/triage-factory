//go:build linux

package sandbox

import (
	"strings"
	"testing"
)

// TestEgressRuleLineMatches pins the field-matching the orphan reaper
// uses to find leaked Part B host-side egress rules. The
// load-bearing properties: the gateway IP must match exactly (not as a
// substring of a longer address), both the bare and "/32" address forms
// must match, the negated "! -d" form iptables actually prints must
// be recognized, and an ACCEPT is ours only in the admitted-port shape.
func TestEgressRuleLineMatches(t *testing.T) {
	const gw = "10.42.7.1"
	cases := []struct {
		name  string
		line  string
		chain string
		want  bool
	}{
		{
			name:  "input_rule_with_mask",
			line:  "-A INPUT -i vh-abcd7 ! -d 10.42.7.1/32 -j DROP",
			chain: "INPUT",
			want:  true,
		},
		{
			name:  "forward_rule_with_mask",
			line:  "-A FORWARD -i vh-abcd7 ! -d 10.42.7.1/32 -j DROP",
			chain: "FORWARD",
			want:  true,
		},
		{
			name:  "bare_address_no_mask",
			line:  "-A INPUT -i vh-abcd7 ! -d 10.42.7.1 -j DROP",
			chain: "INPUT",
			want:  true,
		},
		{
			name:  "sibling_gateway_substring_must_not_match",
			line:  "-A INPUT -i vh-abcd7 ! -d 10.42.7.10/32 -j DROP",
			chain: "INPUT",
			want:  false, // 10.42.7.1 must not substring-match 10.42.7.10
		},
		{
			name:  "different_idx_gateway",
			line:  "-A INPUT -i vh-abcd3 ! -d 10.42.3.1/32 -j DROP",
			chain: "INPUT",
			want:  false,
		},
		{
			name:  "wrong_chain_prefix",
			line:  "-A OUTPUT -i vh-abcd7 ! -d 10.42.7.1/32 -j DROP",
			chain: "INPUT",
			want:  false,
		},
		{
			name:  "not_our_veth",
			line:  "-A INPUT -i eth0 ! -d 10.42.7.1/32 -j DROP",
			chain: "INPUT",
			want:  false,
		},
		{
			name:  "not_a_drop",
			line:  "-A INPUT -i vh-abcd7 ! -d 10.42.7.1/32 -j ACCEPT",
			chain: "INPUT",
			want:  false,
		},
		{
			name:  "gateway_drop",
			line:  "-A INPUT -i vh-abcd7 -d 10.42.7.1/32 -j DROP",
			chain: "INPUT",
			want:  true,
		},
		{
			name:  "admitted_port",
			line:  "-A INPUT -d 10.42.7.1/32 -i vh-abcd7 -p tcp -m tcp --dport 34567 -j ACCEPT",
			chain: "INPUT",
			want:  true,
		},
		{
			name:  "accept_without_port",
			line:  "-A INPUT -i vh-abcd7 -d 10.42.7.1/32 -j ACCEPT",
			chain: "INPUT",
			want:  false,
		},
		{
			name:  "admitted_port_other_gateway",
			line:  "-A INPUT -d 10.42.3.1/32 -i vh-abcd3 -p tcp -m tcp --dport 34567 -j ACCEPT",
			chain: "INPUT",
			want:  false,
		},
		{
			name:  "masquerade_line_ignored",
			line:  "-A POSTROUTING -s 10.42.7.0/24 -o eth0 -j MASQUERADE",
			chain: "INPUT",
			want:  false,
		},
		{
			name:  "empty_line",
			line:  "",
			chain: "INPUT",
			want:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := egressRuleLineMatches(c.line, c.chain, gw); got != c.want {
				t.Errorf("egressRuleLineMatches(%q, %q, %q) = %v, want %v", c.line, c.chain, gw, got, c.want)
			}
		})
	}
}

func TestArgsNameVeth(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"recorded_rule", []string{"-i", "vh-abcd7", "!", "-d", "10.42.7.1", "-j", "DROP"}, true},
		{"listed_rule", strings.Fields("-A INPUT -d 10.42.7.1/32 -i vh-abcd7 -p tcp -m tcp --dport 34567 -j ACCEPT"), true},
		{"other_veth", []string{"-i", "vh-abcd70", "-d", "10.42.7.1", "-j", "DROP"}, false},
		{"output_interface", []string{"-o", "vh-abcd7", "-j", "DROP"}, false},
		{"no_interface", []string{"-d", "10.42.7.1", "-j", "DROP"}, false},
		{"dangling_flag", []string{"-j", "DROP", "-i"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := argsNameVeth(c.args, "vh-abcd7"); got != c.want {
				t.Errorf("argsNameVeth(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}
