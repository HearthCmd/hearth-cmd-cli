//go:build darwin || linux

package main

// Unit coverage for the transcript ordering key (seq/sub) stamped host-side —
// the foundation of the wire sequence number (docs/transcript-sequence-number-spec.md).
// The load-bearing property is that the live tail and the replay path assign
// IDENTICAL seq to the same source line; these tests pin the pieces that make
// that true (stampSeq, transformLineWithSeq, readLastNLinesIndexed's baseIndex).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStampSeq(t *testing.T) {
	t.Run("injects seq+sub and preserves existing fields", func(t *testing.T) {
		out := stampSeq([]byte(`{"type":"text","text":"hi","uuid":"abc"}`), 7, 2)
		var got struct {
			Type string `json:"type"`
			Text string `json:"text"`
			UUID string `json:"uuid"`
			Seq  int    `json:"seq"`
			Sub  int    `json:"sub"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("result not valid JSON: %v (%s)", err, out)
		}
		if got.Type != "text" || got.Text != "hi" || got.UUID != "abc" {
			t.Errorf("existing fields not preserved: %+v", got)
		}
		if got.Seq != 7 || got.Sub != 2 {
			t.Errorf("seq/sub = %d/%d, want 7/2", got.Seq, got.Sub)
		}
	})

	t.Run("non-object entry returned unchanged", func(t *testing.T) {
		for _, in := range []string{`"just a string"`, `not json at all`, `[1,2,3]`} {
			if got := string(stampSeq([]byte(in), 1, 0)); got != in {
				t.Errorf("stampSeq(%q) = %q, want unchanged", in, got)
			}
		}
	})
}

type fakeSeqXform struct{ outs [][]byte }

func (f fakeSeqXform) TransformLine(string) [][]byte { return f.outs }

func TestTransformLineWithSeq(t *testing.T) {
	// One source line fans to three entries, the middle one empty (skipped).
	// sub must be the SLICE index (0 and 2), not a compacted 0/1 — so it stays
	// stable/deterministic across live and replay regardless of empties.
	f := fakeSeqXform{outs: [][]byte{
		[]byte(`{"type":"a"}`),
		{},
		[]byte(`{"type":"b"}`),
	}}
	outs := transformLineWithSeq(f, "src", 5)
	if len(outs) != 3 || len(outs[1]) != 0 {
		t.Fatalf("unexpected outs shape: %v", outs)
	}
	for _, tc := range []struct{ i, wantSub int }{{0, 0}, {2, 2}} {
		var got struct {
			Seq int `json:"seq"`
			Sub int `json:"sub"`
		}
		if err := json.Unmarshal(outs[tc.i], &got); err != nil {
			t.Fatalf("outs[%d] not JSON: %v", tc.i, err)
		}
		if got.Seq != 5 || got.Sub != tc.wantSub {
			t.Errorf("outs[%d]: seq/sub = %d/%d, want 5/%d", tc.i, got.Seq, got.Sub, tc.wantSub)
		}
	}
}

func writeLines(t *testing.T, lines []string, trailingNewline bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	content := ""
	for i, l := range lines {
		content += l
		if i < len(lines)-1 || trailingNewline {
			content += "\n"
		}
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestReadLastNLinesIndexed(t *testing.T) {
	lines := []string{"l0", "l1", "l2", "l3", "l4"}
	for _, trailing := range []bool{true, false} {
		p := writeLines(t, lines, trailing)

		t.Run("window smaller than file", func(t *testing.T) {
			got, base, err := readLastNLinesIndexed(p, 2)
			if err != nil {
				t.Fatal(err)
			}
			if base != 3 { // last 2 of 5 → out[0] is line index 3
				t.Errorf("baseIndex = %d, want 3", base)
			}
			if len(got) != 2 || got[0] != "l3" || got[1] != "l4" {
				t.Errorf("lines = %v, want [l3 l4]", got)
			}
		})

		t.Run("window >= file returns all from base 0", func(t *testing.T) {
			got, base, err := readLastNLinesIndexed(p, 10)
			if err != nil {
				t.Fatal(err)
			}
			if base != 0 {
				t.Errorf("baseIndex = %d, want 0", base)
			}
			if len(got) != 5 {
				t.Errorf("want all 5 lines, got %v", got)
			}
		})
	}

	t.Run("empty file", func(t *testing.T) {
		p := writeLines(t, nil, false)
		got, base, err := readLastNLinesIndexed(p, 3)
		if err != nil || len(got) != 0 || base != 0 {
			t.Errorf("empty file: got=%v base=%d err=%v", got, base, err)
		}
	})
}
