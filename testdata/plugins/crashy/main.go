// Crashy test plugin for the resource-plugin supervisor's backoff
// test. The Init handler immediately exits with status 1, simulating
// a plugin that's permanently broken at startup. Used by
// TestSupervisor_BackoffCap to verify the exponential-backoff
// schedule kicks in and doesn't hot-loop.
//
// Standalone package main; no dependency on the parent module.
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

type rpcRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !scanner.Scan() {
		// Stdin closed before any request. Treat as crash.
		os.Exit(1)
	}
	var req rpcRequest
	_ = json.Unmarshal(scanner.Bytes(), &req)
	// Whatever the first request is (Init in practice), exit non-zero.
	os.Exit(1)
}
