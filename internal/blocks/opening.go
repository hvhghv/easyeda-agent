package blocks

import (
	_ "embed"
	"encoding/json"
)

// connectorOpeningsRaw 是独立声明表(与 _plug_envelope 同例:`_` 前缀被块加载器
// 跳过,须显式 embed)。块内 openings 依旧有效,两处合并 —— 没有电路块可挂的
// 器件(耳机口/FPC/HDMI)落这张表,声明必须带真板实测证据(见文件 _doc)。
//
//go:embed data/_connector_openings.json
var connectorOpeningsRaw []byte

// ConnectorOpening declares, for a footprint, which way its opening (the wire-entry
// / plug face) points in the footprint's LOCAL (rotation-0) frame. This is a
// physical fact that is NOT in the PCB pad/copper geometry (a symmetric 2-pin screw
// terminal looks the same either way), so it must be block-declared — the placer
// consumes it to rotate the connector so its opening faces OFF-board. Block-level
// `openings: [{match, local}]`, keyed by device-name substring so a PLACED part
// resolves by its manufacturerId (e.g. "KF301-5.0-2P" → match "kf301").
type ConnectorOpening struct {
	Match  string `json:"match"` // device-name substring this applies to (e.g. "kf301")
	Local  string `json:"local"` // opening dir in the footprint's local frame: +x | -x | +y | -y
	Reason string `json:"reason"`
}

// LoadConnectorOpenings aggregates every block's declared connector openings. A
// block without any, or a malformed one, is skipped (best-effort, never fatal).
func LoadConnectorOpenings() ([]ConnectorOpening, error) {
	all, err := Load()
	if err != nil {
		return nil, err
	}
	var out []ConnectorOpening
	var standalone struct {
		Openings []ConnectorOpening `json:"openings"`
	}
	if json.Unmarshal(connectorOpeningsRaw, &standalone) == nil {
		for _, o := range standalone.Openings {
			if o.Match != "" && o.Local != "" {
				out = append(out, o)
			}
		}
	}
	for _, b := range all {
		var raw struct {
			Openings []ConnectorOpening `json:"openings"`
		}
		if json.Unmarshal(b.Raw, &raw) != nil {
			continue
		}
		for _, o := range raw.Openings {
			if o.Match != "" && o.Local != "" {
				out = append(out, o)
			}
		}
	}
	return out, nil
}
