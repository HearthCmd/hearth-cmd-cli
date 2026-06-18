//go:build darwin || linux

package main

// Coverage for httputil.go: serverBaseURL (the wss→https /
// ws→http transform), requestAuthCode + verifyAuthCode + enrollHost
// + deviceAuthedPost against an httptest server. These are the
// network plumbing the CLI uses for enrollment, invites, and revoke.
//
// We're verifying the wire shape (URL, headers, JSON body) the CLI
// sends — those are part of the contract with the server. The
// underlying HTTP client behavior is stdlib and not under test.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withWSURL sets wsURL for the duration of a test and restores it.
func withWSURL(t *testing.T, val string) {
	t.Helper()
	prev := wsURL
	wsURL = val
	t.Cleanup(func() { wsURL = prev })
}

// ---------- serverBaseURL ----------

func TestServerBaseURL_WSStoHTTPS(t *testing.T) {
	withWSURL(t, "wss://hearthcmd.com/ws/relay")
	got, err := serverBaseURL()
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://hearthcmd.com" {
		t.Errorf("expected https://hearthcmd.com, got %q", got)
	}
}

func TestServerBaseURL_WStoHTTP(t *testing.T) {
	withWSURL(t, "ws://localhost:8080/ws/relay")
	got, _ := serverBaseURL()
	if got != "http://localhost:8080" {
		t.Errorf("expected http://localhost:8080, got %q", got)
	}
}

func TestServerBaseURL_EmptyErrors(t *testing.T) {
	withWSURL(t, "")
	if _, err := serverBaseURL(); err == nil {
		t.Error("empty wsURL should error")
	}
}

func TestServerBaseURL_PreservesPort(t *testing.T) {
	withWSURL(t, "wss://staging.hearthcmd.com:8443/ws/relay")
	got, _ := serverBaseURL()
	if got != "https://staging.hearthcmd.com:8443" {
		t.Errorf("expected non-default port preserved, got %q", got)
	}
}

// ---------- requestAuthCode ----------

func TestRequestAuthCode_HappyPath(t *testing.T) {
	var seenBody []byte
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if err := requestAuthCode(srv.URL, "alice@example.com"); err != nil {
		t.Fatalf("requestAuthCode: %v", err)
	}
	if seenPath != "/auth/request" {
		t.Errorf("expected /auth/request, got %q", seenPath)
	}
	var body map[string]string
	_ = json.Unmarshal(seenBody, &body)
	if body["email"] != "alice@example.com" {
		t.Errorf("expected email in body, got %v", body)
	}
}

func TestRequestAuthCode_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	if err := requestAuthCode(srv.URL, "x@example.com"); err == nil {
		t.Error("non-200 should error")
	}
}

// ---------- verifyAuthCode ----------

func TestVerifyAuthCode_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["purpose"] != "enroll_host" {
			t.Errorf("expected purpose=enroll_host, got %q", body["purpose"])
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"session_token": "tok-abc",
			"is_new_user":   true,
		})
	}))
	defer srv.Close()

	tok, isNew, err := verifyAuthCode(srv.URL, "alice@example.com", "123456", "enroll_host")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-abc" || !isNew {
		t.Errorf("got tok=%q isNew=%v", tok, isNew)
	}
}

func TestVerifyAuthCode_BadCodeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid or expired code"}`))
	}))
	defer srv.Close()
	if _, _, err := verifyAuthCode(srv.URL, "x@example.com", "000000", "enroll_host"); err == nil {
		t.Error("400 should surface as error")
	}
}

func TestVerifyAuthCode_EmptyTokenErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"session_token":""}`))
	}))
	defer srv.Close()
	if _, _, err := verifyAuthCode(srv.URL, "x@example.com", "123456", "enroll_host"); err == nil {
		t.Error("empty session token should error even on 200")
	}
}

// ---------- enrollHost ----------

func TestEnrollHost_HappyPath(t *testing.T) {
	var seenAuth, seenPath string
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"host_id":          "host-1",
			"host_secret":      "hs",
			"io_device_id":     "dev-1",
			"io_device_secret": "ds",
			"human_user_id":    "user-1",
			"organization_id":  "org-1",
		})
	}))
	defer srv.Close()

	got, err := enrollHost(srv.URL, "tok-x", "host-1", "alice-mac", "fresh", "Alice", "Alice Co", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.HumanUserID != "user-1" || got.OrganizationID != "org-1" || got.IODeviceID != "dev-1" || got.IODeviceSecret != "ds" || got.HostSecret != "hs" {
		t.Errorf("missing fields in result: %+v", got)
	}
	if seenPath != "/hosts/enroll" {
		t.Errorf("expected /hosts/enroll, got %q", seenPath)
	}
	if seenAuth != "Bearer tok-x" {
		t.Errorf("expected Bearer auth, got %q", seenAuth)
	}
	var body map[string]string
	_ = json.Unmarshal(seenBody, &body)
	if body["mode"] != "fresh" || body["hostname"] != "alice-mac" || body["user_name"] != "Alice" || body["organization_name"] != "Alice Co" {
		t.Errorf("body fields wrong: %v", body)
	}
}

func TestEnrollHost_OmitsBlankOptionalFields(t *testing.T) {
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"host_id":          "h",
			"host_secret":      "hs",
			"io_device_id":     "d",
			"io_device_secret": "ds",
			"human_user_id":    "u",
		})
	}))
	defer srv.Close()
	_, err := enrollHost(srv.URL, "tok", "h", "", "fresh", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.Unmarshal(seenBody, &body)
	if _, present := body["hostname"]; present {
		t.Error("blank hostname should not be sent")
	}
	if _, present := body["user_name"]; present {
		t.Error("blank user_name should not be sent")
	}
	if _, present := body["organization_name"]; present {
		t.Error("blank organization_name should not be sent")
	}
}

func TestEnrollHost_IncompleteResponseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Missing io_device_secret + host_secret + human_user_id
		_, _ = w.Write([]byte(`{"io_device_id":"d"}`))
	}))
	defer srv.Close()
	if _, err := enrollHost(srv.URL, "tok", "h", "", "fresh", "", "", "", ""); err == nil {
		t.Error("incomplete response must error")
	}
}

func TestEnrollHost_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()
	if _, err := enrollHost(srv.URL, "tok", "h", "", "fresh", "", "", "", ""); err == nil {
		t.Error("400 must surface")
	}
}

// ---------- deviceAuthedPost ----------

func TestDeviceAuthedPost_HappyPath(t *testing.T) {
	var seenAuth, seenDev string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenDev = r.Header.Get("X-IO-Device-ID")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	got, err := deviceAuthedPost(srv.URL, "/x", "dev-1", "secret",
		ActionTuple{Kind: "test", Action: "test"},
		map[string]string{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer secret" || seenDev != "dev-1" {
		t.Errorf("auth headers wrong: auth=%q dev=%q", seenAuth, seenDev)
	}
	if !strings.Contains(string(got), `"ok":true`) {
		t.Errorf("body lost: %s", got)
	}
}

func TestDeviceAuthedPost_SurfacesServerErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"specific reason"}`))
	}))
	defer srv.Close()
	_, err := deviceAuthedPost(srv.URL, "/x", "d", "s", ActionTuple{Kind: "test", Action: "test"}, nil)
	if err == nil || !strings.Contains(err.Error(), "specific reason") {
		t.Errorf("expected error to surface server's envelope text, got %v", err)
	}
}

func TestDeviceAuthedPost_FallsBackToStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		// No JSON envelope at all.
	}))
	defer srv.Close()
	_, err := deviceAuthedPost(srv.URL, "/x", "d", "s", ActionTuple{Kind: "test", Action: "test"}, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("expected status code in error, got %v", err)
	}
}

func TestDeviceAuthedPost_NilPayloadOK(t *testing.T) {
	var bodyLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyLen = len(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := deviceAuthedPost(srv.URL, "/x", "d", "s", ActionTuple{Kind: "test", Action: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if bodyLen != 0 {
		t.Errorf("nil payload should produce empty body, got len=%d", bodyLen)
	}
}
