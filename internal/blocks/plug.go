package blocks

// plug.go —— 连接器「插拔包络宽」查找表的 loader（#168② connector-plug-clearance）。
//
// 为什么这张表要放在 blocks/data 而不是 skill 树：go:embed 够不到 `..`，
// skills/easyeda-agent/references/standard-parts.json 编不进二进制（PlacementIndex
// 的注释里已经记过这条硬约束）。blocks/data 是唯一能 embed 的数据目录，`_` 前缀
// 又刚好被块加载器跳过 —— 与既有 _schema.json 同例。
//
// 与 ConnectorOpening(opening.go) 的关系：同一套匹配范式（device-name 小写子串），
// 但那边声明的是**开口朝哪**（一维方向），这边声明的是**插拔要占多宽**（一维尺寸）。
// 两者都属于"器件类别的通用物理知识"，所以都落在块库侧而不是硬编码进算法。

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// plugEnvelopeRaw 必须用**显式文件名** embed：目录 embed(`//go:embed data`)会排除
// `_` 开头的文件，所以 blocks.go 那个 var 里根本没有这张表。
//
//go:embed data/_plug_envelope.json
var plugEnvelopeRaw []byte

// PlugEnvelope 是一个连接器类别的插拔包络。
//
// 语义（与 _plug_envelope.json 的 _doc 一致）：宽度是「插拔这个口时侧向占用的宽度」，
// 取插头护套宽与母座本体宽的**较大者**。这样定义保证包络永远不比 footprint 窄 ——
// 否则查到表反而比 bbox 兜底更宽松，规则会被数据削弱。
//
// 两种声明方式二选一：
//   - PlugWidthMM > 0        固定宽（Type-C / DC 座 / IPEX…）
//   - PitchMM > 0            排式连接器，宽度随脚数变：pitch×(pins−1) + margin
type PlugEnvelope struct {
	Match             string   `json:"match"`   // device-name 小写子串（主 key）
	Aliases           []string `json:"aliases"` // 同一类别的其它写法
	Name              string   `json:"name"`
	ReceptacleWidthMM float64  `json:"receptacle_width_mm"` // 母座本体宽，仅供人对照
	PlugWidthMM       float64  `json:"plug_width_mm"`
	PitchMM           float64  `json:"pitch_mm"`
	PlugMarginMM      float64  `json:"plug_margin_mm"`
	Confidence        string   `json:"confidence"` // datasheet | measured | estimated
	Reason            string   `json:"reason"`
}

// WidthMM 返回该类别在给定引脚数下的插拔包络宽（mm）。
//
// pins 只对排式连接器有意义。读不到脚数时按 2P 估：宁可低估（少报一条 WARN）也不要
// 凭空放大间距要求 —— 一个靠猜出来的宽度报出的"插头打架"会让人不再信这条规则。
func (p PlugEnvelope) WidthMM(pins int) float64 {
	if p.PitchMM > 0 {
		n := pins
		if n < 2 {
			n = 2
		}
		return p.PitchMM*float64(n-1) + p.PlugMarginMM
	}
	return p.PlugWidthMM
}

// Keys 返回这条记录的全部匹配 key（小写去空，主 key 在前）。
func (p PlugEnvelope) Keys() []string {
	out := make([]string, 0, 1+len(p.Aliases))
	if k := strings.ToLower(strings.TrimSpace(p.Match)); k != "" {
		out = append(out, k)
	}
	for _, a := range p.Aliases {
		if k := strings.ToLower(strings.TrimSpace(a)); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// valid 报告这条记录能不能算出宽度。没有任何宽度来源的条目是数据错误，直接丢弃 ——
// 留着它会让 MatchPlugEnvelope 返回一个 0 宽包络，下游拿 0 去比中心距等于永不报警，
// 比查不到还糟（查不到还会走 bbox 兜底）。
func (p PlugEnvelope) valid() bool {
	if strings.TrimSpace(p.Match) == "" {
		return false
	}
	if p.PitchMM > 0 {
		return p.PlugMarginMM > 0
	}
	return p.PlugWidthMM > 0
}

// plugEnvelopeDoc 是 _plug_envelope.json 的顶层形状。_doc 是给人读的说明，解析时忽略。
type plugEnvelopeDoc struct {
	SchemaVersion int            `json:"schema_version"`
	Envelopes     []PlugEnvelope `json:"envelopes"`
}

// LoadPlugEnvelopes 解析整张表（丢弃无宽度来源的坏条目），按主 key 排序保证输出确定。
func LoadPlugEnvelopes() ([]PlugEnvelope, error) {
	var doc plugEnvelopeDoc
	if err := json.Unmarshal(plugEnvelopeRaw, &doc); err != nil {
		return nil, err
	}
	out := make([]PlugEnvelope, 0, len(doc.Envelopes))
	for _, e := range doc.Envelopes {
		if e.valid() {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Match < out[j].Match })
	return out, nil
}

var (
	plugOnce  sync.Once
	plugTable []PlugEnvelope
)

// plugEnvelopes 是进程内缓存（表是只读的，加载一次即可）。
func plugEnvelopes() []PlugEnvelope {
	plugOnce.Do(func() {
		if t, err := LoadPlugEnvelopes(); err == nil {
			plugTable = t
		}
	})
	return plugTable
}

// MatchPlugEnvelope 按 device 名做小写子串匹配。
//
// **最长 key 赢**：泛 key("usb")不能抢走专 key("usb-c")。相同长度时按主 key 字典序
// 定胜负，保证同一块板每次跑出同一个答案（打分要能进 golden 回归，不确定的匹配会让
// fixture 随机抖动）。
func MatchPlugEnvelope(device string) (PlugEnvelope, bool) {
	d := strings.ToLower(strings.TrimSpace(device))
	if d == "" {
		return PlugEnvelope{}, false
	}
	var best PlugEnvelope
	bestLen := 0
	found := false
	for _, e := range plugEnvelopes() {
		for _, k := range e.Keys() {
			if !strings.Contains(d, k) {
				continue
			}
			if len(k) > bestLen || (len(k) == bestLen && found && e.Match < best.Match) {
				best, bestLen, found = e, len(k), true
			}
		}
	}
	return best, found
}
