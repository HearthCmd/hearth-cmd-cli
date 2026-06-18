//go:build darwin || linux

package main

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for the message-router branches in daemon_ws.go that
// are pure dispatch logic — they read d.instances + the per-callback
// funcs and don't touch the underlying WSClient. We construct a
// bare-bones DaemonWS (no .ws), wire up the callbacks we want to
// observe, and feed it pre-built control frames.

func newTestDaemonWS() *DaemonWS {
	return &DaemonWS{
		instances: make(map[string]*agentWS),
	}
}

// ---------- routeControlFrame ----------

func TestRouteControlFrame_KillCallsKillFunc(t *testing.T) {
	d := newTestDaemonWS()
	var killed int32
	d.instances["agent-1"] = &agentWS{
		aiAgentInstanceID: "agent-1",
		killFunc:          func() { atomic.AddInt32(&killed, 1) },
	}

	d.routeControlFrame([]byte(`{"type":"kill","ai_agent_instance_id":"agent-1"}`))

	if atomic.LoadInt32(&killed) != 1 {
		t.Errorf("killFunc not called (got %d)", killed)
	}
}

func TestRouteControlFrame_KillUnknownInstanceIsSilent(t *testing.T) {
	d := newTestDaemonWS()
	// No instance registered. Must not panic / not call anything.
	d.routeControlFrame([]byte(`{"type":"kill","ai_agent_instance_id":"ghost"}`))
}

func TestRouteControlFrame_KillWithoutKillFuncIsSilent(t *testing.T) {
	d := newTestDaemonWS()
	d.instances["agent-1"] = &agentWS{aiAgentInstanceID: "agent-1"} // no killFunc
	// Must not panic.
	d.routeControlFrame([]byte(`{"type":"kill","ai_agent_instance_id":"agent-1"}`))
}

func TestRouteControlFrame_RetireDropsLocalInstanceState(t *testing.T) {
	d := newTestDaemonWS()
	d.instances["agent-1"] = &agentWS{aiAgentInstanceID: "agent-1"}
	d.routeControlFrame([]byte(`{"type":"retire_agent_instance","ai_agent_instance_id":"agent-1"}`))

	d.mu.RLock()
	defer d.mu.RUnlock()
	if _, ok := d.instances["agent-1"]; ok {
		t.Error("retire should have removed the instance from d.instances")
	}
}

func TestRouteControlFrame_SleepFiresSleepFunc(t *testing.T) {
	d := newTestDaemonWS()
	var got string
	d.sleepFunc = func(id string) { got = id }

	d.routeControlFrame([]byte(`{"type":"sleep","ai_agent_instance_id":"agent-7"}`))

	if got != "agent-7" {
		t.Errorf("sleepFunc got %q", got)
	}
}

func TestRouteControlFrame_SleepNoCallbackIsSilent(t *testing.T) {
	d := newTestDaemonWS()
	// sleepFunc nil — must not panic.
	d.routeControlFrame([]byte(`{"type":"sleep","ai_agent_instance_id":"x"}`))
}

func TestRouteControlFrame_WakeFiresAsyncWithSpawnContext(t *testing.T) {
	d := newTestDaemonWS()
	type call struct {
		id  string
		ctx json.RawMessage
	}
	ch := make(chan call, 1)
	d.wakeFunc = func(id string, ctx json.RawMessage) {
		ch <- call{id: id, ctx: ctx}
	}

	frame := []byte(`{"type":"wake","ai_agent_instance_id":"agent-2","spawn_context":{"k":"v"}}`)
	d.routeControlFrame(frame)

	select {
	case c := <-ch:
		if c.id != "agent-2" {
			t.Errorf("id = %q", c.id)
		}
		// SpawnContext is whatever the server sent — it's an opaque blob
		// the wake handler unpacks. Round-trip the JSON to compare
		// independently of whitespace.
		var got map[string]string
		if err := json.Unmarshal(c.ctx, &got); err != nil {
			t.Fatalf("ctx unmarshal: %v", err)
		}
		if got["k"] != "v" {
			t.Errorf("ctx = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wakeFunc not called within 2s")
	}
}

func TestRouteControlFrame_MalformedJSONIsSilent(t *testing.T) {
	d := newTestDaemonWS()
	d.sleepFunc = func(string) { t.Fatal("must not invoke sleepFunc on bad JSON") }
	d.routeControlFrame([]byte(`{not json`))
}

func TestRouteControlFrame_UnknownTypeIsSilent(t *testing.T) {
	d := newTestDaemonWS()
	// No callback for "totally_made_up". Function logs and returns;
	// must not invoke any of the wired callbacks.
	d.sleepFunc = func(string) { t.Fatal("sleepFunc should not fire") }
	d.wakeFunc = func(string, json.RawMessage) { t.Fatal("wakeFunc should not fire") }
	d.routeControlFrame([]byte(`{"type":"totally_made_up","ai_agent_instance_id":"x"}`))
}

// ---------- handleTextFrame: identity pushes ----------

func TestHandleTextFrame_AccountInvokesCallback(t *testing.T) {
	d := newTestDaemonWS()
	var (
		gotUser, gotEmail string
		mu                sync.Mutex
	)
	d.accountFunc = func(u, e string) {
		mu.Lock()
		defer mu.Unlock()
		gotUser, gotEmail = u, e
	}

	consumed := d.handleTextFrame([]byte(`{"type":"account","human_user_id":"u-1","email":"a@b"}`))
	if !consumed {
		t.Error("expected handleTextFrame to consume account push")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotUser != "u-1" || gotEmail != "a@b" {
		t.Errorf("got user=%q email=%q", gotUser, gotEmail)
	}
}

func TestHandleTextFrame_OrganizationsListInvokesCallback(t *testing.T) {
	d := newTestDaemonWS()
	var got []daemonOrgEntry
	d.organizationsFunc = func(orgs []daemonOrgEntry) { got = orgs }

	frame := []byte(`{"type":"organizations_list","organizations":[{"id":"o1","name":"Alpha"},{"id":"o2","name":"Beta","is_current":true}]}`)
	consumed := d.handleTextFrame(frame)
	if !consumed {
		t.Error("expected consumed")
	}
	if len(got) != 2 || got[1].ID != "o2" || !got[1].IsCurrent {
		t.Errorf("got %+v", got)
	}
}

func TestHandleTextFrame_ResourceConnectionsChangedInvokesCallback(t *testing.T) {
	d := newTestDaemonWS()
	var fired int32
	d.resourceConnectionsChangedFunc = func() { atomic.AddInt32(&fired, 1) }

	frame := []byte(`{"type":"resource_connections_changed","organization_id":"o","change_kind":"create","connection_id":"echo-test"}`)
	consumed := d.handleTextFrame(frame)
	if !consumed {
		t.Error("expected consumed")
	}
	if atomic.LoadInt32(&fired) != 1 {
		t.Errorf("callback fired %d times; want 1", fired)
	}
}

func TestHandleTextFrame_ResourceConnectionsChangedWithoutCallbackIsSilent(t *testing.T) {
	d := newTestDaemonWS()
	// resourceConnectionsChangedFunc unset — must not panic.
	consumed := d.handleTextFrame([]byte(`{"type":"resource_connections_changed","organization_id":"o","change_kind":"delete","connection_id":"x"}`))
	if !consumed {
		t.Error("frame should still be consumed even if callback unset")
	}
}

func TestHandleTextFrame_NotJSONReturnsFalse(t *testing.T) {
	d := newTestDaemonWS()
	if d.handleTextFrame([]byte("plain text")) {
		t.Error("non-JSON should return false (not consumed)")
	}
	if d.handleTextFrame(nil) {
		t.Error("empty payload should return false")
	}
}

func TestHandleTextFrame_NoInstanceIDFallsThrough(t *testing.T) {
	d := newTestDaemonWS()
	// Valid JSON but no ai_agent_instance_id and not an identity push —
	// returns false so the upstream WSClient handler can try other paths.
	if d.handleTextFrame([]byte(`{"type":"something_else"}`)) {
		t.Error("should not consume frame without instance ID")
	}
}

func TestHandleTextFrame_UnknownInstanceIDFallsThrough(t *testing.T) {
	d := newTestDaemonWS()
	// Has ai_agent_instance_id but no matching registered instance.
	if d.handleTextFrame([]byte(`{"type":"input","ai_agent_instance_id":"ghost","text":"aGk="}`)) {
		t.Error("should return false for unregistered instance")
	}
}

func TestHandleTextFrame_DestroyAgentRoutesToControlFrame(t *testing.T) {
	// Regression: server sends destroy_agent_instance as a JSON text
	// frame on the daemon WS. handleTextFrame must forward to
	// routeControlFrame so the agent's killFunc fires and routeControlFrame
	// proceeds to the on-disk rm-rf. Previously the type was missing from
	// handleTextFrame's routing switch, the frame fell through to the
	// PTY-inject path, hit the "no text/data field" branch, was marked
	// consumed and silently dropped — leaving the workdir on disk despite
	// the server-side cascade.
	d := newTestDaemonWS()
	var killed int32
	d.instances["agent-9"] = &agentWS{
		aiAgentInstanceID: "agent-9",
		killFunc:          func() { atomic.AddInt32(&killed, 1) },
	}

	consumed := d.handleTextFrame([]byte(`{"type":"destroy_agent_instance","ai_agent_instance_id":"agent-9","working_dir":""}`))
	if !consumed {
		t.Error("destroy frame must be consumed")
	}
	if atomic.LoadInt32(&killed) != 1 {
		t.Errorf("killFunc not called via routeControlFrame (got %d)", killed)
	}
	d.mu.RLock()
	_, stillRegistered := d.instances["agent-9"]
	d.mu.RUnlock()
	if stillRegistered {
		t.Error("destroy frame should have unregistered the instance")
	}
}

func TestHandleTextFrame_RetireAgentRoutesToControlFrame(t *testing.T) {
	// Companion to the destroy test — retire_agent_instance also arrives
	// as a daemon-WS text frame and must route to routeControlFrame so
	// the in-memory instance state is dropped. (Server-side DB is SOT,
	// so the consequence of a silent drop was less visible than
	// destroy's surviving workdir, but the routing gap was the same.)
	d := newTestDaemonWS()
	d.instances["agent-10"] = &agentWS{aiAgentInstanceID: "agent-10"}

	consumed := d.handleTextFrame([]byte(`{"type":"retire_agent_instance","ai_agent_instance_id":"agent-10"}`))
	if !consumed {
		t.Error("retire frame must be consumed")
	}
	d.mu.RLock()
	_, stillRegistered := d.instances["agent-10"]
	d.mu.RUnlock()
	if stillRegistered {
		t.Error("retire frame should have unregistered the instance")
	}
}
