package main

// feedback_cursor.go 调用 matrix-agent-runner 的 feedback-digest agent（cursor CLI）
// 对一周反馈正文做语义聚类，产出周报的「AI 洞察」板块。forwarder 自己没有
// cursor CLI，复用告警 Copilot 已经在用的 runner HTTP 通道（COPILOT_RUNNER_URL）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// feedbackAIInsight 是 cursor 语义聚类的结构化结果。
type feedbackAIInsight struct {
	Title     string   `json:"title"`
	TopIssues []string `json:"top_issues"`
	Judgement []string `json:"judgement"`
	NextSteps []string `json:"next_steps"`
}

const feedbackDigestMaxSamples = 150

// clusterFeedbackWithCursor 把本周反馈正文喂给 feedback-digest agent，拿回
// 语义化的 Top 问题 + 优先级判断 + 建议。任何失败都返回 error，由调用方降级。
func (s *server) clusterFeedbackWithCursor(ctx context.Context, stats *feedbackWeeklyStats) (*feedbackAIInsight, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("COPILOT_RUNNER_URL")), "/")
	if base == "" {
		return nil, fmt.Errorf("COPILOT_RUNNER_URL not set")
	}
	if stats == nil || len(stats.sampleFeedbacks) == 0 {
		return nil, fmt.Errorf("no feedback samples to cluster")
	}
	agent := envOrDefault("FEEDBACK_DIGEST_AGENT", "feedback-digest-agent")

	samples := stats.sampleFeedbacks
	if len(samples) > feedbackDigestMaxSamples {
		samples = samples[:feedbackDigestMaxSamples]
	}
	ruleTop := make([]string, 0, len(stats.TopIssues))
	for _, it := range stats.TopIssues {
		ruleTop = append(ruleTop, fmt.Sprintf("%s:%d", it.Category, it.Count))
	}
	loc := feedbackWeeklyLoc()
	window := stats.WindowStart.In(loc).Format("2006-01-02") + " ~ " +
		stats.WindowEnd.In(loc).AddDate(0, 0, -1).Format("2006-01-02")

	payload := runnerInvokeRequest{
		TaskID: newRunnerTaskID(),
		Context: map[string]any{
			"window":           window,
			"feedback_count":   stats.FeedbackCount,
			"rule_top_issues":  ruleTop,
			"feedback_samples": samples,
		},
		CreatedAt: time.Now(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	timeout := durationFromEnv("FEEDBACK_DIGEST_TIMEOUT", 100*time.Second)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := fmt.Sprintf("%s/agents/%s/invoke", base, agent)
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout + 5*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if runCtx.Err() != nil {
			return nil, runCtx.Err()
		}
		return nil, fmt.Errorf("runner request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read runner response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner status %d: %s", resp.StatusCode, truncateMsg(strings.TrimSpace(string(respBody)), 256))
	}

	var report runnerAgentReport
	if err := json.Unmarshal(respBody, &report); err != nil {
		return nil, fmt.Errorf("decode runner report: %w", err)
	}
	if report.Status != "" && report.Status != "success" {
		if report.Error != "" {
			return nil, fmt.Errorf("runner status=%s error=%s", report.Status, report.Error)
		}
		return nil, fmt.Errorf("runner status=%s", report.Status)
	}

	insight := &feedbackAIInsight{
		Title:     strings.TrimSpace(report.Title),
		TopIssues: flattenJSONList(report.Facts),
		Judgement: judgementToLines(report.Judgement),
		NextSteps: flattenJSONList(report.NextSteps),
	}
	if len(insight.TopIssues) == 0 && insight.Title == "" {
		return nil, fmt.Errorf("empty digest from runner")
	}
	return insight, nil
}
