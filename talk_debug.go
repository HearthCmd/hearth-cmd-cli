//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DEBUG-ONLY: env-gated diagnostic taps for `hearth talk`. See
// docs/debug-flags.md. Grep `DEBUG-ONLY` to find related instrumentation.

// debugLogFrame writes a single labeled line to ~/.hearth/talk-debug.log
// when HEARTH_TALK_DEBUG is set. Used to diagnose issues where transcript
// frames arrive at the TUI but don't render (e.g. empty Event/Text, missing
// relay_id, unexpected shapes). No-op when the env var is unset.
func debugLogFrame(label string, payload []byte) {
	f := openDebugLog()
	if f == nil {
		return
	}
	talkDebugMu.Lock()
	defer talkDebugMu.Unlock()
	fmt.Fprintf(f, "[%s] %s: %s\n", time.Now().Format("15:04:05.000"), label, string(payload))
}

// debugLogf writes a formatted line (no raw frame body) to the debug log.
func debugLogf(format string, args ...interface{}) {
	f := openDebugLog()
	if f == nil {
		return
	}
	talkDebugMu.Lock()
	defer talkDebugMu.Unlock()
	fmt.Fprintf(f, "[%s] "+format+"\n", append([]interface{}{time.Now().Format("15:04:05.000")}, args...)...)
}

var (
	talkDebugMu   sync.Mutex
	talkDebugOnce sync.Once
	talkDebugFile *os.File
)

func openDebugLog() *os.File {
	if os.Getenv("HEARTH_TALK_DEBUG") == "" {
		return nil
	}
	talkDebugOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dir := filepath.Join(home, ".hearth")
		os.MkdirAll(dir, 0755)
		path := filepath.Join(dir, "talk-debug.log")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}
		talkDebugFile = f
	})
	return talkDebugFile
}
