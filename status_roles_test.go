//go:build darwin || linux

package main

import "testing"

func TestFormatHostRoles(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "agent"},
		{[]string{}, "agent"},
		{[]string{"agent"}, "agent"},
		{[]string{"display"}, "display"},
		{[]string{"agent", "display"}, "agent,display"},
	}
	for _, c := range cases {
		if got := formatHostRoles(c.in); got != c.want {
			t.Errorf("formatHostRoles(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
