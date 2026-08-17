package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The connector shape: one BRIDGE tree (共线合并短路) — a wire tree spanning two
// wires whose anchored flags carry two DIFFERENT net names, plus one ORPHAN tree
// (a stub touching a pin with no net flag).
func TestParseAndRenderBridge_BridgeAndOrphan(t *testing.T) {
	result := map[string]any{
		"passed": false,
		"summary": map[string]any{
			"trees":          float64(2),
			"bridges":        float64(1),
			"orphans":        float64(1),
			"wireTreesTotal": float64(9),
		},
		"trees": []any{
			map[string]any{
				"kind":    "BRIDGE",
				"wireIds": []any{"w1", "w2"},
				"flagIds": []any{"f1", "f2"},
				"pins":    []any{"U1:5", "R3:1"},
				"nets":    []any{"GND", "VCC"},
			},
			map[string]any{
				"kind":    "ORPHAN",
				"wireIds": []any{"w7"},
				"flagIds": []any{},
				"pins":    []any{"C2:2"},
				"nets":    []any{},
			},
		},
	}
	rep, err := parseBridgeReport(result)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Passed {
		t.Error("expected passed=false")
	}
	if rep.Summary.Bridges != 1 || rep.Summary.Orphans != 1 || len(rep.Trees) != 2 {
		t.Errorf("unexpected summary/trees: %+v", rep)
	}
	// parseBridgeReport stamps the kebab-case rule type + level from Kind so
	// --json consumers can gate by type like sch check / pcb check findings.
	if rep.Trees[0].Type != "wire-bridge" || rep.Trees[0].Level != "error" {
		t.Errorf("BRIDGE tree not typed wire-bridge/error: %+v", rep.Trees[0])
	}
	if rep.Trees[1].Type != "orphan-stub" || rep.Trees[1].Level != "warn" {
		t.Errorf("ORPHAN tree not typed orphan-stub/warn: %+v", rep.Trees[1])
	}

	var buf bytes.Buffer
	renderBridgeReport(rep, &buf)
	out := buf.String()
	for _, want := range []string{"ERROR", "wire-bridge", "BRIDGE", "nets=[GND,VCC]", "pins=[U1:5,R3:1]", "w1, w2", "WARN", "orphan-stub", "ORPHAN", "C2:2"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// Clean board: no problem trees → passed, and the "no bridges or orphans" line.
func TestRenderBridge_Clean(t *testing.T) {
	rep := bridgeReport{Passed: true, Summary: bridgeSummary{WireTreesTotal: 12}}
	var buf bytes.Buffer
	renderBridgeReport(rep, &buf)
	if !strings.Contains(buf.String(), "no bridges or orphans") {
		t.Errorf("expected clean line, got:\n%s", buf.String())
	}
}

// --json output must be wrapped in the {id,type,version,ok,result} envelope, so a
// uniform-envelope parser reading result.trees works consistently with sch check.
func TestEncodeResultEnvelope_BridgeReport(t *testing.T) {
	rep := bridgeReport{
		Passed:  false,
		Summary: bridgeSummary{Trees: 1, Bridges: 1},
		Trees: []bridgeTree{
			{Kind: "BRIDGE", Type: "wire-bridge", Level: "error", WireIds: []string{"w1", "w2"}, Nets: []string{"GND", "VCC"}, Pins: []string{"U1:5"}},
		},
	}
	res := &actionResult{ID: "req-9", Type: "response", Version: "1", OK: true}

	var buf bytes.Buffer
	if err := encodeResultEnvelope(res, rep, &buf); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var env struct {
		ID     string `json:"id"`
		OK     bool   `json:"ok"`
		Result struct {
			Passed bool         `json:"passed"`
			Trees  []bridgeTree `json:"trees"`
		} `json:"result"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, buf.String())
	}
	if env.ID != "req-9" || !env.OK {
		t.Errorf("envelope metadata lost: %+v", env)
	}
	if len(env.Result.Trees) != 1 || env.Result.Trees[0].Kind != "BRIDGE" || len(env.Result.Trees[0].Nets) != 2 {
		t.Errorf("result.trees not reachable via envelope: %+v", env.Result)
	}
	// The kebab-case rule type must survive the envelope round-trip so a JSON
	// gate can count by type.
	if env.Result.Trees[0].Type != "wire-bridge" || env.Result.Trees[0].Level != "error" {
		t.Errorf("rule type/level lost in envelope: %+v", env.Result.Trees[0])
	}
}

// ORPHAN_TREE: a wire tree touching NO pin at all — the component-move-residue
// form (flag+stub survives the move) that ORPHAN (needs a touched pin) and
// ORPHAN_FLAG (needs the flag to sit on NO wire) were both structurally blind
// to (live 2026-08-18: two GND flag+stub trees on P2_MCU reported clean).
func TestParseAndRenderBridge_OrphanTree(t *testing.T) {
	result := map[string]any{
		"passed": false,
		"summary": map[string]any{
			"trees":          float64(2),
			"bridges":        float64(0),
			"orphans":        float64(0),
			"orphanFlags":    float64(0),
			"orphanTrees":    float64(2),
			"wireTreesTotal": float64(14),
		},
		"trees": []any{
			map[string]any{ // move residue: GND flag + 20u stub, no pin under it
				"kind":    "ORPHAN_TREE",
				"wireIds": []any{"w9"},
				"flagIds": []any{"f9"},
				"pins":    []any{},
				"nets":    []any{"GND"},
			},
			map[string]any{ // bare dead wire: neither flags nor pins
				"kind":    "ORPHAN_TREE",
				"wireIds": []any{"w10"},
				"flagIds": []any{},
				"pins":    []any{},
				"nets":    []any{},
			},
		},
	}
	rep, err := parseBridgeReport(result)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Passed {
		t.Error("expected passed=false")
	}
	if rep.Summary.OrphanTrees != 2 {
		t.Errorf("orphanTrees not parsed: %+v", rep.Summary)
	}
	for i := range rep.Trees {
		if rep.Trees[i].Type != "orphan-tree" || rep.Trees[i].Level != "warn" {
			t.Errorf("tree %d not typed orphan-tree/warn: %+v", i, rep.Trees[i])
		}
	}

	var buf bytes.Buffer
	renderBridgeReport(rep, &buf)
	out := buf.String()
	for _, want := range []string{"orphan-tree", "2 orphan-tree(s) (tree touching no pin)", "nets=[GND]", "w9", "f9", "悬空树"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n--- output ---\n%s", want, out)
		}
	}
}
