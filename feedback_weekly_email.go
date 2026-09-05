package main

// feedback_weekly_email.go 负责把 feedbackWeeklyStats 渲染成周报邮件
// （HTML 主体 + 纯文本兜底），视觉风格与告警 Copilot 邮件一致（内联 CSS、
// 卡片 + 表格 + 进度条），保证企业邮箱客户端不丢样式。

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"strings"
	"time"
)

type feedbackDayBar struct {
	Date  string
	Count int
	Width int
}

type feedbackTopIssueRow struct {
	Category string
	Count    int
	Width    int
	Samples  string
}

type feedbackWorkOrderView struct {
	Total            int
	PrevTotal        int
	DeltaLabel       string
	Untouched        int
	Processing       int
	Backlog          int
	Closed           int
	ClosedByUser     int
	ClosedByAuto     int
	ClosedByAdmin    int
	Replied          int
	UnReplied        int
	RepliedWithin24h int
	CurrentUnreplied int
	Response24hRate  string
	AvgFirstReply    string
	MaxFirstReply    string
	MaxUnrepliedAge  string
	TopVersions      string
	TopLanguages     string
}

type feedbackWeeklyEmailData struct {
	Title         string
	HeaderColor   string
	Env           string
	WindowLabel   string
	GeneratedAt   string
	FeedbackCount int
	FollowupCount int
	TotalMessages int
	DailyBars     []feedbackDayBar
	TopIssues     []feedbackTopIssueRow
	TopIssueBy    string
	HasAI         bool
	AITitle       string
	AIIssues      []string
	AIJudgement   []string
	AINextSteps   []string
	HasWorkOrder  bool
	WorkOrder     feedbackWorkOrderView
	Insights      []string
}

func feedbackWeeklyEnv() string {
	return nonEmpty(strings.TrimSpace(os.Getenv("FEEDBACK_WEEKLY_ENV")), "prod")
}

// renderFeedbackWeeklyEmail 把统计渲染成可发送的 EmailMessage。
func renderFeedbackWeeklyEmail(stats feedbackWeeklyStats, to []string, loc *time.Location) (EmailMessage, error) {
	if len(to) == 0 {
		return EmailMessage{}, fmt.Errorf("renderFeedbackWeeklyEmail: at least one recipient required")
	}
	startLabel := stats.WindowStart.In(loc).Format("2006-01-02")
	endLabel := stats.WindowEnd.In(loc).AddDate(0, 0, -1).Format("2006-01-02")
	windowLabel := startLabel + " ~ " + endLabel

	data := feedbackWeeklyEmailData{
		Title:         "📮 用户反馈周报",
		HeaderColor:   "#3370ff",
		Env:           feedbackWeeklyEnv(),
		WindowLabel:   windowLabel,
		GeneratedAt:   stats.GeneratedAt.In(loc).Format("2006-01-02 15:04:05"),
		FeedbackCount: stats.FeedbackCount,
		FollowupCount: stats.FollowupCount,
		TotalMessages: stats.TotalMessages,
		DailyBars:     buildFeedbackDayBars(stats.DailyCounts),
		TopIssues:     buildFeedbackTopIssueRows(stats.TopIssues),
		TopIssueBy:    feedbackTopIssueByLabel(stats.TopIssueBy),
		Insights:      buildFeedbackInsights(stats),
	}
	if stats.AIInsight != nil {
		data.HasAI = true
		data.AITitle = stats.AIInsight.Title
		data.AIIssues = stats.AIInsight.TopIssues
		data.AIJudgement = stats.AIInsight.Judgement
		data.AINextSteps = stats.AIInsight.NextSteps
	}
	if stats.WorkOrder != nil {
		data.HasWorkOrder = true
		data.WorkOrder = buildFeedbackWorkOrderView(*stats.WorkOrder)
	}

	t, err := template.New("feedback-weekly").Parse(feedbackWeeklyHTMLTemplate)
	if err != nil {
		return EmailMessage{}, fmt.Errorf("parse feedback weekly template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return EmailMessage{}, fmt.Errorf("execute feedback weekly template: %w", err)
	}
	subject := fmt.Sprintf("[用户反馈周报][%s] %s 反馈%d条", data.Env, windowLabel, stats.FeedbackCount)
	return EmailMessage{
		To:       to,
		Subject:  subject,
		BodyText: renderFeedbackWeeklyText(stats, windowLabel, loc),
		BodyHTML: buf.String(),
	}, nil
}

func feedbackTopIssueByLabel(by string) string {
	if by == "cursor" {
		return "AI 语义聚类"
	}
	return "关键词规则聚类"
}

func buildFeedbackDayBars(daily []feedbackDailyCount) []feedbackDayBar {
	max := 0
	for _, d := range daily {
		if d.Count > max {
			max = d.Count
		}
	}
	out := make([]feedbackDayBar, 0, len(daily))
	for _, d := range daily {
		width := 0
		if max > 0 {
			width = d.Count * 100 / max
			if width < 4 {
				width = 4
			}
		}
		out = append(out, feedbackDayBar{Date: d.Date, Count: d.Count, Width: width})
	}
	return out
}

func buildFeedbackTopIssueRows(issues []feedbackTopIssue) []feedbackTopIssueRow {
	max := 0
	for _, it := range issues {
		if it.Count > max {
			max = it.Count
		}
	}
	out := make([]feedbackTopIssueRow, 0, len(issues))
	for _, it := range issues {
		width := 0
		if max > 0 {
			width = it.Count * 100 / max
			if width < 4 {
				width = 4
			}
		}
		out = append(out, feedbackTopIssueRow{
			Category: it.Category,
			Count:    it.Count,
			Width:    width,
			Samples:  strings.Join(it.Samples, "；"),
		})
	}
	return out
}

func buildFeedbackInsights(stats feedbackWeeklyStats) []string {
	var out []string
	if stats.FeedbackCount > 0 {
		out = append(out, fmt.Sprintf("本周飞书群共收到 %d 条用户反馈，群内人工跟进/讨论 %d 条。",
			stats.FeedbackCount, stats.FollowupCount))
	}
	if len(stats.TopIssues) > 0 {
		top := stats.TopIssues[0]
		out = append(out, fmt.Sprintf("反馈最集中的是「%s」（%d 条），建议优先排查。", top.Category, top.Count))
	}
	if stats.WorkOrder == nil {
		out = append(out, "App 内工单（work_order）数据将在下一步接入，届时补充处理时效与积压情况。")
	}
	return out
}

func buildFeedbackWorkOrderView(wo workOrderWeeklyStats) feedbackWorkOrderView {
	delta := "—"
	if wo.PrevTotal > 0 {
		pct := float64(wo.Total-wo.PrevTotal) / float64(wo.PrevTotal) * 100
		sign := "+"
		if pct < 0 {
			sign = ""
		}
		delta = fmt.Sprintf("%s%.0f%%", sign, pct)
	}
	return feedbackWorkOrderView{
		Total:            wo.Total,
		PrevTotal:        wo.PrevTotal,
		DeltaLabel:       delta,
		Untouched:        wo.Untouched,
		Processing:       wo.Processing,
		Backlog:          wo.Untouched + wo.Processing,
		Closed:           wo.Closed,
		ClosedByUser:     wo.ClosedByUser,
		ClosedByAuto:     wo.ClosedByAuto,
		ClosedByAdmin:    wo.ClosedByAdmin,
		Replied:          wo.Replied,
		UnReplied:        wo.CurrentUnreplied,
		RepliedWithin24h: wo.RepliedWithin24h,
		CurrentUnreplied: wo.CurrentUnreplied,
		Response24hRate:  feedbackRatioLabel(wo.RepliedWithin24h, wo.Total),
		AvgFirstReply:    feedbackMinutesLabel(wo.AvgFirstReplyM),
		MaxFirstReply:    feedbackMinutesLabel(wo.MaxFirstReplyM),
		MaxUnrepliedAge:  feedbackMinutesLabel(wo.MaxUnrepliedAgeM),
		TopVersions:      feedbackLabelCountsJoin(wo.TopVersions),
		TopLanguages:     feedbackLabelCountsJoin(wo.TopLanguages),
	}
}

func feedbackRatioLabel(part, total int) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d（%.0f%%）", part, total, float64(part)/float64(total)*100)
}

func feedbackMinutesLabel(m float64) string {
	if m <= 0 {
		return "-"
	}
	if m >= 60 {
		return fmt.Sprintf("%.1fh", m/60)
	}
	return fmt.Sprintf("%.0fm", m)
}

func feedbackLabelCountsJoin(items []feedbackLabelCount) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s(%d)", it.Label, it.Count))
	}
	return strings.Join(parts, " · ")
}

func renderFeedbackWeeklyText(stats feedbackWeeklyStats, windowLabel string, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "用户反馈周报 %s\n\n", windowLabel)
	fmt.Fprintf(&b, "一、飞书反馈群（客服渠道）\n")
	fmt.Fprintf(&b, "- 本周反馈：%d 条\n", stats.FeedbackCount)
	fmt.Fprintf(&b, "- 群内人工跟进/讨论：%d 条\n", stats.FollowupCount)
	fmt.Fprintf(&b, "- 群消息总数：%d 条\n\n", stats.TotalMessages)
	if len(stats.DailyCounts) > 0 {
		fmt.Fprintf(&b, "每日趋势：")
		parts := make([]string, 0, len(stats.DailyCounts))
		for _, d := range stats.DailyCounts {
			parts = append(parts, fmt.Sprintf("%s=%d", d.Date, d.Count))
		}
		fmt.Fprintf(&b, "%s\n\n", strings.Join(parts, "  "))
	}
	if len(stats.TopIssues) > 0 {
		fmt.Fprintf(&b, "本周 Top 问题（%s）：\n", feedbackTopIssueByLabel(stats.TopIssueBy))
		for i, it := range stats.TopIssues {
			fmt.Fprintf(&b, "%d. %s（%d 条）", i+1, it.Category, it.Count)
			if len(it.Samples) > 0 {
				fmt.Fprintf(&b, "：%s", strings.Join(it.Samples, "；"))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if stats.AIInsight != nil {
		ai := stats.AIInsight
		fmt.Fprintf(&b, "AI 洞察（Cursor 语义聚类）\n")
		if ai.Title != "" {
			fmt.Fprintf(&b, "%s\n", ai.Title)
		}
		for _, it := range ai.TopIssues {
			fmt.Fprintf(&b, "- %s\n", it)
		}
		for _, j := range ai.Judgement {
			fmt.Fprintf(&b, "[优先级] %s\n", j)
		}
		for _, n := range ai.NextSteps {
			fmt.Fprintf(&b, "[建议] %s\n", n)
		}
		b.WriteString("\n")
	}
	if stats.WorkOrder != nil {
		wo := buildFeedbackWorkOrderView(*stats.WorkOrder)
		fmt.Fprintf(&b, "二、App 内工单（work_order）\n")
		fmt.Fprintf(&b, "- 新增 %d（上周 %d，环比 %s）\n", wo.Total, wo.PrevTotal, wo.DeltaLabel)
		fmt.Fprintf(&b, "- 当前待跟进 %d（未处理 %d + 处理中 %d）\n", wo.Backlog, wo.Untouched, wo.Processing)
		fmt.Fprintf(&b, "- 当前未响应 %d，最长等待 %s\n", wo.CurrentUnreplied, wo.MaxUnrepliedAge)
		fmt.Fprintf(&b, "- 已完结 %d（用户/自动/客服：%d/%d/%d）\n",
			wo.Closed, wo.ClosedByUser, wo.ClosedByAuto, wo.ClosedByAdmin)
		fmt.Fprintf(&b, "- 说明：当前未响应仅统计未处理/处理中且无客服回复的工单；已完结工单不计入。时长按自然时间计算，遇周末/非工作时间可能偏长。\n")
		fmt.Fprintf(&b, "- 版本：%s ｜ 语种：%s\n\n", wo.TopVersions, wo.TopLanguages)
	}
	fmt.Fprintf(&b, "生成时间：%s\n", stats.GeneratedAt.In(loc).Format("2006-01-02 15:04:05"))
	return b.String()
}

const feedbackWeeklyHTMLTemplate = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"/></head>
<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,'PingFang SC','Microsoft YaHei',sans-serif;color:#1f2329;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f4f5f7;padding:24px 0;">
    <tr><td align="center">
      <table width="680" cellpadding="0" cellspacing="0" border="0" style="background:#ffffff;border-radius:10px;overflow:hidden;box-shadow:0 1px 4px rgba(0,0,0,0.08);">
        <tr><td style="background:{{.HeaderColor}};color:#ffffff;padding:20px 24px;">
          <div style="font-size:18px;font-weight:700;">{{.Title}}</div>
          <div style="font-size:12px;margin-top:8px;opacity:.9;">环境 {{.Env}} · {{.WindowLabel}}</div>
        </td></tr>

        <tr><td style="padding:18px 24px 6px;">
          <table width="100%" cellpadding="0" cellspacing="0" border="0">
            <tr>
              <td style="background:#f0f5ff;border-radius:8px;padding:14px;text-align:center;"><div style="font-size:12px;color:#646a73;">飞书群反馈</div><div style="font-size:26px;font-weight:700;color:#3370ff;">{{.FeedbackCount}}</div></td>
              <td width="12"></td>
              <td style="background:#f6ffed;border-radius:8px;padding:14px;text-align:center;"><div style="font-size:12px;color:#646a73;">人工跟进</div><div style="font-size:26px;font-weight:700;color:#00b96b;">{{.FollowupCount}}</div></td>
              <td width="12"></td>
              <td style="background:#f7f8fa;border-radius:8px;padding:14px;text-align:center;"><div style="font-size:12px;color:#646a73;">群消息总数</div><div style="font-size:26px;font-weight:700;">{{.TotalMessages}}</div></td>
            </tr>
          </table>
        </td></tr>

        {{if .Insights}}
        <tr><td style="padding:6px 24px;">
          <ul style="margin:8px 0;padding-left:20px;font-size:13px;line-height:1.7;color:#1f2329;">
            {{range .Insights}}<li>{{.}}</li>{{end}}
          </ul>
        </td></tr>
        {{end}}

        <tr><td style="padding:8px 24px;">
          <h3 style="font-size:15px;margin:10px 0;border-left:3px solid #3370ff;padding-left:8px;">一、飞书反馈群 · 每日趋势</h3>
          {{if .DailyBars}}
          <table width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:12px;">
            {{range .DailyBars}}
            <tr>
              <td width="56" style="padding:4px 8px 4px 0;color:#646a73;">{{.Date}}</td>
              <td style="padding:4px 8px 4px 0;">
                <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#e8f1ff;height:10px;border-radius:999px;">
                  <tr><td width="{{.Width}}%" style="background:#3370ff;height:10px;border-radius:999px;font-size:0;line-height:0;">&nbsp;</td><td style="font-size:0;line-height:0;">&nbsp;</td></tr>
                </table>
              </td>
              <td width="40" align="right" style="padding:4px 0;font-family:Menlo,Consolas,monospace;">{{.Count}}</td>
            </tr>
            {{end}}
          </table>
          {{else}}<div style="font-size:13px;color:#646a73;">本周暂无反馈。</div>{{end}}
        </td></tr>

        <tr><td style="padding:8px 24px;">
          <h3 style="font-size:15px;margin:10px 0;border-left:3px solid #fa8c16;padding-left:8px;">本周 Top 问题 <span style="font-size:12px;color:#8f959e;font-weight:400;">（{{.TopIssueBy}}）</span></h3>
          {{if .TopIssues}}
          <table width="100%" cellpadding="6" cellspacing="0" border="0" style="font-size:13px;border-collapse:collapse;">
            <tr style="background:#f7f8fa;color:#646a73;"><th align="left">主题</th><th width="56">条数</th><th align="left">样例</th></tr>
            {{range .TopIssues}}<tr style="border-bottom:1px solid #eef0f3;vertical-align:top;"><td><b>{{.Category}}</b></td><td align="center">{{.Count}}</td><td style="color:#4e5969;">{{.Samples}}</td></tr>{{end}}
          </table>
          {{else}}<div style="font-size:13px;color:#646a73;">本周暂无可聚类的反馈。</div>{{end}}
        </td></tr>

        {{if .HasAI}}
        <tr><td style="padding:8px 24px;">
          <div style="background:#f5f3ff;border:1px solid #e9e2ff;border-radius:8px;padding:14px 16px;">
            <div style="font-size:15px;font-weight:700;color:#722ed1;">🤖 AI 洞察 <span style="font-size:12px;color:#9254de;font-weight:400;">（Cursor 语义聚类）</span></div>
            {{if .AITitle}}<div style="font-size:13px;color:#1f2329;margin:8px 0;">{{.AITitle}}</div>{{end}}
            {{if .AIIssues}}
            <div style="font-size:12px;color:#8f959e;margin-top:8px;">语义 Top 问题</div>
            <ul style="margin:4px 0;padding-left:20px;font-size:13px;line-height:1.7;">{{range .AIIssues}}<li>{{.}}</li>{{end}}</ul>
            {{end}}
            {{if .AIJudgement}}
            <div style="font-size:12px;color:#8f959e;margin-top:8px;">优先级判断</div>
            <ul style="margin:4px 0;padding-left:20px;font-size:13px;line-height:1.7;">{{range .AIJudgement}}<li>{{.}}</li>{{end}}</ul>
            {{end}}
            {{if .AINextSteps}}
            <div style="font-size:12px;color:#8f959e;margin-top:8px;">建议下一步</div>
            <ul style="margin:4px 0;padding-left:20px;font-size:13px;line-height:1.7;">{{range .AINextSteps}}<li>{{.}}</li>{{end}}</ul>
            {{end}}
          </div>
        </td></tr>
        {{end}}

        <tr><td style="padding:8px 24px 18px;">
          <h3 style="font-size:15px;margin:10px 0;border-left:3px solid #13c2c2;padding-left:8px;">二、App 内工单（work_order）</h3>
          {{if .HasWorkOrder}}
          <table width="100%" cellpadding="6" cellspacing="0" border="0" style="font-size:13px;border-collapse:collapse;">
            <tr style="background:#f7f8fa;color:#646a73;"><th align="left">指标</th><th align="left">本周</th></tr>
            <tr style="border-bottom:1px solid #eef0f3;"><td>新增反馈（环比上周）</td><td><b>{{.WorkOrder.Total}}</b>（上周 {{.WorkOrder.PrevTotal}}，{{.WorkOrder.DeltaLabel}}）</td></tr>
            <tr style="border-bottom:1px solid #eef0f3;"><td>当前待跟进</td><td>{{.WorkOrder.Backlog}}（未处理 {{.WorkOrder.Untouched}} + 处理中 {{.WorkOrder.Processing}}）</td></tr>
            <tr style="border-bottom:1px solid #eef0f3;"><td>当前未响应</td><td><b>{{.WorkOrder.CurrentUnreplied}}</b>（最长等待 {{.WorkOrder.MaxUnrepliedAge}}）</td></tr>
            <tr style="border-bottom:1px solid #eef0f3;"><td>已完结（用户/自动/客服）</td><td>{{.WorkOrder.Closed}}（{{.WorkOrder.ClosedByUser}} / {{.WorkOrder.ClosedByAuto}} / {{.WorkOrder.ClosedByAdmin}}）</td></tr>
            <tr style="border-bottom:1px solid #eef0f3;"><td>版本分布</td><td>{{.WorkOrder.TopVersions}}</td></tr>
            <tr style="border-bottom:1px solid #eef0f3;"><td>语种分布</td><td>{{.WorkOrder.TopLanguages}}</td></tr>
          </table>
          <div style="font-size:12px;color:#8f959e;margin-top:8px;">说明：当前未响应仅统计当前仍处于未处理/处理中，且尚无客服回复的工单；已完结工单不计入。时长按自然时间计算，遇周末/非工作时间可能偏长。</div>
          {{else}}<div style="font-size:13px;color:#646a73;">工单数据下一步接入（matrix-backend 只读聚合接口）。</div>{{end}}
        </td></tr>

        <tr><td style="padding:14px 24px;background:#fafbfc;color:#8f959e;font-size:12px;border-top:1px solid #eef0f3;">生成时间：{{.GeneratedAt}}<br/>报告由 matrix-alert-forwarder 自动生成。</td></tr>
      </table>
    </td></tr>
  </table>
</body></html>`
