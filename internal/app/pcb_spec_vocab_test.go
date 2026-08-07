package app

import (
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

// TestZoneVocabularyMatchesSpecPackage 钉住跨包词汇一致性。
//
// internal/spec 是契约侧（S0 spec 的合法取值），internal/app 的 pcbZoneNames 是
// 消费侧（zone→矩形解析、zone-violation 规则）。两边各存一份是无奈之举——app 可以
// import spec，反向不行，所以词汇表定义在 spec、这里断言 app 没跑偏。
//
// 分叉的后果很具体：spec validate 说某个 zone 合法、pcb zones set 却报错拒绝，
// 或者反过来 spec 放行了一个 pcbZoneRect 解析不出矩形的名字，导致模块被静默
// 跳过（parseZoneSpec 对空 zone 就是静默跳过的）。
func TestZoneVocabularyMatchesSpecPackage(t *testing.T) {
	for name := range spec.ZoneNames {
		if !pcbZoneNames[name] {
			t.Errorf("spec.ZoneNames has %q but internal/app pcbZoneNames does not — `spec validate` would accept a zone that `pcb zones set` rejects", name)
		}
	}
	for name := range pcbZoneNames {
		if !spec.ZoneNames[name] {
			t.Errorf("pcbZoneNames has %q but spec.ZoneNames does not — `pcb zones set` would accept a zone `spec validate` calls invalid", name)
		}
	}
}

// TestSpecEdgeVocabularyMatchesApEdge 钉住板边词汇一致性：spec 的 edge 取值
// （去掉 "any"）必须都是 apEdge.String() 能产出的名字，否则 spec 里写的
// `edge: "bottom"` 与打分侧算出的 nearestEdge 对不上，internal-on-edge /
// 对外口聚边这两维会永远判「边不符」。
func TestSpecEdgeVocabularyMatchesApEdge(t *testing.T) {
	produced := map[string]bool{}
	for _, e := range []apEdge{edgeLeft, edgeRight, edgeTop, edgeBottom} {
		produced[e.String()] = true
	}
	for name := range spec.EdgeNames {
		if name == "any" { // "any" 是意图侧的通配，不是几何侧产出的边名
			continue
		}
		if !produced[name] {
			t.Errorf("spec.EdgeNames has %q which apEdge.String() never produces", name)
		}
	}
	for name := range produced {
		if !spec.EdgeNames[name] {
			t.Errorf("apEdge produces %q which spec.EdgeNames rejects", name)
		}
	}
}
