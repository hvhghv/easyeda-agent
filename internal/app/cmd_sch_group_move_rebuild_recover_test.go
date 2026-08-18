package app

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ── group-move 平移中途失败的恢复段 ─────────────────────────────────────────
//
// 真机事故:第 3 步先清扫全组桩线/旗,第 4 步逐件 modify,任一件失败即裸退 ——
// 已采集的连接表 conns 弃之不用,J1+R4+R5 组桩线全清、器件没挪、十几个脚全悬空。
// 修复后的契约:modify 中止 → 用手里的 conns 对**全部**成员立即重连(挪成的在
// 新位置、没挪成的在原位都能连回)→ 带着「几成几败」返回错误。

func recoverTestFixtures() ([]groupRebuildMember, []acConnSpec) {
	movable := []groupRebuildMember{
		{ID: "a", Designator: "J1", X: 100, Y: 100},
		{ID: "b", Designator: "R4", X: 200, Y: 100},
		{ID: "c", Designator: "R5", X: 300, Y: 100},
	}
	conns := []acConnSpec{
		{PinRef: "J1:1", Kind: "gnd", Net: "GND"},
		{PinRef: "R4:1", Kind: "power", Net: "5V"},
		{PinRef: "R5:2", Kind: "netport", Net: "SIG"},
	}
	return movable, conns
}

// modify 中途失败:恢复段必须被调用,且 conns 一个不少地全部尝试重连;
// 错误里注明「已自动重连,几成几败」并点名仍断的 pin。
func TestGroupMoveTranslateWithRecovery_MidFailureRunsRecovery(t *testing.T) {
	movable, conns := recoverTestFixtures()
	var modified []string
	reconnectCalls := 0
	var reconnectGot []acConnSpec

	err := groupMoveTranslateWithRecovery(movable, 40, -20, conns,
		func(m groupRebuildMember, nx, ny float64) error {
			modified = append(modified, m.Designator)
			if nx != m.X+40 || ny != m.Y-20 {
				t.Errorf("%s 的目标坐标错了: (%v,%v)", m.Designator, nx, ny)
			}
			if m.Designator == "R4" { // 第 2/3 件失败
				return errors.New("connector wedged")
			}
			return nil
		},
		func(cs []acConnSpec) ([]string, []string, error) {
			reconnectCalls++
			reconnectGot = cs
			// 恢复重连 2 成 1 败
			return []string{"J1:1", "R4:1"}, []string{"R5:2"},
				fmt.Errorf("autoconnect: 1 connection(s) failed")
		}, &bytes.Buffer{})

	if err == nil {
		t.Fatal("modify 失败必须仍返回错误(恢复不是吞错)")
	}
	if reconnectCalls != 1 {
		t.Fatalf("恢复段必须被调用恰好一次, got %d", reconnectCalls)
	}
	if len(reconnectGot) != len(conns) {
		t.Fatalf("恢复段必须对全部 conns 尝试重连: got %d want %d", len(reconnectGot), len(conns))
	}
	for i := range conns {
		if reconnectGot[i].PinRef != conns[i].PinRef {
			t.Errorf("conns[%d] 没有原样传给恢复段: %+v", i, reconnectGot[i])
		}
	}
	msg := err.Error()
	if !strings.Contains(msg, "已自动重连") || !strings.Contains(msg, "2 成 1 败") {
		t.Errorf("错误里必须注明「已自动重连,几成几败」: %q", msg)
	}
	if !strings.Contains(msg, "R5:2") {
		t.Errorf("错误里必须点名仍断的 pin: %q", msg)
	}
	// 失败件之后的成员不再继续 modify(状态已不可信)。
	if got := strings.Join(modified, ","); got != "J1,R4" {
		t.Errorf("失败后不该继续平移后续成员: %q", got)
	}
}

// 恢复重连本身也失败(比如读场景就挂了,一个都没连回):如实列出全部仍断的 pin。
func TestGroupMoveTranslateWithRecovery_RecoveryItselfFailsListsAllPins(t *testing.T) {
	movable, conns := recoverTestFixtures()
	err := groupMoveTranslateWithRecovery(movable, 10, 0, conns,
		func(m groupRebuildMember, nx, ny float64) error { return errors.New("boom") },
		func(cs []acConnSpec) ([]string, []string, error) {
			return nil, nil, errors.New("components.list: connector did not respond")
		}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("必须返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "恢复重连本身失败") {
		t.Errorf("必须如实说明恢复也失败了: %q", msg)
	}
	for _, ref := range []string{"J1:1", "R4:1", "R5:2"} {
		if !strings.Contains(msg, ref) {
			t.Errorf("恢复失败时必须列出仍断的 pin %s: %q", ref, msg)
		}
	}
}

// 全部平移成功:恢复段一次都不该跑(它只属于失败路径)。
func TestGroupMoveTranslateWithRecovery_SuccessSkipsRecovery(t *testing.T) {
	movable, conns := recoverTestFixtures()
	reconnectCalls := 0
	err := groupMoveTranslateWithRecovery(movable, 40, -20, conns,
		func(m groupRebuildMember, nx, ny float64) error { return nil },
		func(cs []acConnSpec) ([]string, []string, error) {
			reconnectCalls++
			return nil, nil, nil
		}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("全部成功不该报错: %v", err)
	}
	if reconnectCalls != 0 {
		t.Errorf("成功路径不该触发恢复重连, got %d 次", reconnectCalls)
	}
}

// 组内本来就没有已连引脚(conns 空):失败路径不假装重连,摘要如实说明。
func TestGroupMoveRecoverConnections_NoConnsIsExplicit(t *testing.T) {
	got := groupMoveRecoverConnections(nil,
		func(cs []acConnSpec) ([]string, []string, error) {
			t.Fatal("conns 为空时不该调用重连")
			return nil, nil, nil
		}, &bytes.Buffer{})
	if !strings.Contains(got, "无需恢复") {
		t.Errorf("空 conns 的摘要应说明无需恢复: %q", got)
	}
}

// 全部连回:摘要报「n/n 全部恢复」。
func TestGroupMoveRecoverConnections_FullRecovery(t *testing.T) {
	_, conns := recoverTestFixtures()
	got := groupMoveRecoverConnections(conns,
		func(cs []acConnSpec) ([]string, []string, error) {
			return []string{"J1:1", "R4:1", "R5:2"}, nil, nil
		}, &bytes.Buffer{})
	if !strings.Contains(got, "3/3") || !strings.Contains(got, "全部恢复") {
		t.Errorf("全部连回应报 n/n 全部恢复: %q", got)
	}
}
