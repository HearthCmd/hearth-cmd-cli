//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeOAuthExchanger struct {
	gotProvider string
	gotToken    string
	gotConnID   string
}

func (f *fakeOAuthExchanger) ExchangeOAuthToken(_ context.Context, provider string, refreshToken []byte, connectionID string) (string, int, error) {
	f.gotProvider = provider
	f.gotToken = string(refreshToken)
	f.gotConnID = connectionID
	return "access-for-" + provider, 3600, nil
}

// The provider slug the daemon asks the server for comes from the credential
// TYPE, not from a hard-coded list. This is what lets an OAuth plugin ship
// without a CLI release: the relay's registry is keyed on the same string.
func TestExpandCredentials_DerivesProviderSlugFromType(t *testing.T) {
	for _, tc := range []struct{ credType, wantProvider string }{
		{"oauth2_google", "google"},
		{"oauth2_sonos", "sonos"},
		{"oauth2_some_future_thing", "some_future_thing"},
	} {
		t.Run(tc.credType, func(t *testing.T) {
			ex := NewDeclarativeExecutor()
			fake := &fakeOAuthExchanger{}
			ex.SetOAuthExchanger(fake)

			out, err := ex.expandCredentials(context.Background(),
				[]PluginCredential{{Name: "user_token", Type: tc.credType, Secret: true}},
				map[string]string{"user_token": "refresh-" + tc.wantProvider},
				nil, "rc-123")
			if err != nil {
				t.Fatalf("expandCredentials: %v", err)
			}
			if fake.gotProvider != tc.wantProvider {
				t.Errorf("provider = %q; want %q", fake.gotProvider, tc.wantProvider)
			}
			// The connection id has to reach the server: for a
			// bring-your-own-app upstream it is how the relay finds which
			// client id to refresh as, and where to write a rotated token.
			if fake.gotConnID != "rc-123" {
				t.Errorf("connectionID = %q; want %q", fake.gotConnID, "rc-123")
			}
			got, _ := out["user_token"].(map[string]any)
			if got["access_token"] != "access-for-"+tc.wantProvider {
				t.Errorf("access_token = %v; want the exchanged token", got["access_token"])
			}
		})
	}
}

// A credential with no type is a flat secret ({{credentials.x}} → the raw
// value). It must pass straight through, not be mistaken for an OAuth one —
// this is how Home Assistant's ha_token and GitHub's github_token work.
func TestExpandCredentials_FlatSecretUntouched(t *testing.T) {
	ex := NewDeclarativeExecutor()
	fake := &fakeOAuthExchanger{}
	ex.SetOAuthExchanger(fake)

	out, err := ex.expandCredentials(context.Background(),
		[]PluginCredential{{Name: "ha_token", Secret: true}},
		map[string]string{"ha_token": "llat-abc"},
		nil, "rc-123")
	if err != nil {
		t.Fatalf("expandCredentials: %v", err)
	}
	if out["ha_token"] != "llat-abc" {
		t.Errorf("ha_token = %v; want the raw secret", out["ha_token"])
	}
	if fake.gotProvider != "" {
		t.Errorf("a typeless credential triggered an OAuth exchange for %q", fake.gotProvider)
	}
}

// The server acts on credential-spec fields the daemon has no model for —
// today the `oauth:` block that tells the relay where to send the user. A
// typed re-marshal dropped those, which put a CLI release in front of every
// new credential attribute. Parse must keep the block verbatim.
func TestParseManifest_ForwardsUnknownCredentialFields(t *testing.T) {
	m, err := ParseManifest([]byte(`
plugin_slug: verge_labs/sonos
display_name: Sonos
version: 0.1.0
manifest_schema: 2
credentials:
  - name: user_token
    type: oauth2_sonos
    secret: true
    oauth:
      authorize_url: https://api.sonos.com/login/v3/oauth
      token_url: https://api.sonos.com/login/v3/oauth/access
      client_auth: basic
    scopes:
      - playback-control-all
`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	// The typed view still works for what the daemon itself needs.
	if len(m.Credentials) != 1 || m.Credentials[0].Type != "oauth2_sonos" {
		t.Fatalf("typed credentials = %+v", m.Credentials)
	}

	b, err := json.Marshal(credentialSpecSource(m))
	if err != nil {
		t.Fatalf("marshal credential specs: %v", err)
	}
	var specs []struct {
		Type  string `json:"type"`
		OAuth struct {
			AuthorizeURL string `json:"authorize_url"`
			TokenURL     string `json:"token_url"`
			ClientAuth   string `json:"client_auth"`
		} `json:"oauth"`
	}
	if err := json.Unmarshal(b, &specs); err != nil {
		t.Fatalf("unmarshal reported specs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("reported %d credential specs; want 1", len(specs))
	}
	if specs[0].OAuth.AuthorizeURL != "https://api.sonos.com/login/v3/oauth" ||
		specs[0].OAuth.TokenURL != "https://api.sonos.com/login/v3/oauth/access" ||
		specs[0].OAuth.ClientAuth != "basic" {
		t.Errorf("oauth block did not survive the report: %+v", specs[0].OAuth)
	}
}

// A manifest built in memory rather than parsed (how most tests and any
// programmatic caller make one) has no raw block; the typed fallback keeps it
// reporting rather than silently sending nothing.
func TestCredentialSpecSource_FallsBackToTyped(t *testing.T) {
	m := PluginManifest{Credentials: []PluginCredential{{Name: "tok", Secret: true}}}
	src := credentialSpecSource(m)
	if src == nil {
		t.Fatal("expected the typed credentials to be reported")
	}
	if credentialSpecSource(PluginManifest{}) != nil {
		t.Error("a manifest with no credentials should report nothing at all")
	}
}
