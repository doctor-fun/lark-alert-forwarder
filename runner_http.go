package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPRunnerAnalyzer talks to matrix-agent-runner over HTTP. The runner
// returns a structured AgentReport which we map onto the forwarder's
// AnalysisReport so existing card formatting keeps working.
type HTTPRunnerAnalyzer struct {
	BaseURL    string
	AgentName  string
	HTTPClient *http.Client
	Timeout    time.Duration
}

type runnerInvokeRequest struct {
	TaskID    string         `json:"task_id"`
	Context   map[string]any `json:"context"`
	CreatedAt time.Time      `json:"created_at"`
}

type runnerAgentReport struct {
	TaskID          string            `json:"task_id"`
	Agent           string            `json:"agent"`
	Status          string            `json:"status"`
	Title           string            `json:"title"`
	Facts           []json.RawMessage `json:"facts"`
	Judgement       json.RawMessage   `json:"judgement"`
	NextSteps       []json.RawMessage `json:"next_steps"`
	References      []json.RawMessage `json:"references"`
	DiagramPlantUML string            `json:"diagram_plantuml"`
	RawText         string            `json:"raw_text"`
	Error           string            `json:"error"`
	GeneratedAt     time.Time         `json:"generated_at"`
}

func (a HTTPRunnerAnalyzer) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisReport, error) {
	base := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if base == "" {
		return AnalysisReport{}, fmt.Errorf("runner base url is empty")
	}
	agentName := strings.TrimSpace(a.AgentName)
	if agentName == "" {
		agentName = "attribution-agent"
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload := runnerInvokeRequest{
		TaskID:    newRunnerTaskID(),
		Context:   analysisRequestToContext(req),
		CreatedAt: time.Now(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return AnalysisReport{}, err
	}

	endpoint := fmt.Sprintf("%s/agents/%s/invoke", base, agentName)
	httpReq, err := http.NewRequestWithContext(runCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return AnalysisReport{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout + 5*time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if runCtx.Err() != nil {
			return AnalysisReport{}, runCtx.Err()
		}
		return AnalysisReport{}, fmt.Errorf("runner request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return AnalysisReport{}, fmt.Errorf("read runner response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(respBody))
		if len(detail) > 256 {
			detail = detail[:256]
		}
		return AnalysisReport{}, fmt.Errorf("runner status %d: %s", resp.StatusCode, detail)
	}

	var report runnerAgentReport
	if err := json.Unmarshal(respBody, &report); err != nil {
		return AnalysisReport{}, fmt.Errorf("decode runner report: %w", err)
	}
	if report.Status != "" && report.Status != "success" {
		if report.Error != "" {
			return AnalysisReport{}, fmt.Errorf("runner status=%s error=%s", report.Status, report.Error)
		}
		return AnalysisReport{}, fmt.Errorf("runner status=%s", report.Status)
	}
	return AnalysisReport{
		Title:           report.Title,
		Facts:           flattenJSONList(report.Facts),
		Judgement:       judgementToLines(report.Judgement),
		NextSteps:       flattenJSONList(report.NextSteps),
		References:      flattenJSONList(report.References),
		DiagramPlantUML: strings.TrimSpace(report.DiagramPlantUML),
		RawText:         report.RawText,
		GeneratedAt:     report.GeneratedAt,
	}, nil
}

// judgementToLines accepts either a single string or an array, returning a
// []string suitable for AnalysisReport.Judgement (which is rendered as one
// bullet per line).
func judgementToLines(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		return flattenJSONList(arr)
	}
	if s := flattenJSONField(raw); s != "" {
		return []string{s}
	}
	return nil
}

// flattenJSONList renders each item in a JSON array as a human-readable
// string. Items that arrive as plain strings come through verbatim; objects
// are rendered using flattenJSONObject.
func flattenJSONList(items []json.RawMessage) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		if s := flattenJSONField(raw); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// flattenJSONField mirrors the runner-side JSONValue.String() helper but
// returns []string when the field itself is a JSON array (so a top-level
// `judgement` that happens to be an array is split into one bullet per item).
func flattenJSONField(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		parts := make([]string, 0, len(arr))
		for _, item := range arr {
			if v := flattenJSONField(item); v != "" {
				parts = append(parts, v)
			}
		}
		return strings.Join(parts, "\n- ")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		return flattenJSONObject(obj)
	}
	return trimmed
}

// flattenJSONObject mirrors api.objectToString in the runner. Kept in sync by
// hand because forwarder is its own module; the format is intentionally
// simple and stable.
func flattenJSONObject(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	preferredKeys := []string{
		"fact", "step", "action", "value", "summary", "description",
	}
	var primary string
	for _, k := range preferredKeys {
		if raw, ok := obj[k]; ok {
			primary = strings.TrimSpace(fmt.Sprint(raw))
			if primary != "" {
				break
			}
		}
	}
	suffixKeys := []string{"source", "purpose", "link", "url", "keywords"}
	var suffixes []string
	for _, k := range suffixKeys {
		if raw, ok := obj[k]; ok {
			s := strings.TrimSpace(fmt.Sprint(raw))
			if s != "" {
				suffixes = append(suffixes, fmt.Sprintf("%s=%s", k, s))
			}
		}
	}
	if primary == "" {
		buf, _ := json.Marshal(obj)
		return string(buf)
	}
	if len(suffixes) == 0 {
		return primary
	}
	return primary + "（" + strings.Join(suffixes, "; ") + "）"
}

// analysisRequestToContext flattens the AnalysisRequest into a map[string]any
// so the runner agent prompt can iterate over context fields without knowing
// the forwarder's struct shape.
func analysisRequestToContext(req AnalysisRequest) map[string]any {
	ctx := map[string]any{
		"alertname":     req.AlertName,
		"service":       req.Service,
		"env":           req.Env,
		"severity":      req.Severity,
		"status":        req.Status,
		"category":      req.Category,
		"route":         req.Route,
		"target":        req.Target,
		"operation":     req.Operation,
		"pod":           req.Pod,
		"current_value": req.CurrentValue,
		"summary":       req.Summary,
		"description":   req.Description,
		"link":          req.Link,
		"operator":      req.Operator,
	}
	if !req.TriggeredAt.IsZero() {
		ctx["triggered_at"] = req.TriggeredAt
	}
	return ctx
}

func newRunnerTaskID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("alert-%d", time.Now().UnixNano())
	}
	return "alert-" + hex.EncodeToString(buf[:])
}
