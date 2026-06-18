package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type processState int

const (
	stateInitializing processState = iota
	stateReady
	stateDead
)

const (
	// pluginScanBufMax caps the size of any single frame.
	// 1MB is comfortable for HA-state-style payloads; bump if the
	// real HA registry pulls bigger than that.
	pluginScanBufMax = 1 << 20

	// pluginShutdownGrace is how long we wait for cmd.Wait after
	// sending Shutdown before SIGKILL.
	pluginShutdownGrace = 3 * time.Second
)

// InboundHandler is invoked by the per-process reader goroutine when
// a plugin makes a request *to* the daemon mid-Invoke (phase 3 step 5
// State* RPCs). The supervisor provides a closure that bundles the
// daemon's local DB + the in-flight binding scope.
//
// bindingID is whatever was active on the PluginProcess at the moment
// the inbound frame was dispatched (i.e. the binding the current
// Invoke is scoped to). Empty when no Invoke is active (e.g. during
// Init) — handlers that need a binding should refuse with the
// 'forbidden' code rather than silently using an empty scope.
type InboundHandler func(method string, params json.RawMessage, bindingID string) (result json.RawMessage, code ErrorCode, errMsg string)

// PluginProcess is one live plugin subprocess. Phase 3 step 5
// switched the I/O model to bidirectional JSON-RPC: a per-process
// reader goroutine demuxes inbound frames (responses to daemon→plugin
// requests vs. plugin→daemon requests) so the plugin can call
// StateGet/StatePut etc. mid-Invoke.
type PluginProcess struct {
	connID   string
	slug     string
	manifest PluginManifest

	// mu guards lifecycle state (state, seq) — short critical sections.
	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	state processState
	seq   uint64

	// invokeMu serializes Invokes per-process so currentBindingID
	// stays well-defined for any inbound StateGet/Put while an Invoke
	// is in flight. Held for the whole Invoke duration.
	invokeMu sync.Mutex

	// stdinMu guards stdin writes. Both the Invoke caller and the
	// reader goroutine (replying to plugin-issued requests) write to
	// stdin; serialize so two writers can't interleave frames.
	stdinMu sync.Mutex

	// currentBindingID is the binding scope the in-flight Invoke is
	// running under. Read by the inbound handler; written under
	// invokeMu (atomic write, atomic read from reader goroutine —
	// invokeMu pins it for the call's duration).
	currentBindingIDMu sync.RWMutex
	currentBindingID   string

	// pending correlates daemon→plugin request ids to response
	// channels. The reader goroutine delivers responses here.
	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	// inboundHandler dispatches plugin→daemon requests. Nil-safe:
	// when nil, the reader replies with an "unsupported" error.
	inboundHandler InboundHandler

	// waitDone closes when cmd.Wait returns.
	waitDone chan struct{}
	// readerDone closes when the reader goroutine exits.
	readerDone chan struct{}
	// drainWg is signaled by the stderr forwarder.
	drainWg sync.WaitGroup

	scrubForms [][]byte
}

// StartPlugin spawns the plugin binary, starts stderr/stdout reader
// goroutines, and synchronously sends Init. On failure, tears down
// and returns (nil, err).
func StartPlugin(ctx context.Context, manifest PluginManifest, connID string, inboundHandler InboundHandler) (*PluginProcess, error) {
	if manifest.Executable == "" {
		return nil, fmt.Errorf("plugin %s: manifest has empty executable", manifest.PluginSlug)
	}
	binPath := filepath.Join(manifest.SourceDir, manifest.Executable)
	cmd := exec.Command(binPath)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %s: stdin pipe: %w", manifest.PluginSlug, err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %s: stdout pipe: %w", manifest.PluginSlug, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %s: stderr pipe: %w", manifest.PluginSlug, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin %s: start %s: %w", manifest.PluginSlug, binPath, err)
	}

	p := &PluginProcess{
		connID:         connID,
		slug:           manifest.PluginSlug,
		manifest:       manifest,
		cmd:            cmd,
		stdin:          stdin,
		state:          stateInitializing,
		pending:        map[string]chan rpcResponse{},
		inboundHandler: inboundHandler,
		waitDone:       make(chan struct{}),
		readerDone:     make(chan struct{}),
	}

	p.drainWg.Add(1)
	go p.forwardStderr(stderrPipe)
	go p.waitForExit()
	go p.readLoop(stdoutPipe)

	if err := p.callInit(ctx, connID); err != nil {
		_ = p.killAndWait()
		return nil, err
	}

	p.mu.Lock()
	p.state = stateReady
	p.mu.Unlock()
	return p, nil
}

func (p *PluginProcess) logPrefix() string {
	if p.connID != "" {
		return p.connID
	}
	return p.slug
}

func (p *PluginProcess) forwardStderr(r io.Reader) {
	defer p.drainWg.Done()
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 8*1024), 1<<20)
	for s.Scan() {
		line := scrubBytes(s.Bytes(), p.scrubForms)
		log.Printf("plugin %s: %s", p.logPrefix(), line)
	}
}

func (p *PluginProcess) waitForExit() {
	_ = p.cmd.Wait()
	p.mu.Lock()
	p.state = stateDead
	_ = p.stdin.Close()
	p.mu.Unlock()
	close(p.waitDone)

	// Fail any in-flight Invokes waiting on responses.
	p.pendingMu.Lock()
	for id, ch := range p.pending {
		select {
		case ch <- rpcResponse{ID: id, Error: &rpcError{Code: string(ErrTransport), Message: "plugin process exited"}}:
		default:
		}
		delete(p.pending, id)
	}
	p.pendingMu.Unlock()
}

func (p *PluginProcess) isDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == stateDead
}

// readLoop drains stdout, demuxing each frame into either a response
// (delivered to a pending entry) or an inbound request (dispatched to
// the inbound handler, with the reply written back to stdin).
//
// A single goroutine handles both demux and inbound dispatch — order
// matches the plugin's emission order, which is what the plugin
// author expects. If inbound RPCs become latency-sensitive, fan them
// off; for KV state ops the daemon-local DB is fast enough that the
// in-line handling is fine.
func (p *PluginProcess) readLoop(stdoutPipe io.Reader) {
	defer close(p.readerDone)
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), pluginScanBufMax)
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		// Frames are either rpcRequest (has Method) or rpcResponse (has
		// no Method, has Result/Error). Try generic decode and branch.
		var probe struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *rpcError       `json:"error,omitempty"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			log.Printf("plugin %s: malformed frame, dropping: %v", p.logPrefix(), err)
			continue
		}
		if probe.Method != "" {
			p.handleInbound(probe.ID, probe.Method, probe.Params)
			continue
		}
		// Response.
		p.deliverResponse(rpcResponse{ID: probe.ID, Result: probe.Result, Error: probe.Error})
	}
	if err := scanner.Err(); err != nil {
		log.Printf("plugin %s: reader error: %v", p.logPrefix(), err)
	}
}

func (p *PluginProcess) deliverResponse(resp rpcResponse) {
	p.pendingMu.Lock()
	ch, ok := p.pending[resp.ID]
	if ok {
		delete(p.pending, resp.ID)
	}
	p.pendingMu.Unlock()
	if !ok {
		log.Printf("plugin %s: response for unknown id %q (dropped)", p.logPrefix(), resp.ID)
		return
	}
	// Non-blocking send: pending channel is buffered=1.
	select {
	case ch <- resp:
	default:
		log.Printf("plugin %s: response channel for id %q full (dropped)", p.logPrefix(), resp.ID)
	}
}

func (p *PluginProcess) handleInbound(id, method string, params json.RawMessage) {
	if p.inboundHandler == nil {
		p.writeResponse(rpcResponse{ID: id, Error: &rpcError{Code: string(ErrInternal), Message: "daemon: no inbound handler registered"}})
		return
	}
	p.currentBindingIDMu.RLock()
	binding := p.currentBindingID
	p.currentBindingIDMu.RUnlock()
	result, code, errMsg := p.inboundHandler(method, params, binding)
	if code != "" {
		p.writeResponse(rpcResponse{ID: id, Error: &rpcError{Code: string(code), Message: errMsg}})
		return
	}
	p.writeResponse(rpcResponse{ID: id, Result: result})
}

// writeResponse is the stdin write for replies to plugin-issued
// requests. Guarded by stdinMu to interleave safely with Invoke's
// outbound request writes.
func (p *PluginProcess) writeResponse(resp rpcResponse) {
	line, err := json.Marshal(resp)
	if err != nil {
		log.Printf("plugin %s: marshal inbound reply: %v", p.logPrefix(), err)
		return
	}
	line = append(line, '\n')
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if _, err := p.stdin.Write(line); err != nil {
		// Plugin's stdin is gone; the next Invoke will fail and respawn.
		log.Printf("plugin %s: write inbound reply: %v", p.logPrefix(), err)
	}
}

func (p *PluginProcess) callInit(ctx context.Context, connID string) error {
	params, err := json.Marshal(InitParams{
		ConnectionID: connID,
	})
	if err != nil {
		return &PluginError{Code: ErrInternal, Message: "marshal Init params: " + err.Error()}
	}
	_, err = p.exchange(ctx, "Init", params)
	return err
}

// Invoke sends an Invoke request scoped to bindingID and returns the
// parsed result. bindingID is opaque to the plugin in this layer —
// it's stashed on the process so the reader goroutine can inject it
// into any inbound StateGet/Put requests the plugin makes during the
// call. Empty bindingID is allowed (Shape A connections without a
// binding row); the plugin's State* RPCs will fail in that case.
func (p *PluginProcess) Invoke(ctx context.Context, verb string, args json.RawMessage, secretBindings map[string]string, bindingID string) (InvokeResult, error) {
	params, err := json.Marshal(InvokeParams{
		Verb:           verb,
		Args:           args,
		SecretBindings: secretBindings,
	})
	if err != nil {
		return InvokeResult{}, &PluginError{Code: ErrInternal, Message: "marshal Invoke params: " + err.Error()}
	}
	var perInvokeForms [][]byte
	if len(secretBindings) > 0 {
		vals := make([][]byte, 0, len(secretBindings))
		for _, v := range secretBindings {
			if v != "" {
				vals = append(vals, []byte(v))
			}
		}
		perInvokeForms = computeScrubForms(vals)
	}

	p.invokeMu.Lock()
	defer p.invokeMu.Unlock()
	if p.isDead() {
		return InvokeResult{}, &PluginError{Code: ErrTransport, Message: "plugin process not running"}
	}

	// Stash the binding for the duration of this call so inbound
	// StateGet/Put requests scope correctly. Cleared on return.
	p.currentBindingIDMu.Lock()
	p.currentBindingID = bindingID
	p.currentBindingIDMu.Unlock()
	defer func() {
		p.currentBindingIDMu.Lock()
		p.currentBindingID = ""
		p.currentBindingIDMu.Unlock()
	}()

	raw, err := p.exchange(ctx, "Invoke", params)
	if err != nil {
		return InvokeResult{}, err
	}
	var r InvokeResult
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		p.mu.Lock()
		p.state = stateDead
		p.mu.Unlock()
		return InvokeResult{}, &PluginError{Code: ErrTransport, Message: "decode result: " + uerr.Error()}
	}
	if len(perInvokeForms) > 0 {
		r.Stdout = string(scrubBytes([]byte(r.Stdout), perInvokeForms))
	}
	return r, nil
}

// exchange sends one outbound request and waits for the matching
// response. Concurrency-safe — multiple goroutines can call exchange
// in parallel; the reader goroutine demuxes responses by id.
// Cancellation: ctx.Done removes the pending entry and returns an
// ErrTransport error.
func (p *PluginProcess) exchange(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	p.seq++
	id := strconv.FormatUint(p.seq, 10)
	dead := p.state == stateDead
	p.mu.Unlock()
	if dead {
		return nil, &PluginError{Code: ErrTransport, Message: "plugin process not running"}
	}

	line, err := json.Marshal(rpcRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, &PluginError{Code: ErrInternal, Message: "marshal request: " + err.Error()}
	}
	line = append(line, '\n')

	respCh := make(chan rpcResponse, 1)
	p.pendingMu.Lock()
	p.pending[id] = respCh
	p.pendingMu.Unlock()
	cleanup := func() {
		p.pendingMu.Lock()
		delete(p.pending, id)
		p.pendingMu.Unlock()
	}

	p.stdinMu.Lock()
	_, werr := p.stdin.Write(line)
	p.stdinMu.Unlock()
	if werr != nil {
		cleanup()
		p.mu.Lock()
		p.state = stateDead
		p.mu.Unlock()
		return nil, &PluginError{Code: ErrTransport, Message: "write: " + werr.Error()}
	}

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			code := ErrorCode(resp.Error.Code)
			if !isKnownWireErrorCode(code) && code != ErrTransport {
				log.Printf("plugin %s: unfamiliar error code %q (preserved opaque)", p.logPrefix(), code)
			}
			return nil, &PluginError{Code: code, Message: resp.Error.Message}
		}
		return resp.Result, nil
	case <-ctx.Done():
		cleanup()
		return nil, &PluginError{Code: ErrTransport, Message: "ctx: " + ctx.Err().Error()}
	case <-p.waitDone:
		cleanup()
		return nil, &PluginError{Code: ErrTransport, Message: "plugin process exited"}
	}
}

// Shutdown sends a Shutdown request, then waits up to pluginShutdownGrace
// for the process to exit cleanly. SIGKILLs if the deadline fires.
// Idempotent.
func (p *PluginProcess) Shutdown(ctx context.Context) error {
	if p.isDead() {
		<-p.waitDone
		p.drainWg.Wait()
		<-p.readerDone
		return nil
	}
	// Best-effort send Shutdown; if exchange fails, the kill below handles it.
	_, _ = p.exchange(ctx, "Shutdown", json.RawMessage("null"))
	_ = p.stdin.Close()

	select {
	case <-p.waitDone:
	case <-time.After(pluginShutdownGrace):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-p.waitDone
	}
	p.drainWg.Wait()
	<-p.readerDone
	return nil
}

func (p *PluginProcess) killAndWait() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	<-p.waitDone
	p.drainWg.Wait()
	<-p.readerDone
	return nil
}
