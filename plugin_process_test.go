package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildTestPlugins() error {
	for _, name := range []string{"echo", "crashy"} {
		src := filepath.Join("testdata", "plugins", name, "main.go")
		out := filepath.Join("testdata", "plugins", name, "hearth-plugin-"+name)
		cmd := exec.Command("go", "build", "-o", out, src)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s: %w", name, err)
		}
	}
	return nil
}

// echoManifest returns a manifest pointing at the freshly-built
// testdata/plugins/echo binary, with SourceDir set to its absolute
// path so the supervisor can spawn it from anywhere.
func echoManifest(t *testing.T) PluginManifest {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "plugins", "echo"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return PluginManifest{
		PluginSlug:     "echo",
		DisplayName:    "Echo",
		Version:        "0.0.1",
		ManifestSchema: 1,
		Executable:     "./hearth-plugin-echo",
		SourceDir:      abs,
	}
}

func TestProcess_InitInvokeShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	proc, err := StartPlugin(ctx, echoManifest(t), "echo-test", nil)
	if err != nil {
		t.Fatalf("StartPlugin: %v", err)
	}
	defer proc.Shutdown(context.Background())

	result, err := proc.Invoke(ctx, "echo", json.RawMessage(`{"hi":"there"}`), nil, "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Stdout != `{"hi":"there"}` {
		t.Errorf("Stdout = %q; want %q", result.Stdout, `{"hi":"there"}`)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d; want 0", result.ExitCode)
	}
	if proc.isDead() {
		t.Error("process should be alive after successful Invoke")
	}
}

func TestProcess_MultipleInvokesReuseProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proc, err := StartPlugin(ctx, echoManifest(t), "echo-test", nil)
	if err != nil {
		t.Fatalf("StartPlugin: %v", err)
	}
	defer proc.Shutdown(context.Background())

	for i := 0; i < 3; i++ {
		_, err := proc.Invoke(ctx, "echo", json.RawMessage(`{}`), nil, "")
		if err != nil {
			t.Fatalf("Invoke %d: %v", i, err)
		}
	}
	if proc.isDead() {
		t.Error("process should still be alive after 3 invokes")
	}
}

func TestProcess_PluginError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proc, err := StartPlugin(ctx, echoManifest(t), "echo-test", nil)
	if err != nil {
		t.Fatalf("StartPlugin: %v", err)
	}
	defer proc.Shutdown(context.Background())

	_, err = proc.Invoke(ctx, "fail", nil, nil, "")
	pe, ok := err.(*PluginError)
	if !ok {
		t.Fatalf("expected *PluginError, got %T: %v", err, err)
	}
	if pe.Code != ErrInternal {
		t.Errorf("Code = %q; want internal", pe.Code)
	}
	// A plugin-reported error must NOT kill the process; we should
	// be able to call again.
	if proc.isDead() {
		t.Fatal("process should remain alive after plugin-reported error")
	}
	if _, err := proc.Invoke(ctx, "echo", json.RawMessage(`{}`), nil, ""); err != nil {
		t.Errorf("subsequent Invoke after plugin error failed: %v", err)
	}
}

func TestProcess_TransportErrorOnExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proc, err := StartPlugin(ctx, echoManifest(t), "echo-test", nil)
	if err != nil {
		t.Fatalf("StartPlugin: %v", err)
	}

	_, err = proc.Invoke(ctx, "exit", nil, nil, "")
	pe, ok := err.(*PluginError)
	if !ok {
		t.Fatalf("expected *PluginError, got %T: %v", err, err)
	}
	if pe.Code != ErrTransport {
		t.Errorf("Code = %q; want transport", pe.Code)
	}

	// Process should be marked dead. Wait goroutine may take a
	// moment to update state.
	deadline := time.Now().Add(2 * time.Second)
	for !proc.isDead() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !proc.isDead() {
		t.Error("process should be marked dead after exit verb")
	}
}

func TestProcess_StderrForwarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logBuf := captureLog(t)
	proc, err := StartPlugin(ctx, echoManifest(t), "echo-test", nil)
	if err != nil {
		t.Fatalf("StartPlugin: %v", err)
	}
	defer proc.Shutdown(context.Background())

	_, err = proc.Invoke(ctx, "log_stderr", json.RawMessage(`{}`), nil, "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Stderr forwarding is async; give the forwarder a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := logBuf.String()
		if strings.Contains(s, "plugin echo-test:") && strings.Contains(s, "echo plugin stderr marker") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("expected stderr marker in log; got:\n%s", logBuf.String())
}

func TestProcess_ShutdownIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proc, err := StartPlugin(ctx, echoManifest(t), "echo-test", nil)
	if err != nil {
		t.Fatalf("StartPlugin: %v", err)
	}
	if err := proc.Shutdown(context.Background()); err != nil {
		t.Errorf("first Shutdown: %v", err)
	}
	// Second call must not panic or block forever.
	if err := proc.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
	if !proc.isDead() {
		t.Error("process should be dead after Shutdown")
	}
}
