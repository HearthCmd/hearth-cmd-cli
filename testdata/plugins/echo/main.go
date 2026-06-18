// Echo test plugin for the resource-plugin supervisor.
//
// Standalone package main; intentionally has no dependency on the
// parent hearth-cmd-cli module — it's compiled into a separate
// binary by the supervisor tests' TestMain build dance and launched
// as a subprocess.
//
// Implements the JSON-RPC protocol from
// hearth-cmd/docs/external-resource-adapters.md §"Daemon ↔ plugin
// RPC", with three exercise-the-supervisor verbs:
//
//   echo       round-trip the args back as the stdout result
//   fail       return a structured error (code=internal)
//   exit       os.Exit(1) without responding — simulates a crash
//   log_stderr write a marker line to stderr, then echo
//
// Shutdown returns success then exits cleanly. Unknown method or
// verb returns a structured error.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type rpcRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     string      `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type invokeParams struct {
	Verb           string            `json:"verb"`
	Args           json.RawMessage   `json:"args,omitempty"`
	SecretBindings map[string]string `json:"secret_bindings,omitempty"`
}

type invokeResult struct {
	Stdout   string `json:"stdout"`
	ExitCode int    `json:"exit_code"`
}

type initParams struct {
	ConnectionID string `json:"connection_id"`
}

// initState holds what Init received so the `whoami` verb can
// reflect it.
var initState struct {
	ConnectionID string `json:"connection_id"`
}

func main() {
	out := bufio.NewWriter(os.Stdout)
	in := bufio.NewReader(os.Stdin)
	// Match the daemon-side 1MB cap so large requests don't truncate.
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			respond(out, rpcResponse{
				ID:    "",
				Error: &rpcError{Code: "bad_args", Message: "malformed request: " + err.Error()},
			})
			continue
		}

		switch req.Method {
		case "Init":
			var p initParams
			if err := json.Unmarshal(req.Params, &p); err == nil {
				initState.ConnectionID = p.ConnectionID
			}
			respond(out, rpcResponse{ID: req.ID, Result: map[string]bool{"ok": true}})

		case "Invoke":
			handleInvoke(out, req)

		case "Shutdown":
			respond(out, rpcResponse{ID: req.ID, Result: map[string]bool{"ok": true}})
			out.Flush()
			os.Exit(0)

		default:
			respond(out, rpcResponse{
				ID:    req.ID,
				Error: &rpcError{Code: "bad_args", Message: "unknown method: " + req.Method},
			})
		}
	}
}

func handleInvoke(out *bufio.Writer, req rpcRequest) {
	var p invokeParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		respond(out, rpcResponse{
			ID:    req.ID,
			Error: &rpcError{Code: "bad_args", Message: "bad invoke params: " + err.Error()},
		})
		return
	}

	switch p.Verb {
	case "echo":
		respond(out, rpcResponse{
			ID:     req.ID,
			Result: invokeResult{Stdout: string(p.Args), ExitCode: 0},
		})

	case "fail":
		respond(out, rpcResponse{
			ID:    req.ID,
			Error: &rpcError{Code: "internal", Message: "fail verb"},
		})

	case "exit":
		// Crash without responding. Supervisor should see EOF on
		// stdout and surface ErrTransport.
		os.Exit(1)

	case "log_stderr":
		fmt.Fprintln(os.Stderr, "echo plugin stderr marker")
		respond(out, rpcResponse{
			ID:     req.ID,
			Result: invokeResult{Stdout: string(p.Args), ExitCode: 0},
		})

	case "whoami":
		// Reflect the connection id the plugin received at Init.
		// Credentials no longer arrive at Init — see echo_secrets
		// for per-invoke secret bindings reflection.
		stdoutBytes, _ := json.Marshal(initState)
		respond(out, rpcResponse{
			ID:     req.ID,
			Result: invokeResult{Stdout: string(stdoutBytes), ExitCode: 0},
		})

	case "echo_secrets":
		// Reflect the per-invoke SecretBindings map back as the result
		// stdout. Dogfooding aid: the daemon scrubs cleartexts (and
		// encoded variants) from this stream before relaying, so the
		// expected output is the env-var names with *** values.
		stdoutBytes, _ := json.Marshal(p.SecretBindings)
		respond(out, rpcResponse{
			ID:     req.ID,
			Result: invokeResult{Stdout: string(stdoutBytes), ExitCode: 0},
		})

	default:
		respond(out, rpcResponse{
			ID:    req.ID,
			Error: &rpcError{Code: "bad_args", Message: "unknown verb: " + p.Verb},
		})
	}
}

func respond(w io.Writer, resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		// Marshal failure on a fixed shape is a programming bug.
		fmt.Fprintf(os.Stderr, "echo plugin: response marshal error: %v\n", err)
		return
	}
	b = append(b, '\n')
	w.Write(b)
	if bw, ok := w.(*bufio.Writer); ok {
		bw.Flush()
	}
}
