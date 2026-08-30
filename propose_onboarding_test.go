package main

import (
	"encoding/json"
	"testing"
)

func TestBuildProposeOnboardingPayload(t *testing.T) {
	if _, err := buildProposeOnboardingPayload("", "", ""); err == nil {
		t.Fatal("want error for empty --items")
	}
	if _, err := buildProposeOnboardingPayload(`{"op":"x"}`, "", ""); err == nil {
		t.Fatal("want error for non-array --items")
	}
	if _, err := buildProposeOnboardingPayload(`[]`, "", ""); err == nil {
		t.Fatal("want error for empty array")
	}

	p, err := buildProposeOnboardingPayload(`[{"op":"grant","primitive":"rule"}]`, "why", "pos-1")
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
	p2, _ := buildProposeOnboardingPayload(`[{"op":"advisory","primitive":"advisory"}]`, "", "")
	if _, has := p2["rationale"]; has {
		t.Error("rationale should be omitted when empty")
	}
	if _, has := p2["intent_target_id"]; has {
		t.Error("intent_target_id should be omitted when empty")
	}
}
