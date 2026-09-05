package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	testCaseReminderAction         = "test_case_reminder_confirm"
	testCaseReminderSuppressAction = "test_case_reminder_suppress"
)

type testCaseReminderOwner struct {
	Name   string
	OpenID string
}

type testCaseReminderConfig struct {
	ChatID         string
	RepeatInterval time.Duration
	TableURL       string
	Owners         []testCaseReminderOwner
}

type testCaseReminder struct {
	cfg testCaseReminderConfig

	mu            sync.Mutex
	week          string
	active        bool
	confirmed     map[string]time.Time
	suppressed    map[string]time.Time
	allSuppressed bool
}

func parseTestCaseReminderOwners(raw string) []testCaseReminderOwner {
	var owners []testCaseReminderOwner
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, openID, ok := strings.Cut(part, "|")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		openID = strings.TrimSpace(openID)
		if name == "" || openID == "" {
			continue
		}
		owners = append(owners, testCaseReminderOwner{Name: name, OpenID: openID})
	}
	return owners
}

func testCaseReminderFromEnv() *testCaseReminder {
	chatID := strings.TrimSpace(os.Getenv("TEST_CASE_REMINDER_CHAT_ID"))
	if chatID == "" {
		return nil
	}
	repeat := durationFromEnv("TEST_CASE_REMINDER_REPEAT_INTERVAL", 2*time.Hour)
	if repeat <= 0 {
		repeat = 2 * time.Hour
	}
	return &testCaseReminder{
		cfg: testCaseReminderConfig{
			ChatID:         chatID,
			RepeatInterval: repeat,
			TableURL:       strings.TrimSpace(os.Getenv("TEST_CASE_REMINDER_TABLE_URL")),
			Owners:         parseTestCaseReminderOwners(os.Getenv("TEST_CASE_REMINDER_OWNERS")),
		},
		confirmed:  map[string]time.Time{},
		suppressed: map[string]time.Time{},
	}
}

func (r *testCaseReminder) currentWeek(now time.Time) string {
	year, week := now.In(chinaLocation()).ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func (r *testCaseReminder) begin(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	week := r.currentWeek(now)
	if r.week == week && r.active {
		return
	}
	r.week = week
	r.active = true
	r.confirmed = map[string]time.Time{}
	r.suppressed = map[string]time.Time{}
	r.allSuppressed = false
}

func (r *testCaseReminder) confirm(now time.Time, week, openID string) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || week != r.week {
		return false, "本周提醒已失效，请等待下一次提醒"
	}
	if !r.isOwner(openID) {
		return false, "仅Frank或Grace可以确认提交"
	}
	r.confirmed[openID] = now
	if r.allConfirmedLocked() {
		r.active = false
	}
	return true, ""
}

func (r *testCaseReminder) suppress(now time.Time, week, openID string, all bool) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || week != r.week {
		return false, "本周提醒已失效，请等待下一次提醒"
	}
	if !r.isOwner(openID) {
		return false, "仅Frank或Grace可以屏蔽提醒"
	}
	if all {
		r.allSuppressed = true
		r.active = false
		return true, ""
	}
	r.suppressed[openID] = now
	if r.allConfirmedLocked() {
		r.active = false
	}
	return true, ""
}

func (r *testCaseReminder) isOwner(openID string) bool {
	for _, owner := range r.cfg.Owners {
		if owner.OpenID == openID {
			return true
		}
	}
	return false
}

func (r *testCaseReminder) allConfirmedLocked() bool {
	for _, owner := range r.cfg.Owners {
		if _, confirmed := r.confirmed[owner.OpenID]; !confirmed {
			if _, suppressed := r.suppressed[owner.OpenID]; !suppressed {
				return false
			}
		}
		if r.allSuppressed {
			return true
		}
	}
	return true
}

func (r *testCaseReminder) snapshot() (week string, active bool, confirmed, suppressed map[string]time.Time, allSuppressed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	confirmed = make(map[string]time.Time, len(r.confirmed))
	for openID, at := range r.confirmed {
		confirmed[openID] = at
	}
	suppressed = make(map[string]time.Time, len(r.suppressed))
	for openID, at := range r.suppressed {
		suppressed[openID] = at
	}
	return r.week, r.active, confirmed, suppressed, r.allSuppressed
}

func (s *server) startTestCaseReminder() func() {
	if s == nil || s.testCaseReminder == nil || !s.feishuAppConfigured() {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			next := nextFridayAt1400(time.Now())
			log.Printf("test-case reminder: next weekly trigger at %s", next.Format("2006-01-02 15:04 MST"))
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case now := <-timer.C:
				s.testCaseReminder.begin(now)
				s.sendTestCaseReminder(now)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(s.testCaseReminder.cfg.RepeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_, active, _, _, _ := s.testCaseReminder.snapshot()
				if active {
					s.sendTestCaseReminder(now)
				}
			}
		}
	}()
	return cancel
}

func nextFridayAt1400(now time.Time) time.Time {
	loc := chinaLocation()
	current := now.In(loc)
	candidate := time.Date(current.Year(), current.Month(), current.Day(), 14, 0, 0, 0, loc)
	days := (int(time.Friday) - int(current.Weekday()) + 7) % 7
	candidate = candidate.AddDate(0, 0, days)
	if !candidate.After(current) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate
}

func (s *server) sendTestCaseReminder(now time.Time) {
	if s == nil || s.testCaseReminder == nil {
		return
	}
	week, active, confirmed, suppressed, allSuppressed := s.testCaseReminder.snapshot()
	if !active {
		return
	}
	card := buildTestCaseReminderCard(s.testCaseReminder.cfg, week, confirmed, suppressed, allSuppressed)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.sendFeishuAppCardTo(ctx, s.testCaseReminder.cfg.ChatID, "chat_id", feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
		log.Printf("test-case reminder: send failed: %v", err)
		return
	}
	log.Printf("test-case reminder: sent week=%s chat=%s", week, s.testCaseReminder.cfg.ChatID)
}

// handleTestCaseReminderSend 提供受 forwarder token 保护的人工补发入口。
// 周五 14:00 前需要提前发卡确认时使用；begin 对同一周幂等，不会清空已确认人。
func (s *server) handleTestCaseReminderSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && !s.validForwarderToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.testCaseReminder == nil || !s.feishuAppConfigured() {
		http.Error(w, "test case reminder is not configured", http.StatusPreconditionFailed)
		return
	}
	now := time.Now()
	s.testCaseReminder.begin(now)
	s.sendTestCaseReminder(now)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"sent"}`))
}

// handleTestCaseReminderMessageEvent lets the configured group actively open
// the current week's reminder card by sending “用例” or “/用例”.
func (s *server) handleTestCaseReminderMessageEvent(_ context.Context, raw json.RawMessage) error {
	if s == nil || s.testCaseReminder == nil || !s.feishuAppConfigured() {
		return nil
	}
	var event feishuMessageReceiveEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if event.Message.MessageType != "" && event.Message.MessageType != "text" {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(event.Message.ChatID), strings.TrimSpace(s.testCaseReminder.cfg.ChatID)) {
		return nil
	}
	text := normalizeDirtyWorkLaunchCommand(feishuMessageText(event.Message.Content))
	if text != "用例" && text != "/用例" {
		return nil
	}
	now := time.Now()
	s.testCaseReminder.begin(now)
	s.sendTestCaseReminder(now)
	log.Printf("test-case reminder opened by message command chat=%s operator=%s",
		event.Message.ChatID, event.Sender.SenderID.OpenID)
	return nil
}

func (s *server) handleTestCaseReminderAction(_ context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	if s == nil || s.testCaseReminder == nil {
		return feishuCardCallbackResponse{Toast: &feishuCardToast{Type: "warning", Content: "测试用例提醒未启用"}}, nil
	}
	week := strings.TrimSpace(event.Action.Value["week"])
	now := time.Now()
	openID := strings.TrimSpace(event.Operator.OpenID)
	action := event.Action.Value["action"]
	all := event.Action.Value["scope"] == "all"
	var ok bool
	var reason string
	if action == testCaseReminderSuppressAction {
		ok, reason = s.testCaseReminder.suppress(now, week, openID, all)
	} else {
		ok, reason = s.testCaseReminder.confirm(now, week, openID)
	}
	if !ok {
		return feishuCardCallbackResponse{Toast: &feishuCardToast{Type: "warning", Content: reason}}, nil
	}
	currentWeek, active, confirmed, suppressed, allSuppressed := s.testCaseReminder.snapshot()
	log.Printf("test-case reminder action=%s all=%t operator=%s week=%s active=%t",
		action, all, openID, currentWeek, active)
	card := buildTestCaseReminderCard(s.testCaseReminder.cfg, currentWeek, confirmed, suppressed, allSuppressed)
	message := "已确认提交测试用例"
	if action == testCaseReminderSuppressAction {
		message = "已屏蔽本周提醒"
		if all {
			message = "已屏蔽本周全员提醒"
		}
	}
	if !active {
		message = "两位均已确认，本周不再提醒"
	}
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: message},
		Card:  rawFeishuCallbackCard(card),
	}, nil
}

func buildTestCaseReminderCard(cfg testCaseReminderConfig, week string, confirmed, suppressed map[string]time.Time, allSuppressed bool) feishuCard {
	owners := make([]string, 0, len(cfg.Owners))
	statuses := make([]string, 0, len(cfg.Owners))
	for _, owner := range cfg.Owners {
		owners = append(owners, fmt.Sprintf("<at id=\"%s\"></at>", owner.OpenID))
		if at, ok := confirmed[owner.OpenID]; ok {
			statuses = append(statuses, fmt.Sprintf("%s：已确认（%s）", owner.Name, at.In(chinaLocation()).Format("15:04")))
		} else if _, ok := suppressed[owner.OpenID]; ok {
			statuses = append(statuses, owner.Name+"：本周已屏蔽")
		} else {
			statuses = append(statuses, owner.Name+"：待确认")
		}
	}
	if allSuppressed {
		statuses = []string{"本周全员提醒已屏蔽"}
	}
	actions := []map[string]any{
		{
			"tag": "button",
			"text": map[string]string{
				"tag":     "plain_text",
				"content": "我已提交测试用例",
			},
			"type": "primary",
			"value": map[string]string{
				"action": testCaseReminderAction,
				"week":   week,
			},
		},
		{
			"tag": "button",
			"text": map[string]string{
				"tag":     "plain_text",
				"content": "我本周无需提交",
			},
			"type": "default",
			"value": map[string]string{
				"action": testCaseReminderSuppressAction,
				"week":   week,
			},
		},
		{
			"tag": "button",
			"text": map[string]string{
				"tag":     "plain_text",
				"content": "本周全员无需提交",
			},
			"type": "danger",
			"value": map[string]string{
				"action": testCaseReminderSuppressAction,
				"week":   week,
				"scope":  "all",
			},
		},
	}
	if cfg.TableURL != "" {
		actions = append(actions, map[string]any{
			"tag": "button",
			"text": map[string]string{
				"tag":     "plain_text",
				"content": "提交/上传测试用例",
			},
			"type": "default",
			"url":  cfg.TableURL,
		})
	}
	tableLink := ""
	if cfg.TableURL != "" {
		tableLink = "\n\n[打开测试用例提交表](" + cfg.TableURL + ")"
	}
	return feishuCard{
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "orange",
			Title:    feishuCardText{Tag: "plain_text", Content: "每周测试用例提交提醒"},
		},
		Elements: []map[string]any{
			cardMarkdown(fmt.Sprintf("请 %s 在本周五 14:00 前提供测试用例。%s", strings.Join(owners, "、"), tableLink)),
			cardMarkdown("**本周**：" + escapeLarkMarkdown(week) + "\n**确认状态**：\n" + strings.Join(statuses, "\n")),
			{
				"tag":     "action",
				"actions": actions,
			},
			cardMarkdown(fmt.Sprintf("未全部确认前，机器人将每 %s 提醒一次。", formatDurationCN(cfg.RepeatInterval))),
		},
	}
}
