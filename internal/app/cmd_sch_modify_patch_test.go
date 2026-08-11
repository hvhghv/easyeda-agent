package app

import (
	"reflect"
	"testing"
)

func TestBuildModifyPatch(t *testing.T) {
	cases := []struct {
		name           string
		patchJSON      string
		overrides      map[string]any
		want           map[string]any
		wantOverridden []string
		wantErr        bool
	}{
		{
			name:      "flags only",
			overrides: map[string]any{"x": 150.0, "y": 200.0},
			want:      map[string]any{"x": 150.0, "y": 200.0},
		},
		{
			name:      "patch only",
			patchJSON: `{"x":150,"y":200}`,
			want:      map[string]any{"x": 150.0, "y": 200.0},
		},
		{
			name:      "merge disjoint",
			patchJSON: `{"designator":"R12"}`,
			overrides: map[string]any{"x": 10.0},
			want:      map[string]any{"designator": "R12", "x": 10.0},
		},
		{
			name:           "flag overrides same-name patch key and reports it",
			patchJSON:      `{"x":999,"y":200}`,
			overrides:      map[string]any{"x": 150.0},
			want:           map[string]any{"x": 150.0, "y": 200.0},
			wantOverridden: []string{"x"},
		},
		{
			name:           "multiple overrides reported sorted",
			patchJSON:      `{"y":1,"x":2,"rotation":3}`,
			overrides:      map[string]any{"y": 10.0, "x": 20.0},
			want:           map[string]any{"x": 20.0, "y": 10.0, "rotation": 3.0},
			wantOverridden: []string{"x", "y"},
		},
		{
			name:      "explicit zero flag IS written (Changed semantics live in the caller)",
			patchJSON: `{"rotation":90}`,
			overrides: map[string]any{"rotation": 0.0},
			want:      map[string]any{"rotation": 0.0},
			// 0 must land in the patch — the caller only puts explicitly-passed
			// flags into overrides, so a present 0 is intentional.
			wantOverridden: []string{"rotation"},
		},
		{
			name:    "no sources errors",
			wantErr: true,
		},
		{
			name:      "empty patch object with no flags errors",
			patchJSON: `{}`,
			wantErr:   true,
		},
		{
			name:      "invalid patch json errors",
			patchJSON: `{"x":`,
			overrides: map[string]any{"x": 1.0},
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, overridden, err := buildModifyPatch(tc.patchJSON, tc.overrides)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("buildModifyPatch(%q, %v) = %v, want error", tc.patchJSON, tc.overrides, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("patch = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(overridden, tc.wantOverridden) {
				t.Fatalf("overridden = %v, want %v", overridden, tc.wantOverridden)
			}
		})
	}
}

func TestValidatePinTarget(t *testing.T) {
	cases := []struct {
		name               string
		pinSet, xSet, ySet bool
		wantErr            bool
	}{
		{"pin only", true, false, false, false},
		{"coords only", false, true, true, false},
		{"pin plus x conflicts", true, true, false, true},
		{"pin plus y conflicts", true, false, true, true},
		{"pin plus both coords conflicts", true, true, true, true},
		{"x without y", false, true, false, true},
		{"y without x", false, false, true, true},
		{"nothing", false, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePinTarget(tc.pinSet, tc.xSet, tc.ySet)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validatePinTarget(%v,%v,%v) err=%v, wantErr=%v", tc.pinSet, tc.xSet, tc.ySet, err, tc.wantErr)
			}
		})
	}
}
