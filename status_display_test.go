//go:build darwin || linux

package main

import (
	"os"
	"strings"
	"testing"
)

// The THIS HOST section surfaces the display subsystem: a bind failure is called out
// loudly (with the remediation), an active one shows where it's serving, and a
// non-display host shows no display line at all.
func TestPrintThisHost_DisplayState(t *testing.T) {
	render := func(ident *ipcResponse) string {
		f, err := os.CreateTemp(t.TempDir(), "status-*")
		if err != nil {
			t.Fatal(err)
		}
		printThisHostSection(f, ident, nil)
		_ = f.Close()
		b, _ := os.ReadFile(f.Name())
		return string(b)
	}

	failed := render(&ipcResponse{DisplayBind: "0.0.0.0:8090", DisplayError: "listen tcp :8090: bind: address already in use"})
	if !strings.Contains(failed, "FAILED to serve on 0.0.0.0:8090") || !strings.Contains(failed, "display_bind") {
		t.Fatalf("a failed display bind must be surfaced with remediation:\n%s", failed)
	}

	active := render(&ipcResponse{DisplayBind: "0.0.0.0:8090", DisplayActive: true})
	if !strings.Contains(active, "display: serving on 0.0.0.0:8090") {
		t.Fatalf("an active display must show its bind:\n%s", active)
	}

	none := render(&ipcResponse{})
	if strings.Contains(none, "display:") {
		t.Fatalf("a non-display host must show no display line:\n%s", none)
	}
}
