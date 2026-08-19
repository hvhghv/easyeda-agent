package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type cliTransactionTarget struct {
	host     string
	port     int
	windowID string
}

var cliTransactionState struct {
	sync.Mutex
	id      string
	depth   int
	targets map[string]cliTransactionTarget
}

// beginCLITransaction brackets one top-level CLI invocation. Unit tests that
// call command constructors directly stay legacy-compatible; the real app.Run
// path always carries a transaction id and releases every touched window.
func beginCLITransaction() func() {
	cliTransactionState.Lock()
	if cliTransactionState.id != "" {
		cliTransactionState.depth++
		cliTransactionState.Unlock()
		return releaseCLITransactions
	}
	cliTransactionState.id = cliClientID()
	cliTransactionState.depth = 1
	cliTransactionState.targets = map[string]cliTransactionTarget{}
	cliTransactionState.Unlock()
	return releaseCLITransactions
}

func currentCLITransactionID() string {
	cliTransactionState.Lock()
	defer cliTransactionState.Unlock()
	return cliTransactionState.id
}

func trackCLITransactionTarget(host string, port int, response []byte) {
	var envelope struct {
		WindowID string `json:"windowId"`
	}
	if json.Unmarshal(response, &envelope) != nil || envelope.WindowID == "" {
		return
	}
	cliTransactionState.Lock()
	defer cliTransactionState.Unlock()
	if cliTransactionState.id == "" {
		return
	}
	key := fmt.Sprintf("%s:%d/%s", host, port, envelope.WindowID)
	cliTransactionState.targets[key] = cliTransactionTarget{host: host, port: port, windowID: envelope.WindowID}
}

func releaseCLITransactions() {
	cliTransactionState.Lock()
	if cliTransactionState.depth > 1 {
		cliTransactionState.depth--
		cliTransactionState.Unlock()
		return
	}
	id := cliTransactionState.id
	targets := make([]cliTransactionTarget, 0, len(cliTransactionState.targets))
	for _, target := range cliTransactionState.targets {
		targets = append(targets, target)
	}
	cliTransactionState.id = ""
	cliTransactionState.depth = 0
	cliTransactionState.targets = nil
	cliTransactionState.Unlock()
	if id == "" {
		return
	}
	for _, target := range targets {
		body, _ := json.Marshal(map[string]any{
			"action":        "system.transaction.release",
			"windowId":      target.windowID,
			"clientId":      cliClientID(),
			"transactionId": id,
			"timeoutMs":     2000,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("http://%s:%d/action", target.host, target.port), bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, doErr := http.DefaultClient.Do(req); doErr == nil {
				_ = resp.Body.Close()
			}
		}
		cancel()
	}
}
