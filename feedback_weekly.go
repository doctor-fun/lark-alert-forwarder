package main

// feedback_weekly.go 实现「用户反馈周报」：
//
//   - 数据主源：飞书用户反馈群（oc_4b42...）。客服把 App 用户反馈逐条转发进群、
//     @ 客服小助手机器人。这些消息本身不落库，只能通过飞书 im/v1/messages
//     按时间窗拉取历史来统计。本服务有飞书 bot 且在群里，是唯一能取到群数据的地方。
//   - 数据补充源：matrix-backend 的 work_order 工单聚合（App 内意见反馈入口，
//     与飞书群是两条独立渠道）。本期先留接口位（WorkOrder 字段），由后续接入。
//   - AI 增强：反馈正文 Top 问题聚类，本期先用关键词规则聚类兜底，后续接 agent-runner
//     的 cursor agent 做语义聚类。
//
// 周报每周一 10:00（Asia/Shanghai）发上一整周（上周一~周日），也可通过
// /internal/feedback_weekly/report 端点手动触发 / dry-run 自测。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// feedbackWeeklyMaxPages 限制单次拉群分页数，防止异常情况下无限翻页。
	// 每页 50 条，50 页 = 2500 条，足够覆盖一周（当前约 60~200 条/周）。
	feedbackWeeklyMaxPages = 50
	feedbackWeeklyPageSize = 50
	// feedbackWeeklyFeedbackMarker 是客服转发反馈的固定文案标记。
	feedbackWeeklyFeedbackMarker = "反馈问题"
)

// feishuChatMessage 是从飞书群拉到的一条消息的精简视图。
type feishuChatMessage struct {
	MessageID  string
	ThreadID   string
	CreateTime time.Time
	SenderType string // user / app / system
	SenderID   string
	MsgType    string
	Text       string
}

// feedbackTopIssue 是一个反馈主题聚类结果。
type feedbackTopIssue struct {
	Category string   `json:"category"`
	Count    int      `json:"count"`
	Samples  []string `json:"samples"`
}

// feedbackDailyCount 是某一天的反馈条数。
type feedbackDailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// workOrderWeeklyStats 是 App 内工单（work_order）周聚合，本期留位，
// 由后续接 matrix-backend 只读聚合接口填充；nil 表示尚未接入。
type workOrderWeeklyStats struct {
	Total            int                  `json:"total"`
	PrevTotal        int                  `json:"prev_total"`
	Untouched        int                  `json:"untouched"`
	Processing       int                  `json:"processing"`
	Closed           int                  `json:"closed"`
	ClosedByUser     int                  `json:"closed_by_user"`
	ClosedByAuto     int                  `json:"closed_by_auto"`
	ClosedByAdmin    int                  `json:"closed_by_admin"`
	Replied          int                  `json:"replied"`
	RepliedWithin24h int                  `json:"replied_within_24h"`
	CurrentUnreplied int                  `json:"current_unreplied"`
	AvgFirstReplyM   float64              `json:"avg_first_reply_minutes"`
	MaxFirstReplyM   float64              `json:"max_first_reply_minutes"`
	MaxUnrepliedAgeM float64              `json:"max_unreplied_age_minutes"`
	TopVersions      []feedbackLabelCount `json:"top_versions"`
	TopLanguages     []feedbackLabelCount `json:"top_languages"`
}

type feedbackLabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// feedbackWeeklyStats 是整份周报的统计数据。
type feedbackWeeklyStats struct {
	ChatID        string                `json:"chat_id"`
	WindowStart   time.Time             `json:"window_start"`
	WindowEnd     time.Time             `json:"window_end"`
	GeneratedAt   time.Time             `json:"generated_at"`
	TotalMessages int                   `json:"total_messages"`
	FeedbackCount int                   `json:"feedback_count"`
	FollowupCount int                   `json:"followup_count"`
	DailyCounts   []feedbackDailyCount  `json:"daily_counts"`
	TopIssues     []feedbackTopIssue    `json:"top_issues"`
	TopIssueBy    string                `json:"top_issue_by"` // "rule"（表格固定为规则精确计数）
	WorkOrder     *workOrderWeeklyStats `json:"work_order,omitempty"`
	AIInsight     *feedbackAIInsight    `json:"ai_insight,omitempty"`

	// sampleFeedbacks 是本周反馈正文（供 cursor 语义聚类），不序列化进周报 JSON。
	sampleFeedbacks []string `json:"-"`
}

// --- 时间窗口与时区 ---------------------------------------------------------

func feedbackWeeklyLoc() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

// feedbackWeeklyWindow 返回「最近一整周」[上周一 00:00, 本周一 00:00)。
func feedbackWeeklyWindow(now time.Time, loc *time.Location) (start, end time.Time) {
	n := now.In(loc)
	weekday := int(n.Weekday()) // Sunday=0 ... Saturday=6
	if weekday == 0 {
		weekday = 7
	}
	thisMonday := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(weekday - 1))
	return thisMonday.AddDate(0, 0, -7), thisMonday
}

// feedbackWeeklyNextTrigger 返回下一个「周一 hour:00」触发时刻（严格晚于 now）。
func feedbackWeeklyNextTrigger(now time.Time, loc *time.Location, hour int) time.Time {
	n := now.In(loc)
	c := time.Date(n.Year(), n.Month(), n.Day(), hour, 0, 0, 0, loc)
	for i := 0; i < 8; i++ {
		if c.Weekday() == time.Monday && c.After(n) {
			return c
		}
		c = c.AddDate(0, 0, 1)
		c = time.Date(c.Year(), c.Month(), c.Day(), hour, 0, 0, 0, loc)
	}
	return n.Add(7 * 24 * time.Hour)
}

// --- 飞书群消息拉取 ---------------------------------------------------------

type feishuMessageListResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		HasMore   bool   `json:"has_more"`
		PageToken string `json:"page_token"`
		Items     []struct {
			MessageID  string `json:"message_id"`
			ThreadID   string `json:"thread_id"`
			CreateTime string `json:"create_time"`
			MsgType    string `json:"msg_type"`
			Body       struct {
				Content string `json:"content"`
			} `json:"body"`
			Sender struct {
				ID         string `json:"id"`
				SenderType string `json:"sender_type"`
			} `json:"sender"`
		} `json:"items"`
	} `json:"data"`
}

// fetchChatMessages 按时间窗分页拉取群历史消息（升序）。
func (s *server) fetchChatMessages(ctx context.Context, chatID string, start, end time.Time) ([]feishuChatMessage, error) {
	if s == nil || s.feishuAppID == "" || s.feishuAppSecret == "" {
		return nil, fmt.Errorf("feishu app not configured")
	}
	token, err := s.tenantAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get tenant token: %w", err)
	}
	var out []feishuChatMessage
	pageToken := ""
	for page := 0; page < feedbackWeeklyMaxPages; page++ {
		path := fmt.Sprintf(
			"/open-apis/im/v1/messages?container_id_type=chat&container_id=%s&sort_type=ByCreateTimeAsc&page_size=%d&start_time=%d&end_time=%d",
			chatID, feedbackWeeklyPageSize, start.Unix(), end.Unix(),
		)
		if pageToken != "" {
			path += "&page_token=" + pageToken
		}
		var resp feishuMessageListResponse
		if err := s.callFeishuAPI(ctx, http.MethodGet, path, token, nil, &resp); err != nil {
			return nil, fmt.Errorf("list messages page %d: %w", page, err)
		}
		if resp.Code != 0 {
			return nil, fmt.Errorf("list messages code=%d msg=%s", resp.Code, resp.Msg)
		}
		for _, it := range resp.Data.Items {
			ms, _ := strconv.ParseInt(it.CreateTime, 10, 64)
			out = append(out, feishuChatMessage{
				MessageID:  it.MessageID,
				ThreadID:   it.ThreadID,
				CreateTime: time.UnixMilli(ms),
				SenderType: it.Sender.SenderType,
				SenderID:   it.Sender.ID,
				MsgType:    it.MsgType,
				Text:       feishuMessageText(it.Body.Content),
			})
		}
		if !resp.Data.HasMore || resp.Data.PageToken == "" {
			break
		}
		pageToken = resp.Data.PageToken
	}
	return out, nil
}

// --- 统计与聚类 -------------------------------------------------------------

func messageIsFeedback(m feishuChatMessage) bool {
	return m.SenderType == "user" && strings.Contains(m.Text, feedbackWeeklyFeedbackMarker)
}

// buildFeedbackWeeklyStats 把原始消息聚合成周报统计。
func buildFeedbackWeeklyStats(chatID string, start, end time.Time, msgs []feishuChatMessage, loc *time.Location) *feedbackWeeklyStats {
	stats := &feedbackWeeklyStats{
		ChatID:      chatID,
		WindowStart: start,
		WindowEnd:   end,
		GeneratedAt: time.Now(),
		TopIssueBy:  "rule",
	}
	dayIndex := map[string]int{}
	var feedbackTexts []string
	for _, m := range msgs {
		stats.TotalMessages++
		switch {
		case messageIsFeedback(m):
			stats.FeedbackCount++
			feedbackTexts = append(feedbackTexts, m.Text)
			day := m.CreateTime.In(loc).Format("01-02")
			if _, ok := dayIndex[day]; !ok {
				dayIndex[day] = len(stats.DailyCounts)
				stats.DailyCounts = append(stats.DailyCounts, feedbackDailyCount{Date: day})
			}
			stats.DailyCounts[dayIndex[day]].Count++
		case m.SenderType == "user":
			// 群里非反馈的真人消息，视为人工跟进 / 讨论。
			stats.FollowupCount++
		}
	}
	sort.Slice(stats.DailyCounts, func(i, j int) bool {
		return stats.DailyCounts[i].Date < stats.DailyCounts[j].Date
	})
	stats.TopIssues = clusterFeedbackIssues(feedbackTexts)
	stats.sampleFeedbacks = feedbackTexts
	return stats
}

// feedbackIssueRule 是一个关键词规则分组。
type feedbackIssueRule struct {
	Category string
	Keywords []string
}

// feedbackIssueRules 的顺序即优先级：一条反馈归入第一个命中的分组。
var feedbackIssueRules = []feedbackIssueRule{
	{"支付/充值未到账", []string{"支付", "付款", "充值", "到账", "扣款", "退款", "gpa", "payment", "pay", "购买", "vip", "会员", "订阅", "recharge", "google pay"}},
	{"金币/积分/奖励", []string{"金币", "积分", "奖励", "coin", "reward", "没有收到", "未收到"}},
	{"任务/邀请/绑定", []string{"任务", "task", "tiktok", "ins", "instagram", "facebook", "关注", "绑定", "邀请", "invite", "好友", "friend"}},
	{"广告问题", []string{"广告", "ad ", "ads", "观看广告", "看广告"}},
	{"解锁/播放/剧集", []string{"解锁", "观看", "播放", "剧集", "视频", "episode", "unlock", "看不了", "无法观看", "黑屏", "播放不了"}},
	{"登录/账号", []string{"登录", "登陆", "账号", "账户", "login", "account", "注册", "验证码", "封号", "找回"}},
	{"闪退/卡顿/性能", []string{"闪退", "崩溃", "卡顿", "crash", "卡死", "白屏", "打不开", "加载"}},
}

// clusterFeedbackIssues 用关键词规则把反馈聚类成 Top 主题（降序，最多 6 类）。
func clusterFeedbackIssues(texts []string) []feedbackTopIssue {
	buckets := map[string]*feedbackTopIssue{}
	order := []string{}
	add := func(cat, sample string) {
		b, ok := buckets[cat]
		if !ok {
			b = &feedbackTopIssue{Category: cat}
			buckets[cat] = b
			order = append(order, cat)
		}
		b.Count++
		if len(b.Samples) < 2 {
			b.Samples = append(b.Samples, feedbackSampleText(sample))
		}
	}
	for _, t := range texts {
		lower := strings.ToLower(t)
		matched := false
		for _, rule := range feedbackIssueRules {
			for _, kw := range rule.Keywords {
				if strings.Contains(lower, kw) {
					add(rule.Category, t)
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			add("其他", t)
		}
	}
	out := make([]feedbackTopIssue, 0, len(order))
	for _, cat := range order {
		out = append(out, *buckets[cat])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// feedbackSampleText 从一条反馈里抽出「反馈问题：」后面的正文做样例展示，并截断。
func feedbackSampleText(t string) string {
	if idx := strings.Index(t, feedbackWeeklyFeedbackMarker); idx >= 0 {
		rest := t[idx+len(feedbackWeeklyFeedbackMarker):]
		rest = strings.TrimLeft(rest, "：: ")
		t = rest
	}
	t = strings.TrimSpace(strings.ReplaceAll(t, "\n", " "))
	return truncate(t, 50)
}

// --- 生成 + 发送 ------------------------------------------------------------

// runFeedbackWeeklyReport 拉数据、聚合、渲染并发送周报。dryRun=true 时只生成不发送。
func (s *server) runFeedbackWeeklyReport(ctx context.Context, to []string, dryRun bool) (*feedbackWeeklyStats, error) {
	loc := feedbackWeeklyLoc()
	start, end := feedbackWeeklyWindow(time.Now(), loc)
	chatID := feedbackWeeklyChatID()

	msgs, err := s.fetchChatMessages(ctx, chatID, start, end)
	if err != nil {
		return nil, err
	}
	stats := buildFeedbackWeeklyStats(chatID, start, end, msgs, loc)

	// 工单补充：App 内意见反馈（work_order），与飞书群独立的第二条渠道。
	// 拉不到时降级（周报仍以飞书群数据为主），不阻断发送。
	if s.backend != nil {
		if wo, err := s.backend.GetWorkOrderWeeklyStats(ctx, start, end); err != nil {
			log.Printf("feedback weekly report: work_order stats unavailable: %v", err)
		} else {
			stats.WorkOrder = wo
		}
	}

	// cursor 语义聚类：调 agent-runner 的 feedback-digest agent 出「AI 洞察」板块
	// （比关键词规则更准的 Top 问题 + 优先级判断 + 建议）。失败时降级——规则聚类
	// 表格仍在，周报照常发，不阻断。
	if insight, err := s.clusterFeedbackWithCursor(ctx, stats); err != nil {
		log.Printf("feedback weekly report: cursor digest unavailable (fallback to rule clustering): %v", err)
	} else {
		stats.AIInsight = insight
	}

	if dryRun {
		return stats, nil
	}
	if s.mailer == nil {
		return stats, fmt.Errorf("smtp mailer not configured")
	}
	if len(to) == 0 {
		return stats, fmt.Errorf("no recipient configured")
	}
	msg, err := renderFeedbackWeeklyEmail(*stats, to, loc)
	if err != nil {
		return stats, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.mailer.Send(sendCtx, msg); err != nil {
		return stats, fmt.Errorf("smtp send: %w", err)
	}
	log.Printf("feedback weekly report sent window=%s~%s feedback=%d to=%s",
		start.In(loc).Format("2006-01-02"), end.In(loc).AddDate(0, 0, -1).Format("2006-01-02"),
		stats.FeedbackCount, strings.Join(to, ","))
	if feedbackWeeklyPostChatEnabled() {
		chatCtx, chatCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := s.sendFeedbackWeeklyChatReport(chatCtx, *stats, loc); err != nil {
			log.Printf("feedback weekly report: post chat failed: %v", err)
		}
		chatCancel()
	}
	return stats, nil
}

// --- 配置 -------------------------------------------------------------------

func feedbackWeeklyChatID() string {
	return envOrDefault("FEEDBACK_WEEKLY_CHAT_ID", defaultUserFeedbackOncallChatID)
}

func (s *server) feedbackWeeklyRecipients() []string {
	if to := splitEmailList(os.Getenv("FEEDBACK_WEEKLY_EMAIL_TO")); len(to) > 0 {
		return to
	}
	return s.fallbackTo
}

func feedbackWeeklyPostChatEnabled() bool {
	v := strings.TrimSpace(os.Getenv("FEEDBACK_WEEKLY_POST_CHAT"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func feedbackWeeklyHour() int {
	if v := strings.TrimSpace(os.Getenv("FEEDBACK_WEEKLY_HOUR")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 23 {
			return n
		}
	}
	return 10
}

func (s *server) sendFeedbackWeeklyChatReport(ctx context.Context, stats feedbackWeeklyStats, loc *time.Location) error {
	if s == nil {
		return fmt.Errorf("server is nil")
	}
	chatID := feedbackWeeklyChatID()
	if strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("FEEDBACK_WEEKLY_CHAT_ID is empty")
	}
	windowLabel := stats.WindowStart.In(loc).Format("01-02") + " ~ " +
		stats.WindowEnd.In(loc).AddDate(0, 0, -1).Format("01-02")
	text := renderFeedbackWeeklyText(stats, windowLabel, loc)
	token, err := s.tenantAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get tenant token: %w", err)
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	body := map[string]string{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    string(content),
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := s.callFeishuAPI(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type=chat_id", token, body, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("send chat text failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	log.Printf("feedback weekly report posted chat=%s message_id=%s", chatID, resp.Data.MessageID)
	return nil
}

// --- HTTP 手动触发端点 ------------------------------------------------------

// handleFeedbackWeeklyReport 手动触发周报。需 X-Forwarder-Token。
//
//	POST /internal/feedback_weekly/report           生成并发送（收件人取 env/fallback）
//	POST /internal/feedback_weekly/report?dry=1      只生成统计、返回 JSON、不发送（自测）
//	POST /internal/feedback_weekly/report?to=a@x,b@y  指定收件人
func (s *server) handleFeedbackWeeklyReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.validForwarderToken(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	dryRun := r.URL.Query().Get("dry") == "1"
	to := splitEmailList(r.URL.Query().Get("to"))
	if len(to) == 0 {
		to = s.feedbackWeeklyRecipients()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	stats, err := s.runFeedbackWeeklyReport(ctx, to, dryRun)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error(), "stats": stats})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "dry_run": dryRun, "stats": stats})
}

// --- 周报定时调度 -----------------------------------------------------------

// startFeedbackWeeklyReportCron 每周一 hour:00（Asia/Shanghai）发上一整周周报。
// 未配置 mailer 或收件人时不启动。返回的 cancel 用于优雅停止。
func (s *server) startFeedbackWeeklyReportCron() func() {
	if s == nil || s.mailer == nil || s.feishuAppID == "" {
		log.Printf("feedback weekly report cron: disabled (mailer or feishu app missing)")
		return func() {}
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FEEDBACK_WEEKLY_DISABLE")), "1") {
		log.Printf("feedback weekly report cron: disabled by FEEDBACK_WEEKLY_DISABLE=1")
		return func() {}
	}
	loc := feedbackWeeklyLoc()
	hour := feedbackWeeklyHour()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			next := feedbackWeeklyNextTrigger(time.Now(), loc, hour)
			log.Printf("feedback weekly report cron: next run at %s", next.Format("2006-01-02 15:04 MST"))
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				to := s.feedbackWeeklyRecipients()
				if len(to) == 0 {
					log.Printf("feedback weekly report cron: skip, no recipient")
					continue
				}
				runCtx, c := context.WithTimeout(context.Background(), 2*time.Minute)
				if _, err := s.runFeedbackWeeklyReport(runCtx, to, false); err != nil {
					log.Printf("feedback weekly report cron: run failed: %v", err)
				}
				c()
			}
		}
	}()
	return cancel
}
