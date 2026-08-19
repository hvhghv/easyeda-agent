package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type blockApplyTestCall struct {
	Action  string
	Payload map[string]any
}

type blockApplyTestDaemon struct {
	mu    sync.Mutex
	calls []blockApplyTestCall
}

func (d *blockApplyTestDaemon) snapshot() []blockApplyTestCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]blockApplyTestCall(nil), d.calls...)
}

func newBlockApplyTestDaemon(t *testing.T, responder func(blockApplyTestCall) string) (*appConfig, *blockApplyTestDaemon, func()) {
	t.Helper()
	state := &blockApplyTestDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[{"windowId":"w1"}]}`))
		case "/action":
			var body struct {
				Action  string         `json:"action"`
				Payload map[string]any `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode action request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			call := blockApplyTestCall{Action: body.Action, Payload: body.Payload}
			state.mu.Lock()
			state.calls = append(state.calls, call)
			state.mu.Unlock()
			resp := responder(call)
			if resp == "" {
				resp = `{"ok":true,"result":{}}`
			}
			_, _ = w.Write([]byte(resp))
		default:
			http.NotFound(w, r)
		}
	}))

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portText, _ := strings.Cut(hostPort, ":")
	port, err := strconv.Atoi(portText)
	if err != nil {
		srv.Close()
		t.Fatalf("parse test daemon port: %v", err)
	}
	return &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}, state, srv.Close
}

func blockApplyPartsFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "standard-parts.json")
	raw := []byte(`{
		"libraryUuid": "lib",
		"parts": {
			"led.red_0805": {"deviceUuid": "dev-led", "lcsc": "C1"},
			"res.1k_0402": {"deviceUuid": "dev-res", "lcsc": "C2"}
		}
	}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write parts fixture: %v", err)
	}
	return path
}

const blockApplyOverlapGeometry = `{"ok":true,"result":{"components":[
	{"componentType":"part","designator":"LED1","primitiveId":"pid-led",
	 "bbox":{"minX":390,"minY":290,"maxX":410,"maxY":310},
	 "pinsAvailable":true,"pins":[{"pinNumber":"1","x":400,"y":300}]},
	{"componentType":"part","designator":"R1","primitiveId":"pid-r",
	 "bbox":{"minX":400,"minY":295,"maxX":420,"maxY":315},
	 "pinsAvailable":true,"pins":[{"pinNumber":"1","x":415,"y":305}]}
]}}`

const blockApplyCleanGeometry = `{"ok":true,"result":{"components":[
	{"componentType":"part","designator":"LED1","primitiveId":"pid-led",
	 "bbox":{"minX":390,"minY":290,"maxX":400,"maxY":300},
	 "pinsAvailable":true,"pins":[{"pinNumber":"1","x":390,"y":295}]},
	{"componentType":"part","designator":"R1","primitiveId":"pid-r",
	 "bbox":{"minX":410,"minY":290,"maxX":420,"maxY":300},
	 "pinsAvailable":true,"pins":[{"pinNumber":"1","x":420,"y":295}]}
]}}`

const blockApplyPinCoincidenceGeometry = `{"ok":true,"result":{"components":[
	{"componentType":"part","designator":"LED1","primitiveId":"pid-led",
	 "bbox":{"minX":390,"minY":290,"maxX":405,"maxY":310},
	 "pinsAvailable":true,"pins":[{"pinNumber":"1","x":405,"y":300}]},
	{"componentType":"part","designator":"R1","primitiveId":"pid-r",
	 "bbox":{"minX":405,"minY":290,"maxX":420,"maxY":310},
	 "pinsAvailable":true,"pins":[{"pinNumber":"2","x":405,"y":300}]}
]}}`

func blockApplyPlaceResponse(call int, includeID bool) string {
	id, designator := "pid-led", "LED1"
	if call == 2 {
		id, designator = "pid-r", "R1"
	}
	if !includeID {
		return fmt.Sprintf(`{"ok":true,"result":{"component":{"designator":%q}}}`, designator)
	}
	return fmt.Sprintf(`{"ok":true,"result":{"primitiveId":%q,"component":{"primitiveId":%q,"designator":%q}}}`,
		id, id, designator)
}

func assertBlockApplyStoppedBeforeWiring(t *testing.T, calls []blockApplyTestCall) {
	t.Helper()
	for _, call := range calls {
		switch call.Action {
		case "schematic.components.list", "schematic.component.place", "schematic.component.delete":
			// Placement and its compensating cleanup are the only allowed writes.
		case "document.current":
			// Read-only page pin issued by the group-registry leg of the delete
			// cascade (缺陷 2): a verified rollback strips the deleted designators
			// from the persistent-group table, which needs the active page uuid.
		default:
			t.Fatalf("action %q ran after the layout gate; calls=%+v", call.Action, calls)
		}
	}
}

func assertBlockRollbackReadbackIsDocumentWide(t *testing.T, calls []blockApplyTestCall) {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Action != "schematic.components.list" {
			continue
		}
		if calls[i].Payload["allPages"] != true || calls[i].Payload["tagPages"] != true {
			t.Fatalf("rollback read-back payload=%v, want allPages+tagPages", calls[i].Payload)
		}
		return
	}
	t.Fatal("no rollback components.list read-back found")
}

func TestRunBlockApplyOverlapStopsBeforeWiringAndRollsBack(t *testing.T) {
	listCalls, placeCalls := 0, 0
	cfg, daemon, cleanup := newBlockApplyTestDaemon(t, func(call blockApplyTestCall) string {
		switch call.Action {
		case "schematic.components.list":
			listCalls++
			switch listCalls {
			case 1, 2:
				return `{"ok":true,"result":{"components":[]}}`
			case 3:
				return blockApplyOverlapGeometry
			default:
				return `{"ok":true,"result":{"components":[]}}`
			}
		case "schematic.component.place":
			placeCalls++
			return blockApplyPlaceResponse(placeCalls, true)
		case "schematic.component.delete":
			return `{"ok":true,"result":{"deleted":true,"removed":2}}`
		case "document.current":
			// Group-registry cascade page pin; empty result → cascade fail-softs.
			return `{"ok":true,"result":{}}`
		default:
			t.Errorf("unexpected action %q", call.Action)
			return `{"ok":true,"result":{}}`
		}
	})
	defer cleanup()

	var stdout, stderr bytes.Buffer
	err := runBlockApply(cfg, "w1", "led_indicator_gpio", bapInput{},
		blockApplyPartsFixture(t), false, true, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "layout verification found 1 overlap") {
		t.Fatalf("err=%v, want hard overlap failure", err)
	}
	if strings.Contains(stderr.String(), "layout ✓") {
		t.Fatalf("false-green layout output:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "rollback ✓") {
		t.Fatalf("verified rollback not reported:\n%s", stderr.String())
	}

	var manifest bapManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode failure manifest: %v\n%s", err, stdout.String())
	}
	if manifest.OK != "failed-rolled-back" || manifest.PartialState {
		t.Fatalf("manifest status=%q partial=%v, want failed-rolled-back/non-partial", manifest.OK, manifest.PartialState)
	}
	if manifest.Rollback == nil || !manifest.Rollback.Complete || !manifest.Rollback.Verified {
		t.Fatalf("rollback manifest=%+v, want verified complete", manifest.Rollback)
	}
	if len(manifest.Placed) != 2 || manifest.Placed[0].PrimitiveID == "" || manifest.Placed[1].PrimitiveID == "" {
		t.Fatalf("placed primitive IDs not retained: %+v", manifest.Placed)
	}
	calls := daemon.snapshot()
	assertBlockApplyStoppedBeforeWiring(t, calls)
	assertBlockRollbackReadbackIsDocumentWide(t, calls)
}

func TestRunBlockApplyReadOrParseFailureStopsBeforeWiring(t *testing.T) {
	for _, tc := range []struct {
		name       string
		verifyBody string
		want       string
	}{
		{
			name:       "read failure",
			verifyBody: `{"ok":false,"error":{"message":"injected geometry read failure"}}`,
			want:       "read components with real bbox/pin geometry",
		},
		{
			name:       "parse failure",
			verifyBody: `{"ok":true,"result":{}}`,
			want:       "missing components array",
		},
		{
			name:       "incomplete geometry",
			verifyBody: `{"ok":true,"result":{"components":[{"componentType":"part","designator":"LED1","primitiveId":"pid-led","pins":[]}]}}`,
			want:       "has no bbox",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listCalls, placeCalls := 0, 0
			cfg, daemon, cleanup := newBlockApplyTestDaemon(t, func(call blockApplyTestCall) string {
				switch call.Action {
				case "schematic.components.list":
					listCalls++
					if listCalls <= 2 {
						return `{"ok":true,"result":{"components":[]}}`
					}
					if listCalls == 3 {
						return tc.verifyBody
					}
					return `{"ok":true,"result":{"components":[]}}`
				case "schematic.component.place":
					placeCalls++
					return blockApplyPlaceResponse(placeCalls, true)
				case "schematic.component.delete":
					return `{"ok":true,"result":{"deleted":true}}`
				case "document.current":
					// Group-registry cascade page pin; empty result → cascade fail-softs.
					return `{"ok":true,"result":{}}`
				default:
					t.Errorf("unexpected action %q", call.Action)
					return `{"ok":true,"result":{}}`
				}
			})
			defer cleanup()

			var stdout, stderr bytes.Buffer
			err := runBlockApply(cfg, "w1", "led_indicator_gpio", bapInput{},
				blockApplyPartsFixture(t), false, true, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if strings.Contains(stderr.String(), "layout ✓") {
				t.Fatalf("false-green layout output:\n%s", stderr.String())
			}
			var manifest bapManifest
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatalf("decode failure manifest: %v\n%s", err, stdout.String())
			}
			if manifest.OK != "failed-rolled-back" || manifest.Rollback == nil || !manifest.Rollback.Complete {
				t.Fatalf("manifest=%+v, want verified rollback failure", manifest)
			}
			assertBlockApplyStoppedBeforeWiring(t, daemon.snapshot())
		})
	}
}

func TestRunBlockApplyPinCoincidenceStopsBeforeWiring(t *testing.T) {
	listCalls, placeCalls := 0, 0
	cfg, daemon, cleanup := newBlockApplyTestDaemon(t, func(call blockApplyTestCall) string {
		switch call.Action {
		case "schematic.components.list":
			listCalls++
			if listCalls <= 2 {
				return `{"ok":true,"result":{"components":[]}}`
			}
			if listCalls == 3 {
				return blockApplyPinCoincidenceGeometry
			}
			return `{"ok":true,"result":{"components":[]}}`
		case "schematic.component.place":
			placeCalls++
			return blockApplyPlaceResponse(placeCalls, true)
		case "schematic.component.delete":
			return `{"ok":true,"result":{"deleted":true}}`
		case "document.current":
			// Group-registry cascade page pin; empty result → cascade fail-softs.
			return `{"ok":true,"result":{}}`
		default:
			t.Errorf("unexpected action %q", call.Action)
			return `{"ok":true,"result":{}}`
		}
	})
	defer cleanup()

	var stdout, stderr bytes.Buffer
	err := runBlockApply(cfg, "w1", "led_indicator_gpio", bapInput{},
		blockApplyPartsFixture(t), false, true, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "0 overlap(s) and 1 pin coincidence") {
		t.Fatalf("err=%v, want hard pin-coincidence failure", err)
	}
	if !strings.Contains(stderr.String(), "PIN COINCIDENCE") || strings.Contains(stderr.String(), "layout ✓") {
		t.Fatalf("pin/fake-green output wrong:\n%s", stderr.String())
	}
	var manifest bapManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode failure manifest: %v\n%s", err, stdout.String())
	}
	if manifest.OK != "failed-rolled-back" || len(manifest.LayoutOverlaps) != 1 ||
		manifest.LayoutOverlaps[0].Type != "pin-coincidence" {
		t.Fatalf("manifest=%+v, want rolled-back pin-coincidence", manifest)
	}
	assertBlockApplyStoppedBeforeWiring(t, daemon.snapshot())
}

func TestRunBlockApplyRollbackSurvivorReportsPartialState(t *testing.T) {
	listCalls, placeCalls := 0, 0
	cfg, daemon, cleanup := newBlockApplyTestDaemon(t, func(call blockApplyTestCall) string {
		switch call.Action {
		case "schematic.components.list":
			listCalls++
			if listCalls <= 2 {
				return `{"ok":true,"result":{"components":[]}}`
			}
			// The fourth list is rollback verification; deliberately keep both
			// IDs alive to model a connector/platform delete no-op.
			return blockApplyOverlapGeometry
		case "schematic.component.place":
			placeCalls++
			return blockApplyPlaceResponse(placeCalls, true)
		case "schematic.component.delete":
			return `{"ok":true,"result":{"deleted":false,"survived":["pid-led","pid-r"]}}`
		default:
			t.Errorf("unexpected action %q", call.Action)
			return `{"ok":true,"result":{}}`
		}
	})
	defer cleanup()

	var stdout, stderr bytes.Buffer
	err := runBlockApply(cfg, "w1", "led_indicator_gpio", bapInput{},
		blockApplyPartsFixture(t), false, true, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "PARTIAL STATE") {
		t.Fatalf("err=%v, want explicit partial state", err)
	}
	if !strings.Contains(stderr.String(), "PARTIAL STATE") || strings.Contains(stderr.String(), "layout ✓") {
		t.Fatalf("partial/fake-green output wrong:\n%s", stderr.String())
	}
	var manifest bapManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode failure manifest: %v\n%s", err, stdout.String())
	}
	if manifest.OK != "failed-partial" || !manifest.PartialState || manifest.Rollback == nil || manifest.Rollback.Complete {
		t.Fatalf("manifest=%+v, want explicit failed-partial", manifest)
	}
	if len(manifest.Rollback.SurvivedPrimitiveIDs) != 2 {
		t.Fatalf("survivors=%v, want both placed IDs", manifest.Rollback.SurvivedPrimitiveIDs)
	}
	assertBlockApplyStoppedBeforeWiring(t, daemon.snapshot())
}

func TestRunBlockApplyMissingPlacedIDDoesNotGuessRollbackTarget(t *testing.T) {
	listCalls := 0
	cfg, daemon, cleanup := newBlockApplyTestDaemon(t, func(call blockApplyTestCall) string {
		switch call.Action {
		case "schematic.components.list":
			listCalls++
			if listCalls <= 2 {
				return `{"ok":true,"result":{"components":[]}}`
			}
			return `{"ok":true,"result":{"components":[{"primitiveId":"unknown-new-id","designator":"LED1"}]}}`
		case "schematic.component.place":
			return blockApplyPlaceResponse(1, false)
		default:
			t.Errorf("unexpected action %q", call.Action)
			return `{"ok":true,"result":{}}`
		}
	})
	defer cleanup()

	var stdout, stderr bytes.Buffer
	err := runBlockApply(cfg, "w1", "led_indicator_gpio", bapInput{},
		blockApplyPartsFixture(t), false, true, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "PARTIAL STATE") {
		t.Fatalf("err=%v, want explicit partial state", err)
	}
	calls := daemon.snapshot()
	for _, call := range calls {
		if call.Action == "schematic.component.delete" {
			t.Fatalf("rollback guessed a delete target without a returned primitiveId: %+v", calls)
		}
	}
	var manifest bapManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode failure manifest: %v\n%s", err, stdout.String())
	}
	if !manifest.PartialState || manifest.Rollback == nil ||
		len(manifest.Rollback.MissingPrimitiveIDs) != 1 ||
		manifest.Rollback.MissingPrimitiveIDs[0] != "LED1" {
		t.Fatalf("manifest=%+v, want LED1 listed as untracked partial state", manifest)
	}
	assertBlockApplyStoppedBeforeWiring(t, calls)
}

func TestVerifyBlockLayoutAcceptsCompleteCleanGeometry(t *testing.T) {
	cfg, _, cleanup := newBlockApplyTestDaemon(t, func(call blockApplyTestCall) string {
		if call.Action != "schematic.components.list" {
			t.Errorf("unexpected action %q", call.Action)
		}
		return blockApplyCleanGeometry
	})
	defer cleanup()

	findings, _, err := verifyBlockLayout(cfg, "w1", []bapPlacement{
		{Designator: "LED1", PrimitiveID: "pid-led"},
		{Designator: "R1", PrimitiveID: "pid-r"},
	})
	if err != nil || len(findings) != 0 {
		t.Fatalf("findings=%+v err=%v, want proven clean", findings, err)
	}
}
