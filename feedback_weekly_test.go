package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFeedbackWeeklyWindow(t *testing.T) {
	loc := feedbackWeeklyLoc()
	// 2026-06-30 是周二，最近一整周应为 [2026-06-22(周一), 2026-06-29(周一))。
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, loc)
	start, end := feedbackWeeklyWindow(now, loc)
	if got := start.In(loc).Format("2006-01-02"); got != "2026-06-22" {
		t.Fatalf("start = %s, want 2026-06-22", got)
	}
	if got := end.In(loc).Format("2006-01-02"); got != "2026-06-29" {
		t.Fatalf("end = %s, want 2026-06-29", got)
	}
}

func TestFeedbackWeeklyNextTrigger(t *testing.T) {
	loc := feedbackWeeklyLoc()
	// 周二 10:00 -> 下一个周一 10:00。
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, loc)
	next := feedbackWeeklyNextTrigger(now, loc, 10)
	if next.Weekday() != time.Monday {
		t.Fatalf("next weekday = %v, want Monday", next.Weekday())
	}
	if !next.After(now) {
		t.Fatalf("next %v must be after now %v", next, now)
	}
	if got := next.In(loc).Format("2006-01-02 15"); got != "2026-07-06 10" {
		t.Fatalf("next = %s, want 2026-07-06 10", got)
	}
	// 恰好周一 09:00 -> 当天 10:00。
	mon9 := time.Date(2026, 7, 6, 9, 0, 0, 0, loc)
	n2 := feedbackWeeklyNextTrigger(mon9, loc, 10)
	if got := n2.In(loc).Format("2006-01-02 15"); got != "2026-07-06 10" {
		t.Fatalf("mon9 next = %s, want 2026-07-06 10", got)
	}
}

func TestClusterFeedbackIssues(t *testing.T) {
	texts := []string{
		"用户1,反馈问题：GPA订单支付了但没到账",
		"用户2,反馈问题：充值后金币未到账",
		"用户3,反馈问题：做tiktok和ins任务没有收到积分",
		"用户4,反馈问题：邀请好友没有金币奖励",
		"用户5,反馈问题：观看广告没有奖励",
		"用户6,反馈问题：无法广告解锁剧集",
		"用户7,反馈问题：说不清楚的奇怪问题",
	}
	issues := clusterFeedbackIssues(texts)
	if len(issues) == 0 {
		t.Fatal("expected some clustered issues")
	}
	// 降序：第一类条数应 >= 后续。
	for i := 1; i < len(issues); i++ {
		if issues[i].Count > issues[i-1].Count {
			t.Fatalf("issues not sorted desc: %+v", issues)
		}
	}
	// 应包含支付类（GPA/充值到账各一条）。
	var payCount int
	for _, it := range issues {
		if it.Category == "支付/充值未到账" {
			payCount = it.Count
		}
	}
	if payCount < 2 {
		t.Fatalf("pay category count = %d, want >=2; issues=%+v", payCount, issues)
	}
}

func TestBuildFeedbackWeeklyStats(t *testing.T) {
	loc := feedbackWeeklyLoc()
	start := time.Date(2026, 6, 22, 0, 0, 0, 0, loc)
	end := time.Date(2026, 6, 29, 0, 0, 0, 0, loc)
	msgs := []feishuChatMessage{
		{SenderType: "user", Text: "用户1,反馈问题：支付没到账", CreateTime: time.Date(2026, 6, 22, 9, 0, 0, 0, loc)},
		{SenderType: "user", Text: "用户2,反馈问题：广告没奖励", CreateTime: time.Date(2026, 6, 22, 10, 0, 0, 0, loc)},
		{SenderType: "user", Text: "用户3,反馈问题：任务没积分", CreateTime: time.Date(2026, 6, 23, 11, 0, 0, 0, loc)},
		{SenderType: "user", Text: "客服回复一下这个怎么处理", CreateTime: time.Date(2026, 6, 23, 12, 0, 0, 0, loc)},      // 人工跟进
		{SenderType: "app", Text: "⏳ Still working...", CreateTime: time.Date(2026, 6, 23, 12, 5, 0, 0, loc)}, // 机器人，忽略
	}
	stats := buildFeedbackWeeklyStats("oc_x", start, end, msgs, loc)
	if stats.FeedbackCount != 3 {
		t.Fatalf("FeedbackCount = %d, want 3", stats.FeedbackCount)
	}
	if stats.FollowupCount != 1 {
		t.Fatalf("FollowupCount = %d, want 1", stats.FollowupCount)
	}
	if stats.TotalMessages != 5 {
		t.Fatalf("TotalMessages = %d, want 5", stats.TotalMessages)
	}
	if len(stats.DailyCounts) != 2 {
		t.Fatalf("DailyCounts len = %d, want 2 (%+v)", len(stats.DailyCounts), stats.DailyCounts)
	}
	if stats.DailyCounts[0].Date != "06-22" || stats.DailyCounts[0].Count != 2 {
		t.Fatalf("day0 = %+v, want 06-22/2", stats.DailyCounts[0])
	}
}

func TestFeedbackWeeklyPostChatEnabled(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"1", true},
		{"true", true},
		{"yes", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("FEEDBACK_WEEKLY_POST_CHAT", tc.value)
			if got := feedbackWeeklyPostChatEnabled(); got != tc.want {
				t.Fatalf("feedbackWeeklyPostChatEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSendFeedbackWeeklyChatReportPostsRenderedText(t *testing.T) {
	t.Setenv("FEEDBACK_WEEKLY_CHAT_ID", "oc_feedback")

	var sent map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			if got := r.URL.Query().Get("receive_id_type"); got != "chat_id" {
				t.Fatalf("receive_id_type=%q, want chat_id", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Fatalf("authorization=%q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode sent body: %v", err)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_weekly"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	loc := feedbackWeeklyLoc()
	stats := feedbackWeeklyStats{
		ChatID:        "oc_feedback",
		WindowStart:   time.Date(2026, 6, 22, 0, 0, 0, 0, loc),
		WindowEnd:     time.Date(2026, 6, 29, 0, 0, 0, 0, loc),
		GeneratedAt:   time.Date(2026, 6, 30, 14, 0, 0, 0, loc),
		TotalMessages: 2,
		FeedbackCount: 1,
		FollowupCount: 1,
		DailyCounts:   []feedbackDailyCount{{Date: "06-22", Count: 1}},
		TopIssues:     []feedbackTopIssue{{Category: "支付/充值未到账", Count: 1, Samples: []string{"充值没到账"}}},
		TopIssueBy:    "rule",
		WorkOrder: &workOrderWeeklyStats{
			Total:            3,
			PrevTotal:        2,
			Processing:       1,
			Closed:           2,
			CurrentUnreplied: 0,
		},
	}
	s := &server{
		feishuAPIBase:   ts.URL,
		feishuAppID:     "cli",
		feishuAppSecret: "secret",
		feishuChatID:    "oc_default",
		client:          ts.Client(),
	}
	if err := s.sendFeedbackWeeklyChatReport(context.Background(), stats, loc); err != nil {
		t.Fatalf("sendFeedbackWeeklyChatReport: %v", err)
	}
	if sent["receive_id"] != "oc_feedback" || sent["msg_type"] != "text" {
		t.Fatalf("unexpected sent body: %+v", sent)
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(sent["content"]), &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	for _, want := range []string{"用户反馈周报 06-22 ~ 06-28", "本周反馈：1 条", "App 内工单"} {
		if !strings.Contains(content["text"], want) {
			t.Fatalf("posted text missing %q: %s", want, content["text"])
		}
	}
}
