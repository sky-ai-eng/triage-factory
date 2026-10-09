package slackstore

import (
	"reflect"
	"testing"
)

func TestMoveThreadKey(t *testing.T) {
	cases := []struct {
		key, want string
		ok        bool
	}{
		{"G01/1700000000.000100", "C01/1700000000.000100", true},
		{"G019/1700000000.000100", "G019/1700000000.000100", false}, // id that merely starts with the old one
		{"G01", "G01", false},
		{"C02/1700000000.000100", "C02/1700000000.000100", false},
	}
	for _, tc := range cases {
		got, ok := MoveThreadKey(tc.key, "G01", "C01")
		if got != tc.want || ok != tc.ok {
			t.Errorf("MoveThreadKey(%q) = %q, %v; want %q, %v", tc.key, got, ok, tc.want, tc.ok)
		}
	}
	if got, ok := MoveThreadKey("G01/1.0", "G01", "G01"); ok || got != "G01/1.0" {
		t.Errorf("MoveThreadKey onto the same id = %q, %v; want unchanged", got, ok)
	}
}

func TestMoveArtifactKey(t *testing.T) {
	cases := []struct {
		key, want string
		ok        bool
	}{
		{"slack:message:G01/1700000000.000300", "slack:message:C01/1700000000.000300", true},
		{"slack:message:G01/1700000000.000300:anchor", "slack:message:C01/1700000000.000300:anchor", true},
		{"slack:message:G019/1700000000.000300", "slack:message:G019/1700000000.000300", false},
		{"github:comment:G01/1", "github:comment:G01/1", false},
		{"slack:message", "slack:message", false},
	}
	for _, tc := range cases {
		got, ok := MoveArtifactKey(tc.key, "G01", "C01")
		if got != tc.want || ok != tc.ok {
			t.Errorf("MoveArtifactKey(%q) = %q, %v; want %q, %v", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMovePermalink(t *testing.T) {
	cases := []struct {
		name, u, want string
		ok            bool
	}{
		{"root message", "https://acme.slack.com/archives/G01/p1700000000000100",
			"https://acme.slack.com/archives/C01/p1700000000000100", true},
		{"reply keeps its parameters in order", "https://acme.slack.com/archives/G01/p1700000000000300?thread_ts=1700000000.000100&cid=G01",
			"https://acme.slack.com/archives/C01/p1700000000000300?thread_ts=1700000000.000100&cid=C01", true},
		{"channel archive", "https://acme.slack.com/archives/G01",
			"https://acme.slack.com/archives/C01", true},
		{"id that merely starts with the old one", "https://acme.slack.com/archives/G019/p1700000000000100",
			"https://acme.slack.com/archives/G019/p1700000000000100", false},
		{"another channel's cid is not this one's", "https://acme.slack.com/archives/C02/p1?cid=G01",
			"https://acme.slack.com/archives/C02/p1?cid=G01", false},
		{"not a permalink", "https://example.com/G01", "https://example.com/G01", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MovePermalink(tc.u, "G01", "C01")
			if got != tc.want || ok != tc.ok {
				t.Errorf("MovePermalink(%q) = %q, %v; want %q, %v", tc.u, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMoveChannelFilter(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
		ok   bool
	}{
		{"replaced in place", []string{"C09", "G01", "C08"}, []string{"C09", "C01", "C08"}, true},
		{"a list naming both keeps one", []string{"C01", "G01"}, []string{"C01"}, true},
		{"other repeats are left alone", []string{"C09", "C09", "G01"}, []string{"C09", "C09", "C01"}, true},
		{"not named", []string{"C09"}, []string{"C09"}, false},
		{"empty filter", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MoveChannelFilter(tc.in, "G01", "C01")
			if !reflect.DeepEqual(got, tc.want) || ok != tc.ok {
				t.Errorf("MoveChannelFilter(%v) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
