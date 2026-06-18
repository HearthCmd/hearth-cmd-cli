//go:build darwin || linux

package main

import (
	"strings"
	"testing"
	"time"
)

// Pure helpers in status.go: org-selection precedence, ID truncation,
// human-readable durations and last-seen renderings.

func TestCurrentOrg(t *testing.T) {
	t.Run("nil when empty", func(t *testing.T) {
		if got := currentOrg(nil); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("returns the IsCurrent entry", func(t *testing.T) {
		orgs := []daemonOrgEntry{
			{ID: "a", IsCurrent: false},
			{ID: "b", IsCurrent: true},
			{ID: "c", IsCurrent: false},
		}
		got := currentOrg(orgs)
		if got == nil || got.ID != "b" {
			t.Errorf("expected b, got %+v", got)
		}
	})

	t.Run("falls back to first entry when none flagged", func(t *testing.T) {
		orgs := []daemonOrgEntry{{ID: "x"}, {ID: "y"}}
		got := currentOrg(orgs)
		if got == nil || got.ID != "x" {
			t.Errorf("expected x (first), got %+v", got)
		}
	})
}

func TestCurrentOrgID(t *testing.T) {
	if got := currentOrgID(nil); got != "" {
		t.Errorf("nil orgs should yield empty, got %q", got)
	}
	if got := currentOrgID([]daemonOrgEntry{{ID: "z", IsCurrent: true}}); got != "z" {
		t.Errorf("got %q", got)
	}
	// Fallback path — first entry's ID even when nothing is current.
	if got := currentOrgID([]daemonOrgEntry{{ID: "first"}, {ID: "second"}}); got != "first" {
		t.Errorf("got %q", got)
	}
}

func TestShortID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"abcdefgh", "abcdefgh"},
		{"abcdefghi", "abcdefgh"},
		{"abcdefghijklmnop", "abcdefgh"},
	}
	for _, c := range cases {
		if got := shortID(c.in); got != c.want {
			t.Errorf("shortID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{1 * time.Minute, "1m"},
		{45 * time.Minute, "45m"},
		{1 * time.Hour, "1h 0m"},
		{1*time.Hour + 23*time.Minute, "1h 23m"},
		{23 * time.Hour, "23h 0m"},
		{24 * time.Hour, "1d 0h"},
		{2*24*time.Hour + 4*time.Hour, "2d 4h"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestHumanLastSeen(t *testing.T) {
	t.Run("empty → never", func(t *testing.T) {
		if got := humanLastSeen(""); got != "never" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("garbage → input passes through", func(t *testing.T) {
		if got := humanLastSeen("not a timestamp"); got != "not a timestamp" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("future timestamp → just now", func(t *testing.T) {
		ts := time.Now().Add(5 * time.Minute).Format(time.RFC3339)
		if got := humanLastSeen(ts); got != "just now" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a few minutes ago", func(t *testing.T) {
		ts := time.Now().Add(-3 * time.Minute).Format(time.RFC3339)
		got := humanLastSeen(ts)
		if !strings.HasSuffix(got, "m ago") {
			t.Errorf("got %q, want '<n>m ago'", got)
		}
	})

	t.Run("hours ago", func(t *testing.T) {
		ts := time.Now().Add(-5 * time.Hour).Format(time.RFC3339)
		got := humanLastSeen(ts)
		if !strings.HasSuffix(got, "h ago") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("days ago", func(t *testing.T) {
		ts := time.Now().Add(-3 * 24 * time.Hour).Format(time.RFC3339)
		got := humanLastSeen(ts)
		if !strings.HasSuffix(got, "d ago") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("very old falls back to a literal date", func(t *testing.T) {
		ts := time.Now().AddDate(0, -2, 0).Format(time.RFC3339)
		got := humanLastSeen(ts)
		// Literal date format YYYY-MM-DD = 10 chars, 2 dashes.
		if len(got) != 10 || strings.Count(got, "-") != 2 {
			t.Errorf("expected YYYY-MM-DD literal date, got %q", got)
		}
	})
}

func TestExtractVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1.2.3", "v1.2.3"},
		{"v0.9", "v0.9"},
		{"hearth-cmd v1.0.0 (build x)", "v1.0.0"},
		{"no version here", ""},
		{"first v1.0.0 wins over v2.0.0", "v1.0.0"},
		{"v01.02.03", "v01.02.03"},
	}
	for _, c := range cases {
		if got := extractVersion(c.in); got != c.want {
			t.Errorf("extractVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
