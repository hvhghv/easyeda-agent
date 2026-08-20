package app

import (
	"errors"
	"reflect"
	"testing"
)

// fakeAliveBoard 模拟一块「删了才消失」的画布,可注入首删 no-op 的平台病。
type fakeAliveBoard struct {
	alive     map[string]bool
	noopOnce  map[string]bool // 这些 id 首次 delete 静默 no-op(平台病)
	deletes   []string
	aliveErrs []error // 依次弹出的 aliveSet 错误(nil = 成功)
}

func (b *fakeAliveBoard) deleteOne(id string) error {
	b.deletes = append(b.deletes, id)
	if b.noopOnce[id] {
		delete(b.noopOnce, id) // 第二次(重试)才真删
		return nil
	}
	delete(b.alive, id)
	return nil
}

func (b *fakeAliveBoard) aliveSet() (map[string]bool, error) {
	if len(b.aliveErrs) > 0 {
		err := b.aliveErrs[0]
		b.aliveErrs = b.aliveErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	out := map[string]bool{}
	for id := range b.alive {
		out[id] = true
	}
	return out, nil
}

func TestDeleteVerifiedOneByOneCleanSweep(t *testing.T) {
	b := &fakeAliveBoard{alive: map[string]bool{"a": true, "b": true}}
	res, err := deleteVerifiedOneByOne([]string{"a", "b", "a", ""}, b.deleteOne, b.aliveSet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(res.Deleted, []string{"a", "b"}) || len(res.Survived) != 0 || len(res.Retried) != 0 {
		t.Fatalf("clean sweep misreported: %+v", res)
	}
	// 去重后逐个删:恰好 a、b 各删一次。
	if !reflect.DeepEqual(b.deletes, []string{"a", "b"}) {
		t.Fatalf("expected one-by-one deduped deletes, got %v", b.deletes)
	}
}

func TestDeleteVerifiedOneByOneRetriesSilentNoop(t *testing.T) {
	b := &fakeAliveBoard{
		alive:    map[string]bool{"a": true, "b": true},
		noopOnce: map[string]bool{"b": true},
	}
	res, err := deleteVerifiedOneByOne([]string{"a", "b"}, b.deleteOne, b.aliveSet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(res.Retried, []string{"b"}) {
		t.Fatalf("expected b retried, got %+v", res)
	}
	if !reflect.DeepEqual(res.Deleted, []string{"a", "b"}) || len(res.Survived) != 0 {
		t.Fatalf("retry did not recover the silent no-op: %+v", res)
	}
}

func TestDeleteVerifiedOneByOneReportsTrueSurvivor(t *testing.T) {
	b := &fakeAliveBoard{
		alive:    map[string]bool{"a": true, "b": true},
		noopOnce: map[string]bool{},
	}
	// b 永远删不掉:每次删都恢复 noopOnce。
	del := func(id string) error {
		if id == "b" {
			return nil // 静默 no-op,永不生效
		}
		return b.deleteOne(id)
	}
	res, err := deleteVerifiedOneByOne([]string{"a", "b"}, del, b.aliveSet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(res.Survived, []string{"b"}) || !reflect.DeepEqual(res.Deleted, []string{"a"}) {
		t.Fatalf("true survivor misreported: %+v", res)
	}
}

func TestDeleteVerifiedOneByOneVerifyFailureIsAnError(t *testing.T) {
	// settle 预算(settleAttempts=2)内每一次回读都失败,才算真的无法证实。
	b := &fakeAliveBoard{
		alive:     map[string]bool{"a": true},
		aliveErrs: []error{errors.New("connector wedged"), errors.New("connector wedged")},
	}
	if _, err := deleteVerifiedOneByOne([]string{"a"}, b.deleteOne, b.aliveSet); err == nil {
		t.Fatal("verification read failure must surface as an error, not a clean sweep")
	}
}

func TestDeleteVerifiedOneByOneSettlesThroughTransientReadFailure(t *testing.T) {
	// 首读失败、次读成功 = 平台还没缓过来,不是"删不掉"。settle 必须吸收掉它,
	// 否则一次读抖动就把一次干净的删除报成不可证实。
	b := &fakeAliveBoard{
		alive:     map[string]bool{"a": true},
		aliveErrs: []error{errors.New("transient read failure")},
	}
	res, err := deleteVerifiedOneByOne([]string{"a"}, b.deleteOne, b.aliveSet)
	if err != nil {
		t.Fatalf("transient read failure must be absorbed by settle, got %v", err)
	}
	if !reflect.DeepEqual(res.Deleted, []string{"a"}) || len(res.Survived) != 0 {
		t.Fatalf("clean sweep misreported after a transient read failure: %+v", res)
	}
}

// staleThenFreshBoard 模拟「删掉了,但紧跟的回读还是旧快照」:第一次回读仍报
// 图元活着,之后的回读才反映真相。这是连接器 survivingSchPrimitives「删完立刻
// getAll」会踩的那一拍。
type staleThenFreshBoard struct {
	alive      map[string]bool
	reads      int
	deletes    []string
	staleReads int  // 前几次回读返回删除前的快照
	firstStale bool // 记录第一次回读是否报"还活着"(反事实断言用)
}

func (b *staleThenFreshBoard) deleteOne(id string) error {
	b.deletes = append(b.deletes, id)
	delete(b.alive, id)
	return nil
}

func (b *staleThenFreshBoard) aliveSet() (map[string]bool, error) {
	b.reads++
	out := map[string]bool{}
	if b.reads <= b.staleReads {
		out["a"] = true // 尚未落定的快照
		if b.reads == 1 {
			b.firstStale = true
		}
		return out, nil
	}
	for id := range b.alive {
		out[id] = true
	}
	return out, nil
}

func TestDeleteVerifiedOneByOneSettlesStaleReadBack(t *testing.T) {
	b := &staleThenFreshBoard{alive: map[string]bool{"a": true}, staleReads: 1}
	res, err := deleteVerifiedOneByOne([]string{"a"}, b.deleteOne, b.aliveSet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 反事实:立刻读**确实**会报 survived —— 这个 fixture 不是空转。
	if !b.firstStale {
		t.Fatal("fixture did not model a stale first read-back")
	}
	// settle 之后:既不报 survived,也不该白白重发一次删除。
	if len(res.Survived) != 0 {
		t.Fatalf("settled read-back still misreports survivors: %+v", res)
	}
	if len(res.Retried) != 0 {
		t.Fatalf("stale read-back triggered a redundant re-delete: retried=%v deletes=%v", res.Retried, b.deletes)
	}
	if !reflect.DeepEqual(b.deletes, []string{"a"}) {
		t.Fatalf("expected exactly one delete, got %v", b.deletes)
	}
}

func TestSurvivedIDSetShapes(t *testing.T) {
	// primitives.delete 形状:map 类目 → id 列表。
	m := survivedIDSet(map[string]any{"survived": map[string]any{
		"rectangle": []any{"r1"}, "text": []any{"t1", "t2"},
	}})
	if !m["r1"] || !m["t1"] || !m["t2"] || len(m) != 3 {
		t.Fatalf("map shape misparsed: %v", m)
	}
	// component.delete 形状:数组。
	a := survivedIDSet(map[string]any{"survived": []any{"c1"}})
	if !a["c1"] || len(a) != 1 {
		t.Fatalf("array shape misparsed: %v", a)
	}
	if got := survivedIDSet(nil); len(got) != 0 {
		t.Fatalf("nil result should yield empty set, got %v", got)
	}
	if got := survivedIDSet(map[string]any{}); len(got) != 0 {
		t.Fatalf("absent survived should yield empty set, got %v", got)
	}
}
