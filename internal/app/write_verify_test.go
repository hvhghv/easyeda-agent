package app

// 通道 B 的载荷契约测试:字段名必须和 daemon 侧 WriteVerification 的 json tag
// 对得上,否则上报会被静默忽略(健康度又变回只测返回码)。

import (
	"encoding/json"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/daemon"
)

func TestWriteVerifyBodyMatchesDaemonContract(t *testing.T) {
	cfg := &appConfig{host: "127.0.0.1", ports: "60832-60841", project: "ceshi"}
	v := writeVerdict{
		action: "schematic.component.place", source: "sch block-apply",
		requestID: "req_42", returnedOK: true, landed: 1, notLanded: 5,
	}
	buf, err := json.Marshal(writeVerifyBody(cfg, "w1", v))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got daemon.WriteVerification
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("the daemon must be able to decode what we send: %v (%s)", err, buf)
	}
	if got.Action != v.action || got.RequestID != "req_42" || got.Source != "sch block-apply" {
		t.Fatalf("decoded = %+v", got)
	}
	if got.Landed != 1 || got.NotLanded != 5 {
		t.Fatalf("counts lost in transit: %+v", got)
	}
	if got.ReturnedOK == nil || !*got.ReturnedOK {
		t.Fatalf("returnedOK must survive as an explicit true: %+v", got)
	}
	if got.WindowID != "w1" {
		t.Fatalf("windowId = %q", got.WindowID)
	}
	if got.Project != "" {
		t.Fatalf("an explicit window must not also send project (%q)", got.Project)
	}
}

// 没有 windowId 时按 --project 归账(windowId 重连即变,project 才是稳定标识)。
func TestWriteVerifyBodyFallsBackToProject(t *testing.T) {
	cfg := &appConfig{host: "127.0.0.1", ports: "60832-60841", project: "ceshi"}
	buf, _ := json.Marshal(writeVerifyBody(cfg, "", writeVerdict{
		action: "schematic.power.connect_pin", returnedOK: false, landed: 1}))
	var got daemon.WriteVerification
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Project != "ceshi" || got.WindowID != "" {
		t.Fatalf("decoded = %+v", got)
	}
	if got.ReturnedOK == nil || *got.ReturnedOK {
		t.Fatalf("returnedOK=false must be sent explicitly (假失败和假成功的处置动作相反): %+v", got)
	}
}

// 纯遥测:没有可归账的目标、或判决为空时,什么都不做(也绝不 panic)。
func TestReportWriteVerifiedIsInertWithoutATarget(t *testing.T) {
	reportWriteVerified(nil, "w1", writeVerdict{action: "schematic.component.place", landed: 1})
	cfg := &appConfig{host: "127.0.0.1", ports: "60832-60841"}
	reportWriteVerified(cfg, "", writeVerdict{action: "schematic.component.place", landed: 1})
	reportWriteVerified(cfg, "w1", writeVerdict{action: "schematic.component.place"})
	reportWriteVerified(cfg, "w1", writeVerdict{landed: 1})
}
