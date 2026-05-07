package llm

import (
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// TestRecallResultsToMemories_FieldMapping is the unit test for the
// internal projection. It pins the 1:1 Subject/Content mapping and the
// distance→relevance score conversion (1 - distance/2 clamped to [0,1]).
func TestRecallResultsToMemories_FieldMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       []notebook.RecallResult
		wantLen  int
		wantNil  bool
		assertFn func(t *testing.T, got []RecalledMemory)
	}{
		{
			name:    "nil_input_returns_nil",
			in:      nil,
			wantNil: true,
		},
		{
			name:    "empty_input_returns_empty_non_nil",
			in:      []notebook.RecallResult{},
			wantLen: 0,
		},
		{
			name: "preserves_subject_and_content_and_maps_distance",
			in: []notebook.RecallResult{
				{Subject: "s1", Content: "c1", Distance: 0.0},       // perfect match → 1.0
				{Subject: "s2", Content: "c2", Distance: 2.0},       // worst → 0.0
				{Subject: "s3", Content: "c3", Distance: 1.0},       // mid → 0.5
				{Subject: "", Content: "no-subject", Distance: 0.5}, // empty subject preserved
			},
			wantLen: 4,
			assertFn: func(t *testing.T, got []RecalledMemory) {
				if got[0].Subject != "s1" || got[0].Content != "c1" || got[0].Score != 1.0 {
					t.Errorf("got[0] = %+v, want {s1, c1, 1.0}", got[0])
				}
				if got[1].Score != 0.0 {
					t.Errorf("got[1].Score = %v, want 0.0 (distance=2.0)", got[1].Score)
				}
				if got[2].Score != 0.5 {
					t.Errorf("got[2].Score = %v, want 0.5 (distance=1.0)", got[2].Score)
				}
				if got[3].Subject != "" || got[3].Content != "no-subject" {
					t.Errorf("got[3] = %+v, want {empty, no-subject, ...}", got[3])
				}
			},
		},
		{
			name: "out_of_range_distance_clamps_score",
			in: []notebook.RecallResult{
				{Subject: "neg", Content: "c", Distance: -0.5}, // would be 1.25 → clamp 1.0
				{Subject: "big", Content: "c", Distance: 3.0},  // would be -0.5 → clamp 0.0
			},
			wantLen: 2,
			assertFn: func(t *testing.T, got []RecalledMemory) {
				if got[0].Score != 1.0 {
					t.Errorf("negative-distance Score = %v, want 1.0 (clamped)", got[0].Score)
				}
				if got[1].Score != 0.0 {
					t.Errorf("over-2-distance Score = %v, want 0.0 (clamped)", got[1].Score)
				}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := recallResultsToMemories(tc.in)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got = %+v, want nil", got)
				}
				return
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len(got) = %d, want %d", len(got), tc.wantLen)
			}
			if tc.assertFn != nil {
				tc.assertFn(t, got)
			}
		})
	}
}

// TestBuildTurnRequest_CallerOptsApplyLast pins AC5: caller-supplied opts
// are applied AFTER the recalled-memory option, so a caller can override
// System if needed (caller-last-wins). Lives in the in-package test file
// because it constructs a [RequestOption] that touches the private
// `requestParams.system` field.
func TestBuildTurnRequest_CallerOptsApplyLast(t *testing.T) {
	pinTurnDataDir(t)
	sup, db := openTurnSupervisor(t)
	insertOneTurnEntry(t, db)

	const overrideSystem = "OVERRIDE"
	overrideOpt := RequestOption(func(p *requestParams) { p.system = overrideSystem })

	manifest := turnValidManifest()
	embedder := NewFakeEmbeddingProvider()

	req, err := BuildTurnRequest(testCtx(), manifest, "q", embedder, sup, overrideOpt)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if req.System != overrideSystem {
		t.Errorf("System = %q, want %q (caller opt should win)", req.System, overrideSystem)
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusApplied {
		t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusApplied)
	}
}
