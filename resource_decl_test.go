package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- substitute() ----------

func TestSubstitute_BasicPaths(t *testing.T) {
	scope := map[string]any{
		"config":      map[string]any{"ha_url": "http://ha.local:8123"},
		"credentials": map[string]any{"ha_token": "secret"},
		"args":        map[string]any{"entity_id": "light.kitchen"},
	}
	out, err := substitute("{{config.ha_url}}/api/states/{{args.entity_id}}", scope)
	if err != nil {
		t.Fatalf("substitute: %v", err)
	}
	want := "http://ha.local:8123/api/states/light.kitchen"
	if out != want {
		t.Errorf("substitute() = %q; want %q", out, want)
	}
}

func TestSubstitute_TrimsWhitespaceInBraces(t *testing.T) {
	scope := map[string]any{"args": map[string]any{"k": "v"}}
	got, err := substitute("{{  args.k  }}", scope)
	if err != nil || got != "v" {
		t.Errorf("substitute(spaces) = %q, %v", got, err)
	}
}

func TestSubstitute_UnknownPathIsError(t *testing.T) {
	scope := map[string]any{"args": map[string]any{}}
	_, err := substitute("{{args.missing}}", scope)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "args.missing") {
		t.Errorf("error should mention path: %v", err)
	}
}

func TestSubstitute_NumberStringifies(t *testing.T) {
	scope := map[string]any{
		"args": map[string]any{
			"n_int":     42,
			"n_float":   3.14,
			"n_intlike": float64(7), // JSON-decoded ints come through as float64
		},
	}
	cases := map[string]string{
		"{{args.n_int}}":     "42",
		"{{args.n_float}}":   "3.14",
		"{{args.n_intlike}}": "7",
	}
	for tmpl, want := range cases {
		got, err := substitute(tmpl, scope)
		if err != nil {
			t.Errorf("substitute(%q): %v", tmpl, err)
			continue
		}
		if got != want {
			t.Errorf("substitute(%q) = %q; want %q", tmpl, got, want)
		}
	}
}

func TestSubstitute_DomainFunctionCall(t *testing.T) {
	scope := map[string]any{
		"args": map[string]any{"entity_id": "light.kitchen_main"},
	}
	got, err := substitute("/api/services/{{domain(args.entity_id)}}/turn_on", scope)
	if err != nil {
		t.Fatalf("substitute: %v", err)
	}
	want := "/api/services/light/turn_on"
	if got != want {
		t.Errorf("substitute() = %q; want %q", got, want)
	}
}

func TestSubstitute_UnknownFunctionIsError(t *testing.T) {
	_, err := substitute("{{not_a_helper(args.x)}}", map[string]any{"args": map[string]any{"x": "y"}})
	if err == nil || !strings.Contains(err.Error(), "unknown function") {
		t.Errorf("expected unknown-function error; got %v", err)
	}
}

func TestSubstitute_UnterminatedBracesError(t *testing.T) {
	_, err := substitute("hello {{world", map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------- jsonPathExtract() ----------

func TestJSONPath_ScalarLeaf(t *testing.T) {
	body := []byte(`{"state":"on","attributes":{"brightness":255}}`)
	got, err := jsonPathExtract(body, "$.state")
	if err != nil || got != "on" {
		t.Errorf("$.state = %q, %v", got, err)
	}
	got, err = jsonPathExtract(body, "$.attributes.brightness")
	if err != nil || got != "255" {
		t.Errorf("$.attributes.brightness = %q, %v", got, err)
	}
}

func TestJSONPath_ObjectRemarshals(t *testing.T) {
	body := []byte(`{"attrs":{"a":1,"b":"x"}}`)
	got, err := jsonPathExtract(body, "$.attrs")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Order-tolerant — encoding/json preserves source key order, but
	// we just want a parse-equivalence check.
	var m map[string]any
	if jerr := json.Unmarshal([]byte(got), &m); jerr != nil {
		t.Fatalf("got = %q, not JSON: %v", got, jerr)
	}
	if m["a"].(float64) != 1 || m["b"].(string) != "x" {
		t.Errorf("remarshaled value lost data: %+v", m)
	}
}

func TestJSONPath_MissingPathIsError(t *testing.T) {
	body := []byte(`{"state":"on"}`)
	_, err := jsonPathExtract(body, "$.attributes.brightness")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestJSONPath_RootDollarReturnsRaw(t *testing.T) {
	body := []byte(`{"anything":true}`)
	got, err := jsonPathExtract(body, "$")
	if err != nil || got != string(body) {
		t.Errorf("$ = %q, %v", got, err)
	}
}

// ---------- mapStatusToCode ----------

func TestMapStatusToCode(t *testing.T) {
	cases := map[int]ErrorCode{
		401: ErrUnauthorized,
		403: ErrForbidden,
		404: ErrBadArgs,
		400: ErrBadArgs,
		429: ErrBadArgs,
		500: ErrUnavailable,
		503: ErrUnavailable,
		301: ErrInternal, // odd; surface
	}
	for code, want := range cases {
		if got := mapStatusToCode(code); got != want {
			t.Errorf("mapStatusToCode(%d) = %s; want %s", code, got, want)
		}
	}
}

// ---------- Executor.Invoke against an httptest.Server ----------

func newDeclTestExecutor() *DeclarativeExecutor {
	return &DeclarativeExecutor{client: &http.Client{Timeout: 5 * time.Second}}
}

func TestInvoke_GET_URLSubstitution_JSONPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states/light.kitchen" {
			t.Errorf("url path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"state":"on","attributes":{"brightness":255}}`)
	}))
	defer srv.Close()

	res, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
		Spec: &VerbHTTPSpec{
			Method: "GET",
			URL:    "{{config.ha_url}}/api/states/{{args.entity_id}}",
			Headers: map[string]string{
				"Authorization": "Bearer {{credentials.ha_token}}",
			},
			Response: VerbHTTPResponse{Output: "$.state"},
		},
		Config:      map[string]any{"ha_url": srv.URL},
		Credentials: map[string]any{"ha_token": "secret"},
		Args:        map[string]any{"entity_id": "light.kitchen"},
	})
	if perr != nil {
		t.Fatalf("Invoke: %v", perr)
	}
	if res.Stdout != "on" {
		t.Errorf("Stdout = %q; want on", res.Stdout)
	}
}

func TestInvoke_POST_BodySubstitution_LiteralOutput(t *testing.T) {
	var sawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		sawBody = string(raw)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	res, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
		Spec: &VerbHTTPSpec{
			Method: "POST",
			URL:    "{{config.ha_url}}/api/services/light/turn_on",
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body:     `{"entity_id": "{{args.entity_id}}"}`,
			Response: VerbHTTPResponse{Output: "ok"},
		},
		Config: map[string]any{"ha_url": srv.URL},
		Args:   map[string]any{"entity_id": "light.kitchen"},
	})
	if perr != nil {
		t.Fatalf("Invoke: %v", perr)
	}
	if res.Stdout != "ok" {
		t.Errorf("Stdout = %q; want literal ok", res.Stdout)
	}
	if sawBody != `{"entity_id": "light.kitchen"}` {
		t.Errorf("upstream body = %q", sawBody)
	}
}

func TestInvoke_EmptyOutputReturnsRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `whatever`)
	}))
	defer srv.Close()
	res, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
		Spec:   &VerbHTTPSpec{Method: "GET", URL: srv.URL},
		Config: map[string]any{},
	})
	if perr != nil {
		t.Fatalf("Invoke: %v", perr)
	}
	if res.Stdout != "whatever" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
}

func TestInvoke_StatusMaps(t *testing.T) {
	cases := []struct {
		status   int
		wantCode ErrorCode
	}{
		{401, ErrUnauthorized},
		{403, ErrForbidden},
		{404, ErrBadArgs},
		{500, ErrUnavailable},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d", c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, "upstream said no")
			}))
			defer srv.Close()
			_, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
				Spec: &VerbHTTPSpec{Method: "GET", URL: srv.URL},
			})
			if perr == nil {
				t.Fatal("expected PluginError")
			}
			if perr.Code != c.wantCode {
				t.Errorf("code = %s; want %s", perr.Code, c.wantCode)
			}
			if !strings.Contains(perr.Message, "upstream said no") {
				t.Errorf("message should include body snippet: %s", perr.Message)
			}
		})
	}
}

func TestInvoke_SuccessStatusOverride(t *testing.T) {
	// Treat 202 as success when the manifest declares it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(202)
		_, _ = io.WriteString(w, "accepted")
	}))
	defer srv.Close()
	res, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
		Spec: &VerbHTTPSpec{
			Method:   "POST",
			URL:      srv.URL,
			Response: VerbHTTPResponse{SuccessStatus: []int{200, 202}},
		},
	})
	if perr != nil {
		t.Fatalf("Invoke: %v", perr)
	}
	if res.Stdout != "accepted" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
}

func TestInvoke_NetworkErrorMapsUnavailable(t *testing.T) {
	// Closed listener → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	_, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
		Spec: &VerbHTTPSpec{Method: "GET", URL: srv.URL},
	})
	if perr == nil {
		t.Fatal("expected error from dead server")
	}
	if perr.Code != ErrUnavailable {
		t.Errorf("code = %s; want %s", perr.Code, ErrUnavailable)
	}
}

func TestInvoke_ContextCancelMapsInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, perr := newDeclTestExecutor().Invoke(ctx, DeclarativeInvokeInput{
		Spec: &VerbHTTPSpec{Method: "GET", URL: srv.URL},
	})
	if perr == nil {
		t.Fatal("expected error from canceled context")
	}
	if perr.Code != ErrInternal {
		t.Errorf("code = %s; want %s (context-cancel is local fault, not upstream)", perr.Code, ErrInternal)
	}
}

func TestInvoke_MissingTemplateKeyIsBadArgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be hit when template substitution fails")
	}))
	defer srv.Close()
	_, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
		Spec: &VerbHTTPSpec{
			Method: "GET",
			URL:    "{{config.ha_url}}/api/states/{{args.entity_id}}",
		},
		Config: map[string]any{"ha_url": srv.URL},
		Args:   map[string]any{}, // entity_id missing
	})
	if perr == nil {
		t.Fatal("expected error for missing template key")
	}
	if perr.Code != ErrBadArgs {
		t.Errorf("code = %s; want %s", perr.Code, ErrBadArgs)
	}
}

// ---------- jsonPathIterate ----------

func TestJSONPathIterate_TopLevelArray(t *testing.T) {
	items, err := jsonPathIterate([]byte(`[{"a":1},{"a":2}]`), "$[*]")
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d; want 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["a"].(float64) != 1 {
		t.Errorf("items[0].a = %v", first["a"])
	}
}

func TestJSONPathIterate_NestedField(t *testing.T) {
	items, err := jsonPathIterate([]byte(`{"results":[{"id":"x"},{"id":"y"}]}`), "$.results[*]")
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
}

func TestJSONPathIterate_MissingStarSuffix(t *testing.T) {
	_, err := jsonPathIterate([]byte(`[]`), "$.results")
	if err == nil {
		t.Fatal("expected error for path without [*] suffix")
	}
}

func TestJSONPathIterate_LeafNotArray(t *testing.T) {
	_, err := jsonPathIterate([]byte(`{"results":"not-an-array"}`), "$.results[*]")
	if err == nil {
		t.Fatal("expected error for non-array leaf")
	}
}

// ---------- entityIDDomain ----------

func TestEntityIDDomain(t *testing.T) {
	cases := map[string]string{
		"light.kitchen":           "light",
		"lock.front_door":         "lock",
		"sensor.kitchen_humidity": "sensor",
		"no_dot_at_all":           "no_dot_at_all",
		"":                        "",
	}
	for in, want := range cases {
		if got := entityIDDomain(in); got != want {
			t.Errorf("entityIDDomain(%q) = %q; want %q", in, got, want)
		}
	}
}

// ---------- RunSnapshot ----------

func haSnapshotSpec(serverURL string) *SnapshotSpec {
	return &SnapshotSpec{
		HTTP: VerbHTTPSpec{
			Method: "GET",
			URL:    "{{config.ha_url}}/api/states",
			Headers: map[string]string{
				"Authorization": "Bearer {{credentials.ha_token}}",
			},
		},
		Extract: ExtractSpec{
			Iterate:  "$[*]",
			EntityID: "$.entity_id",
			Kind:     ExtractKindSpec{PrefixWithEntityDomain: "ha."},
			Labels: map[string]string{
				"friendly_name": "$.attributes.friendly_name",
				"area":          "$.attributes.area_id",
			},
		},
	}
}

func TestRunSnapshot_HAShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer ha-secret" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `[
			{"entity_id":"light.kitchen","attributes":{"friendly_name":"Kitchen","area_id":"kitchen"}},
			{"entity_id":"lock.front_door","attributes":{"friendly_name":"Front Door"}},
			{"entity_id":"scene.movie","attributes":{}}
		]`)
	}))
	defer srv.Close()

	ents, perr := newDeclTestExecutor().RunSnapshot(context.Background(), DeclarativeSnapshotInput{
		Spec:        haSnapshotSpec(srv.URL),
		Config:      map[string]any{"ha_url": srv.URL},
		Credentials: map[string]any{"ha_token": "ha-secret"},
	})
	if perr != nil {
		t.Fatalf("RunSnapshot: %v", perr)
	}
	if len(ents) != 3 {
		t.Fatalf("entity count = %d; want 3", len(ents))
	}

	want := []Entity{
		{EntityID: "light.kitchen", Kind: "ha.light", Labels: map[string]string{"friendly_name": "Kitchen", "area": "kitchen"}},
		{EntityID: "lock.front_door", Kind: "ha.lock", Labels: map[string]string{"friendly_name": "Front Door"}},
		{EntityID: "scene.movie", Kind: "ha.scene", Labels: map[string]string{}},
	}
	for i, w := range want {
		got := ents[i]
		if got.EntityID != w.EntityID {
			t.Errorf("entities[%d].EntityID = %q; want %q", i, got.EntityID, w.EntityID)
		}
		if got.Kind != w.Kind {
			t.Errorf("entities[%d].Kind = %q; want %q", i, got.Kind, w.Kind)
		}
		if len(got.Labels) != len(w.Labels) {
			t.Errorf("entities[%d].Labels = %v; want %v", i, got.Labels, w.Labels)
			continue
		}
		for k, v := range w.Labels {
			if got.Labels[k] != v {
				t.Errorf("entities[%d].Labels[%q] = %q; want %q", i, k, got.Labels[k], v)
			}
		}
	}
}

func TestRunSnapshot_LiteralKindForSlackShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"channels":[
			{"id":"C01","name":"general"},
			{"id":"C02","name":"random"}
		]}`)
	}))
	defer srv.Close()

	spec := &SnapshotSpec{
		HTTP: VerbHTTPSpec{Method: "GET", URL: srv.URL + "/channels"},
		Extract: ExtractSpec{
			Iterate:  "$.channels[*]",
			EntityID: "$.id",
			Kind:     ExtractKindSpec{Literal: "slack.channel"},
			Labels:   map[string]string{"name": "$.name"},
		},
	}
	ents, perr := newDeclTestExecutor().RunSnapshot(context.Background(), DeclarativeSnapshotInput{Spec: spec})
	if perr != nil {
		t.Fatalf("RunSnapshot: %v", perr)
	}
	if len(ents) != 2 || ents[0].Kind != "slack.channel" || ents[0].EntityID != "C01" {
		t.Errorf("entities = %+v", ents)
	}
}

func TestRunSnapshot_JSONPathKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"x","kind":"gdrive.file","parent_id":"folder:1"}]`)
	}))
	defer srv.Close()

	spec := &SnapshotSpec{
		HTTP: VerbHTTPSpec{Method: "GET", URL: srv.URL},
		Extract: ExtractSpec{
			Iterate:  "$[*]",
			EntityID: "$.id",
			Kind:     ExtractKindSpec{JSONPath: "$.kind"},
			Parent:   "$.parent_id",
		},
	}
	ents, perr := newDeclTestExecutor().RunSnapshot(context.Background(), DeclarativeSnapshotInput{Spec: spec})
	if perr != nil {
		t.Fatalf("RunSnapshot: %v", perr)
	}
	if len(ents) != 1 || ents[0].Kind != "gdrive.file" || ents[0].Parent != "folder:1" {
		t.Errorf("entities = %+v", ents)
	}
}

func TestRunSnapshot_MissingLabelsSilentlyOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"entity_id":"sensor.kitchen","attributes":{}}]`)
	}))
	defer srv.Close()

	spec := &SnapshotSpec{
		HTTP: VerbHTTPSpec{Method: "GET", URL: srv.URL},
		Extract: ExtractSpec{
			Iterate:  "$[*]",
			EntityID: "$.entity_id",
			Kind:     ExtractKindSpec{PrefixWithEntityDomain: "ha."},
			Labels: map[string]string{
				"area":          "$.attributes.area_id",
				"friendly_name": "$.attributes.friendly_name",
			},
		},
	}
	ents, perr := newDeclTestExecutor().RunSnapshot(context.Background(), DeclarativeSnapshotInput{Spec: spec})
	if perr != nil {
		t.Fatalf("RunSnapshot: %v", perr)
	}
	if len(ents) != 1 {
		t.Fatalf("len = %d", len(ents))
	}
	if len(ents[0].Labels) != 0 {
		t.Errorf("labels should be empty when all paths miss; got %+v", ents[0].Labels)
	}
}

func TestRunSnapshot_UpstreamErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	_, perr := newDeclTestExecutor().RunSnapshot(context.Background(), DeclarativeSnapshotInput{
		Spec: &SnapshotSpec{
			HTTP: VerbHTTPSpec{Method: "GET", URL: srv.URL},
			Extract: ExtractSpec{
				Iterate:  "$[*]",
				EntityID: "$.id",
				Kind:     ExtractKindSpec{Literal: "x"},
			},
		},
	})
	if perr == nil || perr.Code != ErrUnauthorized {
		t.Errorf("expected unauthorized; got %v", perr)
	}
}

func TestInvoke_OversizedBodyRejected(t *testing.T) {
	// Stream more than declarativeMaxRespBody bytes; executor caps.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 1 MiB + 16 bytes — just past the cap.
		blob := strings.Repeat("a", declarativeMaxRespBody+16)
		_, _ = io.WriteString(w, blob)
	}))
	defer srv.Close()
	_, perr := newDeclTestExecutor().Invoke(context.Background(), DeclarativeInvokeInput{
		Spec: &VerbHTTPSpec{Method: "GET", URL: srv.URL},
	})
	if perr == nil {
		t.Fatal("expected size-cap error")
	}
	if perr.Code != ErrUnavailable {
		t.Errorf("code = %s; want %s", perr.Code, ErrUnavailable)
	}
}
