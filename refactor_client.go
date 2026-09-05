package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type RefactorOrchestratorClient struct {
	BaseURL    string
	HTTPClient *http.Client
	Timeout    time.Duration
}

type refactorMetricTrigger struct {
	Repo        string            `json:"repo"`
	Service     string            `json:"service"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	AutoEnqueue bool              `json:"auto_enqueue,omitempty"`
}

type refactorEnqueueResult struct {
	Duplicate bool   `json:"duplicate"`
	ItemID    string `json:"item_id"`
}

func (c RefactorOrchestratorClient) EnqueueMetric(ctx context.Context, trigger refactorMetricTrigger) (refactorEnqueueResult, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return refactorEnqueueResult{}, fmt.Errorf("refactor orchestrator base url is empty")
	}
	trigger.Repo = strings.TrimSpace(trigger.Repo)
	trigger.Service = strings.TrimSpace(trigger.Service)
	if trigger.Repo == "" {
		return refactorEnqueueResult{}, fmt.Errorf("refactor repo is empty")
	}
	body, err := json.Marshal(trigger)
	if err != nil {
		return refactorEnqueueResult{}, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, base+"/refactor/triggers/metric", bytes.NewReader(body))
	if err != nil {
		return refactorEnqueueResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout + time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if runCtx.Err() != nil {
			return refactorEnqueueResult{}, runCtx.Err()
		}
		return refactorEnqueueResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return refactorEnqueueResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return refactorEnqueueResult{}, fmt.Errorf("refactor orchestrator status %d: %s", resp.StatusCode, truncateMsg(strings.TrimSpace(string(respBody)), 256))
	}
	var raw struct {
		Duplicate bool `json:"duplicate"`
		Item      struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return refactorEnqueueResult{}, err
	}
	return refactorEnqueueResult{Duplicate: raw.Duplicate, ItemID: raw.Item.ID}, nil
}
