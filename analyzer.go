package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Analyzer interface {
	Analyze(ctx context.Context, req AnalysisRequest) (AnalysisReport, error)
}

type AnalysisRequest struct {
	AlertName    string `json:"alertname"`
	Service      string `json:"service"`
	Env          string `json:"env"`
	Severity     string `json:"severity"`
	Status       string `json:"status"`
	Category     string `json:"category,omitempty"`
	Route        string `json:"route,omitempty"`
	Target       string `json:"target,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Pod          string `json:"pod,omitempty"`
	CurrentValue string `json:"current_value,omitempty"`
	Summary      string `json:"summary"`
	Description  string `json:"description"`
	Link         string `json:"link"`
	// Operator is the human-readable label rendered in the card body
	// (e.g. "@Alice"). Kept separate from OperatorOpenID so we
	// don't accidentally render an open_id in the UI.
	Operator string `json:"operator"`
	// OperatorOpenID is the Feishu open_id of the user who clicked the
	// "AI 归因" button. Used at email-delivery time to look up their
	// mailbox via the contact API. Empty when the call did not originate
	// from a Feishu interactive card (e.g. unit tests, REST clients).
	OperatorOpenID string `json:"operator_open_id,omitempty"`
	// OpenChatID is the Feishu chat ID the original alert card lives in.
	// Carried through so the email pipeline can expand "one person
	// clicked" into "broadcast to every configured member of the chat",
	// which is the desired behaviour for an on-call room.
	OpenChatID  string    `json:"open_chat_id,omitempty"`
	TriggeredAt time.Time `json:"triggered_at,omitempty"`
}

type AnalysisReport struct {
	RawText         string    `json:"raw_text,omitempty"`
	Title           string    `json:"title,omitempty"`
	Facts           []string  `json:"facts,omitempty"`
	Judgement       []string  `json:"judgement,omitempty"`
	NextSteps       []string  `json:"next_steps,omitempty"`
	References      []string  `json:"references,omitempty"`
	DiagramPlantUML string    `json:"diagram_plantuml,omitempty"`
	GeneratedAt     time.Time `json:"generated_at,omitempty"`
}

type RuleBasedAnalyzer struct{}

func (RuleBasedAnalyzer) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisReport, error) {
	select {
	case <-ctx.Done():
		return AnalysisReport{}, ctx.Err()
	default:
	}

	alertName := firstNonEmpty(req.AlertName, "Grafana 告警")
	service := firstNonEmpty(req.Service, "-")
	env := firstNonEmpty(req.Env, "-")
	status := strings.ToUpper(firstNonEmpty(req.Status, "unknown"))
	severity := firstNonEmpty(req.Severity, "-")

	facts := []string{
		fmt.Sprintf("告警：%s", alertName),
		fmt.Sprintf("服务/环境：%s / %s", service, env),
		fmt.Sprintf("状态/级别：%s / %s", status, severity),
	}
	if req.Summary != "" && req.Summary != "-" {
		facts = append(facts, "摘要："+req.Summary)
	}
	if impact := analysisImpactLine(req); impact != "" {
		facts = append(facts, "影响面："+impact)
	}
	if req.CurrentValue != "" {
		facts = append(facts, "当前值："+req.CurrentValue)
	}
	if req.TriggeredAt.IsZero() {
		facts = append(facts, "触发时间：未从卡片上下文获取")
	} else {
		facts = append(facts, "触发时间："+formatAlertTime(req.TriggeredAt))
	}

	report := AnalysisReport{
		Title:       "告警 Copilot 只读归因",
		Facts:       facts,
		Judgement:   judgementFor(req),
		NextSteps:   nextStepsFor(req),
		GeneratedAt: time.Now(),
	}
	if req.Link != "" {
		report.References = append(report.References, req.Link)
	}
	return report, nil
}

func judgementFor(req AnalysisRequest) []string {
	name := strings.ToLower(req.AlertName)
	description := strings.ToLower(req.Description + " " + req.Summary)

	switch {
	case strings.Contains(name, "downstream") || req.Target != "":
		return []string{
			"优先判断为下游依赖退化，需要先按 target 和 error_type 定位具体依赖。",
			"如果入口 5xx 或业务失败率同步升高，再按用户影响升级处理。",
		}
	case strings.Contains(name, "pod") || strings.Contains(name, "restart") || req.Pod != "":
		return []string{
			"优先判断为实例稳定性问题，需要检查 ACK 事件、OOMKilled、CrashLoopBackOff 和最近发布。",
		}
	case strings.Contains(name, "5xx") || strings.Contains(description, "5xx"):
		return []string{
			"优先判断为服务端错误或下游依赖错误，需要先看同时间窗 ERROR 日志。",
			"如果近期有发布，先把发布时间、错误开始时间和异常接口对齐。",
		}
	case strings.Contains(name, "p99") || strings.Contains(name, "latency") || strings.Contains(description, "延迟"):
		return []string{
			"优先判断为延迟类问题，需要区分整体流量上涨、慢依赖、DB 慢查询和实例资源抖动。",
			"如果错误率没有同步升高，先按最慢接口和下游耗时拆解。",
		}
	case strings.Contains(name, "success") || strings.Contains(description, "成功率"):
		return []string{
			"优先判断为成功率下降，需要同时看 4xx/5xx 占比，避免把业务拒绝误判为系统故障。",
		}
	case strings.Contains(name, "log") || strings.Contains(description, "error") || strings.Contains(description, "panic"):
		return []string{
			"优先判断为日志规则触发，需要按服务和时间窗聚合高频错误签名。",
		}
	default:
		return []string{
			"当前为规则型初判，尚未接入 Prometheus、SLS 和发布事实，只能给出排查路径。",
		}
	}
}

func nextStepsFor(req AnalysisRequest) []string {
	service := firstNonEmpty(req.Service, "<service>")
	env := firstNonEmpty(req.Env, "prod")
	steps := []string{
		fmt.Sprintf("查询 %s/%s 在告警前后 10 分钟的 SLS 日志，聚合 ERROR、panic、timeout。", env, service),
		"查看告警面板对应指标是否与流量、实例数或发布事件同步变化。",
		"确认最近一次云效流水线、镜像版本和 ACK rollout 时间。",
	}
	if req.Route != "" {
		steps = append(steps, "优先过滤 route="+req.Route+" 的 HTTP 指标和日志，确认是否单接口退化。")
	}
	if req.Target != "" {
		steps = append(steps, "优先检查下游 target="+req.Target+" 的错误类型、耗时和实例状态。")
	}
	if req.Pod != "" {
		steps = append(steps, "优先检查 Pod "+req.Pod+" 的重启原因、上一版镜像和节点事件。")
	}
	if req.Link != "" {
		steps = append(steps, "打开大盘链接确认异常是否仍在持续。")
	}
	steps = append(steps, "如需修复，先在线程里沉淀候选方案，再走审批按钮执行。")
	return steps
}

func analysisImpactLine(req AnalysisRequest) string {
	var parts []string
	for _, item := range []struct {
		label string
		value string
	}{
		{"category", req.Category},
		{"route", req.Route},
		{"target", req.Target},
		{"operation", req.Operation},
		{"pod", req.Pod},
	} {
		if strings.TrimSpace(item.value) != "" {
			parts = append(parts, item.label+"="+item.value)
		}
	}
	return strings.Join(parts, "，")
}

func (r AnalysisReport) FormatText() string {
	if strings.TrimSpace(r.RawText) != "" {
		return strings.TrimSpace(r.RawText)
	}

	var b strings.Builder
	title := firstNonEmpty(r.Title, "告警 Copilot 只读归因")
	fmt.Fprintf(&b, "%s\n\n", title)
	writeReportSection(&b, "事实", r.Facts)
	writeReportSection(&b, "判断", r.Judgement)
	writeReportSection(&b, "建议下一步", r.NextSteps)
	writeReportSection(&b, "参考", r.References)
	if diagram := strings.TrimSpace(r.DiagramPlantUML); diagram != "" {
		fmt.Fprintf(&b, "RCA 图（PlantUML）：\n%s\n\n", diagram)
	}
	if !r.GeneratedAt.IsZero() {
		fmt.Fprintf(&b, "\n生成时间：%s", formatAlertTime(r.GeneratedAt))
	}
	return strings.TrimSpace(b.String())
}

func writeReportSection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s：\n", title)
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			continue
		}
		fmt.Fprintf(b, "- %s\n", strings.TrimSpace(item))
	}
	b.WriteString("\n")
}
