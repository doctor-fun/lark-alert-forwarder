package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const defaultRunnerOutputLimit = 64 << 10

type FallbackAnalyzer struct {
	Primary  Analyzer
	Fallback Analyzer
}

func (a FallbackAnalyzer) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisReport, error) {
	var primaryErr error
	if a.Primary != nil {
		report, err := a.Primary.Analyze(ctx, req)
		if err == nil {
			return report, nil
		}
		primaryErr = err
	}
	if a.Fallback == nil {
		return AnalysisReport{}, fmt.Errorf("copilot analyzer is not configured")
	}
	report, err := a.Fallback.Analyze(ctx, req)
	if err != nil {
		return AnalysisReport{}, err
	}
	if primaryErr != nil {
		report = markFallbackReport(report, primaryErr)
	}
	return report, nil
}

func markFallbackReport(report AnalysisReport, cause error) AnalysisReport {
	reason := compactFallbackReason(cause)
	if strings.TrimSpace(report.Title) == "" {
		report.Title = "告警 Copilot 规则兜底归因（AI runner 未完成）"
	} else if !strings.Contains(report.Title, "兜底") {
		report.Title = "规则兜底：" + report.Title
	}
	report.Facts = append([]string{
		"AI runner 未产出有效归因，当前报告为规则兜底（原因：" + reason + "）。",
	}, report.Facts...)
	report.Judgement = append([]string{
		"[证据不足] 本报告不是 attribution-agent 基于 SLS/trace 的完整 RCA，仅用于首响兜底；需要补齐 runner 输出后再下结论。",
	}, report.Judgement...)
	report.NextSteps = append([]string{
		"查看 matrix-agent-runner 日志确认 AI runner 超时/失败原因，必要时重新触发 AI 归因。",
	}, report.NextSteps...)
	report.References = append(report.References, "fallback: rule-based analyzer")
	return report
}

func compactFallbackReason(cause error) string {
	reason := strings.TrimSpace(cause.Error())
	if reason == "" {
		return "unknown"
	}
	reason = strings.Join(strings.Fields(reason), " ")
	runes := []rune(reason)
	if len(runes) > 180 {
		return string(runes[:177]) + "..."
	}
	return reason
}

type CommandAnalyzer struct {
	Command     string
	Timeout     time.Duration
	OutputLimit int
}

type commandAnalyzerInput struct {
	Version   string          `json:"version"`
	Request   AnalysisRequest `json:"request"`
	Generated time.Time       `json:"generated_at"`
}

func (a CommandAnalyzer) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisReport, error) {
	command := strings.TrimSpace(a.Command)
	if command == "" {
		return AnalysisReport{}, fmt.Errorf("copilot runner command is empty")
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	input, err := json.Marshal(commandAnalyzerInput{
		Version:   "alert-copilot/v1",
		Request:   req,
		Generated: time.Now(),
	})
	if err != nil {
		return AnalysisReport{}, err
	}

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Stdin = bytes.NewReader(input)

	var stdout, stderr limitedBuffer
	limit := a.OutputLimit
	if limit <= 0 {
		limit = defaultRunnerOutputLimit
	}
	stdout.limit = limit
	stderr.limit = limit
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return AnalysisReport{}, runCtx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			return AnalysisReport{}, err
		}
		return AnalysisReport{}, fmt.Errorf("%w: %s", err, detail)
	}

	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		return AnalysisReport{}, fmt.Errorf("copilot runner returned empty output")
	}

	var report AnalysisReport
	if err := json.Unmarshal([]byte(raw), &report); err == nil && (report.RawText != "" || report.Title != "" || len(report.Facts) > 0) {
		return report, nil
	}
	return AnalysisReport{
		RawText:     raw,
		GeneratedAt: time.Now(),
	}, nil
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}
