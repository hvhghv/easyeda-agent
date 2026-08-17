package app

import (
	"reflect"
	"testing"
)

// 图框守卫的内部删单侧:deep-sweep/清创的程序化删除清单里混进 sheet id 必须被
// 滤掉 —— CLI prim-delete 有交互守卫,内部删单不能裸奔(2026-08-17 图框误删案)。
func TestDropSheetIDs(t *testing.T) {
	comps := []layoutComp{
		{ID: "s1", ComponentType: "sheet"},
		{ID: "p1", ComponentType: "part"},
		{ID: "f1", ComponentType: "netflag"},
	}
	got := dropSheetIDs([]string{"p1", "s1", "f1"}, comps)
	if want := []string{"p1", "f1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sheet id 该被滤掉:got %v want %v", got, want)
	}
	// 无 sheet 时原样返回(零分配路径)。
	if got := dropSheetIDs([]string{"a", "b"}, comps[1:]); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("无 sheet 场景不该改动清单:%v", got)
	}
}
