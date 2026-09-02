package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadProposeItems(t *testing.T) {
	const arr = `[{"op":"grant","primitive":"rule"}]`

	// --items-file wins and reads the file.
	dir := t.TempDir()
	path := filepath.Join(dir, "items.json")
	if err := os.WriteFile(path, []byte(arr), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readProposeItems(path, `["inline"]`, os.Stdin); err != nil || got != arr {
		t.Fatalf("items-file: got %q err %v, want file contents", got, err)
	}

	// A bad path errors rather than silently falling through.
	if _, err := readProposeItems(filepath.Join(dir, "nope.json"), arr, os.Stdin); err == nil {
		t.Fatal("want error for missing --items-file path")
	}

	// Inline is used when no file is given.
	if got, err := readProposeItems("", arr, os.Stdin); err != nil || got != arr {
		t.Fatalf("inline: got %q err %v", got, err)
	}

	// Piped stdin is read when neither file nor inline is given.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = w.WriteString(arr); w.Close() }()
	if got, err := readProposeItems("", "", r); err != nil || got != arr {
		t.Fatalf("stdin: got %q err %v", got, err)
	}
}

func TestBuildProposeOnboardingPayload(t *testing.T) {
	if _, err := buildProposeOnboardingPayload("", "", "", "", ""); err == nil {
		t.Fatal("want error for empty --items")
	}
	if _, err := buildProposeOnboardingPayload(`{"op":"x"}`, "", "", "", ""); err == nil {
		t.Fatal("want error for non-array --items")
	}
	if _, err := buildProposeOnboardingPayload(`[]`, "", "", "", ""); err == nil {
		t.Fatal("want error for empty array")
	}

	p, err := buildProposeOnboardingPayload(`[{"op":"grant","primitive":"rule"}]`, "why", "pos-1", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p["intent_kind"] != "entity" {
		t.Errorf("intent_kind = %v, want entity", p["intent_kind"])
	}
	if p["rationale"] != "why" {
		t.Errorf("rationale = %v", p["rationale"])
	}
	if p["intent_target_id"] != "pos-1" {
		t.Errorf("intent_target_id = %v", p["intent_target_id"])
	}
	items, ok := p["items"].([]json.RawMessage)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %v, want one raw item", p["items"])
	}

	// Optional fields are omitted when empty (not sent as empty strings).
	p2, _ := buildProposeOnboardingPayload(`[{"op":"advisory","primitive":"advisory"}]`, "", "", "", "")
	if _, has := p2["rationale"]; has {
		t.Error("rationale should be omitted when empty")
	}
	if _, has := p2["intent_target_id"]; has {
		t.Error("intent_target_id should be omitted when empty")
	}
}

// Provenance rides along only when a blueprint was actually named. A version
// with no slug would say nothing, so it never travels alone.
func TestProposeOnboardingPayloadCarriesProvenance(t *testing.T) {
	const items = `[{"op":"grant","primitive":"rule"}]`

	p, err := buildProposeOnboardingPayload(items, "", "", "verge_labs/dj", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if p["source_blueprint"] != "verge_labs/dj" || p["source_blueprint_version"] != "0.1.0" {
		t.Fatalf("payload = %+v", p)
	}

	// A slug with no version is fine — the slug alone still answers "where did
	// this come from?".
	p2, _ := buildProposeOnboardingPayload(items, "", "", "verge_labs/dj", "")
	if p2["source_blueprint"] != "verge_labs/dj" {
		t.Fatalf("payload = %+v", p2)
	}
	if _, ok := p2["source_blueprint_version"]; ok {
		t.Fatalf("emitted an empty version: %+v", p2)
	}

	// A version with no slug is meaningless and must not be sent.
	p3, _ := buildProposeOnboardingPayload(items, "", "", "", "0.1.0")
	for _, k := range []string{"source_blueprint", "source_blueprint_version"} {
		if _, ok := p3[k]; ok {
			t.Fatalf("emitted %s for a proposal that named no blueprint: %+v", k, p3)
		}
	}

	// Nothing named: neither key appears, so an ordinary proposal is unchanged.
	p4, _ := buildProposeOnboardingPayload(items, "why", "pos-1", "", "")
	for _, k := range []string{"source_blueprint", "source_blueprint_version"} {
		if _, ok := p4[k]; ok {
			t.Fatalf("unprompted %s on a plain proposal: %+v", k, p4)
		}
	}
}
