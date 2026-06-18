//go:build integration

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Paths set by TestMain
var (
	hearthBin string // path to compiled hearth binary
	mockClaudeBin string // path to mock claude binary
)

// ---------- test server ----------

type recordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

type testServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// per-endpoint response overrides (path → handler)
	handlers map[string]http.HandlerFunc

	// optional WebSocket handler for /ws/relay
	wsHandlerFn func(w http.ResponseWriter, r *http.Request)
}

func newTestServer() *testServer {
	ts := &testServer{
		handlers: make(map[string]http.HandlerFunc),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrade for /ws/relay
		if r.URL.Path == "/ws/relay" {
			ts.mu.Lock()
			wsh := ts.wsHandlerFn
			ts.mu.Unlock()
			if wsh != nil {
				wsh(w, r)
				return
			}
			w.WriteHeader(404)
			return
		}

		body, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		ts.requests = append(ts.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   body,
		})
		ts.mu.Unlock()

		ts.mu.Lock()
		h, ok := ts.handlers[r.URL.Path]
		ts.mu.Unlock()

		if ok {
			// re-create body for handler since we consumed it
			r.Body = io.NopCloser(bytes.NewReader(body))
			h(w, r)
			return
		}

		// defaults
		switch r.URL.Path {
		case "/session/enroll":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"approved":true}`)
		case "/activity":
			w.WriteHeader(200)
		case "/transcript":
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	})
	ts.Server = httptest.NewServer(mux)
	return ts
}

func (ts *testServer) setHandler(path string, h http.HandlerFunc) {
	ts.mu.Lock()
	ts.handlers[path] = h
	ts.mu.Unlock()
}

func (ts *testServer) setWSHandler(h func(w http.ResponseWriter, r *http.Request)) {
	ts.mu.Lock()
	ts.wsHandlerFn = h
	ts.mu.Unlock()
}

func (ts *testServer) clearHandlers() {
	ts.mu.Lock()
	ts.handlers = make(map[string]http.HandlerFunc)
	ts.wsHandlerFn = nil
	ts.requests = nil
	ts.mu.Unlock()
}

func (ts *testServer) getRequests(path string) []recordedRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []recordedRequest
	for _, r := range ts.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (ts *testServer) allRequests() []recordedRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]recordedRequest, len(ts.requests))
	copy(out, ts.requests)
	return out
}

// wsURL returns ws://host:port/ws/relay for use in -ldflags
func (ts *testServer) wsURL() string {
	return "ws://" + ts.Listener.Addr().String() + "/ws/relay"
}

// baseURL returns http://host:port
func (ts *testServer) baseURL() string {
	return "http://" + ts.Listener.Addr().String()
}

// ---------- helpers ----------

type runResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func run(t *testing.T, args []string, env []string, stdin string) runResult {
	t.Helper()
	return runWithTimeout(t, args, env, stdin, 10*time.Second)
}

func runWithTimeout(t *testing.T, args []string, env []string, stdin string, timeout time.Duration) runResult {
	t.Helper()
	cmd := exec.Command(hearthBin, args...)

	// Start with a clean env, then add what the test needs
	baseEnv := []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"TERM=xterm-256color",
	}
	cmd.Env = append(baseEnv, env...)

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run with timeout
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start hearth: %v", err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				t.Fatalf("unexpected error: %v", err)
			}
		}
		return runResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: code,
		}
	case <-time.After(timeout):
		cmd.Process.Kill()
		t.Fatalf("command timed out after %v; stdout=%q stderr=%q", timeout, stdout.String(), stderr.String())
		return runResult{}
	}
}

// ---------- TestMain ----------

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "hearth-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	// Start test server to get the address for the build
	ts := newTestServer()
	defer ts.Close()
	testServerURL = ts

	// Build hearth binary with the test server URL and version
	hearthBin = filepath.Join(tmpDir, "hearth")
	buildCmd := exec.Command("go", "build",
		"-ldflags", "-X main.wsURL="+ts.wsURL()+" -X main.version=0.0.0-test",
		"-o", hearthBin,
		".",
	)
	buildEnv := []string{"CGO_ENABLED=0"}
	if runtime.GOOS == "darwin" {
		buildEnv = append(buildEnv, "MACOSX_DEPLOYMENT_TARGET=13.0")
	}
	buildCmd.Env = append(os.Environ(), buildEnv...)
	buildCmd.Dir = sourceDir()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build hearth:\n%s\n%v\n", out, err)
		os.Exit(1)
	}

	// Build test plugin binaries (echo, crashy) used by plugin process tests.
	if err := buildTestPlugins(); err != nil {
		fmt.Fprintf(os.Stderr, "build test plugins: %v\n", err)
		os.Exit(1)
	}

	// Build mock claude binary
	mockClaudeBin = filepath.Join(tmpDir, "claude")
	mockCmd := exec.Command("go", "build", "-o", mockClaudeBin, "./testdata/mock_claude.go")
	mockCmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
	)
	mockCmd.Dir = sourceDir()
	if out, err := mockCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build mock claude:\n%s\n%v\n", out, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// testServerURL is shared across tests
var testServerURL *testServer

func sourceDir() string {
	// This test file lives in the repo root
	dir, _ := os.Getwd()
	return dir
}

// ---------- CLI basics ----------

func TestIntegration_NoSubcommand(t *testing.T) {
	r := run(t, nil, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "Usage:") {
		t.Errorf("expected usage text, got stderr=%q", r.Stderr)
	}
	if !strings.Contains(r.Stderr, "0.0.0-test") {
		t.Errorf("expected version in usage text, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_UnknownSubcommand(t *testing.T) {
	r := run(t, []string{"bogus"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "unknown command") {
		t.Errorf("expected 'unknown command', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Help(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			r := run(t, []string{arg}, nil, "")
			if r.ExitCode != 0 {
				t.Errorf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
			}
			if !strings.Contains(r.Stderr, "Usage:") {
				t.Errorf("expected usage text, got stderr=%q", r.Stderr)
			}
			if !strings.Contains(r.Stderr, "0.0.0-test") {
				t.Errorf("expected version in usage text, got stderr=%q", r.Stderr)
			}
		})
	}
}

func TestIntegration_Version(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			r := run(t, []string{arg}, nil, "")
			if r.ExitCode != 0 {
				t.Errorf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
			}
			if !strings.Contains(r.Stderr, "hearth 0.0.0-test") {
				t.Errorf("expected 'hearth 0.0.0-test', got stderr=%q", r.Stderr)
			}
		})
	}
}

// ---------- stream — arg validation ----------

func TestIntegration_Stream_MissingTranscript(t *testing.T) {
	r := run(t, []string{"stream", "--session-id", "s1", "--bridge", "/tmp/b"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for missing --transcript")
	}
}

func TestIntegration_Stream_MissingSessionID(t *testing.T) {
	r := run(t, []string{"stream", "--transcript", "/tmp/t.jsonl", "--bridge", "/tmp/b"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for missing --session-id")
	}
}

func TestIntegration_Stream_MissingServerOrBridge(t *testing.T) {
	r := run(t, []string{"stream", "--transcript", "/tmp/t.jsonl", "--session-id", "s1", "--device-id", "d1"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for missing --server/--bridge")
	}
}

func TestIntegration_Stream_HTTPMode_FatalError(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/transcript", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	})
	defer testServerURL.clearHandlers()

	tmpDir, err := os.MkdirTemp("", "hearth-stream-fatal-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	os.WriteFile(transcriptPath, []byte(`{"type":"msg"}`+"\n"), 0644)

	cmd := exec.Command(hearthBin, "stream",
		"--transcript", transcriptPath,
		"--session-id", "test-fatal-1",
		"--device-id", "test-dev",
		"--project", "test-proj",
		"--relay-id", "relay-fatal-1",
		"--server", testServerURL.baseURL(),
	)
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
	}

	done := make(chan error, 1)
	cmd.Start()
	go func() { done <- cmd.Wait() }()

	// Should exit on its own due to 400 error
	select {
	case <-done:
		// good, streamer exited
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Error("streamer did not exit on fatal 400 error")
	}
}


// ---------- embedded library extraction ----------

func TestIntegration_EmbeddedLib(t *testing.T) {
	p := extractEmbeddedLib()
	if p == "" {
		t.Fatal("extractEmbeddedLib returned empty path")
	}
	defer os.Remove(p)

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("extracted file does not exist at %s: %v", p, err)
	}

	if info.Size() == 0 {
		t.Error("extracted library is empty")
	}
}
