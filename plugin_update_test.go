package main

import "testing"

func TestPluginUpdateAvailable(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		published string
		want      bool
	}{
		{"newer published", "0.1.1", "0.1.3", true},
		{"same version", "0.1.3", "0.1.3", false},
		{"installed ahead", "0.2.0", "0.1.3", false},
		{"major bump", "0.9.9", "1.0.0", true},
		{"numeric not lexical", "0.1.9", "0.1.10", true},
		// Equal as semver, different as strings. The naive "!= and >="
		// formulation reports an update here and would nag forever about a
		// plugin that is already current.
		{"semver-equal padded", "0.1", "0.1.0", false},
		{"semver-equal reversed", "0.1.0", "0.1", false},
		{"v-prefixed published", "0.1.1", "v0.1.3", true},
		{"missing installed", "", "0.1.3", false},
		{"missing published", "0.1.1", "", false},
		// Unparseable means unknown, not "update available". A badge no
		// install can satisfy is unactionable and trains people to ignore it.
		{"unparseable installed", "nightly", "0.1.3", false},
		{"unparseable published", "0.1.1", "nightly", false},
	}
	for _, tc := range cases {
		if got := pluginUpdateAvailable(tc.installed, tc.published); got != tc.want {
			t.Errorf("%s: pluginUpdateAvailable(%q,%q) = %v, want %v",
				tc.name, tc.installed, tc.published, got, tc.want)
		}
	}
}
