package main

import "testing"

// semverParts/semverGTE had no test coverage at all, which is how the
// v-prefix bug survived: hearth's own release binaries carry a v-prefixed
// version (scripts/build.sh force-prefixes it), so semverParts("v1.0.2")
// returned nil and semverGTE reported "below floor" for every comparison
// against our own version. Plugin min_daemon_version therefore failed on
// release builds, not merely on dev builds.

func TestSemverParts_StripsVPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []int
	}{
		{"plain", "1.0.2", []int{1, 0, 2}},
		{"released hearth binary", "v1.0.2", []int{1, 0, 2}},
		{"capitalized", "V1.0.2", []int{1, 0, 2}},
		{"surrounding space", " v1.0.2 ", []int{1, 0, 2}},
		{"missing components pad with zero", "v0.128", []int{0, 128, 0}},
		{"trailing junk tolerated", "v1.2.3-rc1", []int{1, 2, 3}},
		{"multi-digit components", "v2.10.30", []int{2, 10, 30}},
	}
	for _, tc := range cases {
		got := semverParts(tc.in)
		if got == nil {
			t.Errorf("%s: semverParts(%q) = nil, want %v", tc.name, tc.in, tc.want)
			continue
		}
		if len(got) != 3 || got[0] != tc.want[0] || got[1] != tc.want[1] || got[2] != tc.want[2] {
			t.Errorf("%s: semverParts(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestSemverParts_NilOnUnparseable(t *testing.T) {
	// "dev" is what cli-build.sh injects for dev/local builds. It must parse
	// as nil so callers can distinguish "no version info" from a real number:
	// the plugin path turns that into fail-open (see checkMinDaemonVersion),
	// while harness checks keep treating it as not-meeting-floor.
	for _, in := range []string{"dev", "", "v", "vdev", "abc"} {
		if got := semverParts(in); got != nil {
			t.Errorf("semverParts(%q) = %v, want nil", in, got)
		}
	}
}

func TestSemverGTE_VPrefixedSelfVersion(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		floor     string
		want      bool
	}{
		{"released binary meets floor", "v1.0.2", "1.0.0", true},
		{"equal satisfies", "v1.0.2", "1.0.2", true},
		{"genuinely below is still refused", "v1.0.2", "1.0.3", false},
		{"prefix on the floor side", "1.0.2", "v1.0.0", true},
		{"numeric not lexical", "v1.0.10", "v1.0.9", true},
		{"unparseable stays fail-closed here", "dev", "1.0.0", false},
		{"empty floor means no requirement", "v1.0.2", "", true},
	}
	for _, tc := range cases {
		if got := semverGTE(tc.installed, tc.floor); got != tc.want {
			t.Errorf("%s: semverGTE(%q,%q) = %v, want %v",
				tc.name, tc.installed, tc.floor, got, tc.want)
		}
	}
}
