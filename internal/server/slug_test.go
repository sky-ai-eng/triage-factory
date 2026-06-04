package server

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Aidan Allchin", "aidan-allchin"},
		{"  Aidan  Allchin  ", "aidan-allchin"},
		{"O'Brien", "o-brien"},
		{"hello world!!!", "hello-world"},
		{"アイダン", ""}, // no [a-z0-9] survives — caller falls back
		{"aidan@allchin.com", "aidan-allchin-com"},
		{"", ""},
		{"   ", ""},
		{strings.Repeat("a", 100), strings.Repeat("a", 48)},
		{"a-b-c", "a-b-c"},
		{"A_B_C", "a-b-c"},
	}
	for _, tc := range cases {
		got := slugify(tc.in)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
