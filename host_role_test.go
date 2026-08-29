//go:build darwin || linux

package main

import (
	"reflect"
	"testing"
)

func TestApplyHostRoleChange(t *testing.T) {
	cases := []struct {
		name    string
		current []string
		op, arg string
		want    []string
	}{
		{"add display to agent", []string{"agent"}, "add", "display", []string{"agent", "display"}},
		{"add is idempotent", []string{"agent", "display"}, "add", "display", []string{"agent", "display"}},
		{"add agent to display", []string{"display"}, "add", "agent", []string{"display", "agent"}},
		{"remove display leaves agent", []string{"agent", "display"}, "remove", "display", []string{"agent"}},
		{"remove missing role is a no-op", []string{"agent"}, "remove", "display", []string{"agent"}},
		{"remove the only role yields empty (caller refuses)", []string{"display"}, "remove", "display", []string{}},
		{"dedup preserved on add", []string{"agent", "agent"}, "add", "display", []string{"agent", "display"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyHostRoleChange(c.current, c.op, c.arg)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("applyHostRoleChange(%v, %q, %q) = %v, want %v", c.current, c.op, c.arg, got, c.want)
			}
		})
	}
}
