package blocks

import (
	"math"
	"strings"
	"testing"
)

// 表本身要能加载，并且每条都自带可追溯的出处 —— 一条没有 reason 的阈值等于一个
// 无法校准的魔数（#167/#168 的硬要求）。
func TestLoadPlugEnvelopes(t *testing.T) {
	all, err := LoadPlugEnvelopes()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(all) < 10 {
		t.Fatalf("only %d envelopes — the table must at least cover Type-C/USB-A/Micro-USB/DC/XH/PH/KF/IPEX/排针", len(all))
	}
	confOK := map[string]bool{"datasheet": true, "measured": true, "estimated": true}
	seen := map[string]string{}
	for _, e := range all {
		if strings.TrimSpace(e.Reason) == "" {
			t.Errorf("%s: reason is empty — every width must carry its provenance", e.Match)
		}
		if !confOK[e.Confidence] {
			t.Errorf("%s: confidence %q not in datasheet|measured|estimated", e.Match, e.Confidence)
		}
		if e.WidthMM(2) <= 0 {
			t.Errorf("%s: width at 2 pins = %v, want > 0", e.Match, e.WidthMM(2))
		}
		// 包络绝不能比母座本体还窄，否则查到表反而比 bbox 兜底更宽松。
		if e.ReceptacleWidthMM > 0 && e.PlugWidthMM > 0 && e.PlugWidthMM < e.ReceptacleWidthMM {
			t.Errorf("%s: plug %.2fmm < receptacle %.2fmm — envelope must be max(plug, body)", e.Match, e.PlugWidthMM, e.ReceptacleWidthMM)
		}
		for _, k := range e.Keys() {
			if prev, dup := seen[k]; dup {
				t.Errorf("key %q claimed by both %q and %q — matching would be ambiguous", k, prev, e.Match)
			}
			seen[k] = e.Match
		}
	}
}

// 排式连接器的宽度必须随脚数线性增长：PH2.0-3P 比 2P 宽整整一个 pitch。
func TestPlugEnvelopeWidthByPins(t *testing.T) {
	ph, ok := MatchPlugEnvelope("PH2.0-3P")
	if !ok {
		t.Fatal("PH2.0-3P should match the PH2.0 entry")
	}
	if got, want := ph.WidthMM(3), 2.0*2+4.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("PH2.0 3P width = %v, want %v", got, want)
	}
	if ph.WidthMM(3)-ph.WidthMM(2) != ph.PitchMM {
		t.Errorf("3P − 2P should be exactly one pitch; got %v", ph.WidthMM(3)-ph.WidthMM(2))
	}
	// 脚数读不到（0）时按 2P 估，不许返回负数或 0。
	if ph.WidthMM(0) != ph.WidthMM(2) {
		t.Errorf("unknown pin count should fall back to the 2P width; got %v", ph.WidthMM(0))
	}
}

// 最长 key 赢：一个名字里同时含 "usb" 和 "usb-c" 的器件必须解析成 Type-C，
// 而不是被泛 key 抢走（否则 Type-C 会拿到 USB-A 的 18mm 包络）。
func TestMatchPlugEnvelopeLongestKeyWins(t *testing.T) {
	cases := []struct {
		device string
		want   string
	}{
		{"TYPE-C-31-M-12", "type-c"},
		{"USB-C 16P 母座", "type-c"},
		{"USB-A-90度插板", "usb-a"},
		{"MICRO-USB-B-5P", "micro-usb"},
		{"XH2.54-4P 直插", "xh2.54"},
		{"KF301-5.0-3P", "kf301"},
		{"IPEX-1 天线座", "ipex"},
		{"DC-005 电源座", "dc-005"},
	}
	for _, c := range cases {
		got, ok := MatchPlugEnvelope(c.device)
		if !ok {
			t.Errorf("%s: no match, want %s", c.device, c.want)
			continue
		}
		if got.Match != c.want {
			t.Errorf("%s → %s, want %s", c.device, got.Match, c.want)
		}
	}
	if _, ok := MatchPlugEnvelope("0402 100nF"); ok {
		t.Error("a passive must not match any connector envelope")
	}
	if _, ok := MatchPlugEnvelope(""); ok {
		t.Error("empty device name must not match")
	}
}
