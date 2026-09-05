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
	productionAcceptanceCompleteAction = "production_acceptance_complete"
	productionAcceptanceSuppressAction = "production_acceptance_suppress"
)

type productionAcceptanceReminderOwner struct {
	Name   string
	OpenID string
}

type productionAcceptanceReminderConfig struct {
	ChatID string
	Owners []productionAcceptanceReminderOwner
}

// productionAcceptanceReminder keeps the current reminder window in memory.
// A restart intentionally resets a prior completion/suppression, matching the
// existing test-case reminder's in-memory state model.
type productionAcceptanceReminder struct {
	cfg productionAcceptanceReminderConfig

	mu          sync.Mutex
	window      string
	lastSentDay string
	stopped     bool
	completed   bool
	operator    string
}

func parseProductionAcceptanceOwners(raw string) []productionAcceptanceReminderOwner {
	var owners []productionAcceptanceReminderOwner
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
		owners = append(owners, productionAcceptanceReminderOwner{Name: name, OpenID: openID})
	}
	return owners
}

func productionAcceptanceReminderFromEnv() *productionAcceptanceReminder {
	chatID := strings.TrimSpace(os.Getenv("PRODUCTION_ACCEPTANCE_REMINDER_CHAT_ID"))
	if chatID == "" {
		return nil
	}
	return &productionAcceptanceReminder{
		cfg: productionAcceptanceReminderConfig{
			ChatID: chatID,
			Owners: parseProductionAcceptanceOwners(os.Getenv("PRODUCTION_ACCEPTANCE_OWNERS")),
		},
	}
}

func productionAcceptanceWindow(now time.Time) (start, end time.Time, ok bool) {
	loc := chinaLocation()
	current := now.In(loc)
	start = time.Date(current.Year(), current.Month(), current.Day(), 9, 0, 0, 0, loc)
	daysSinceThursday := (int(current.Weekday()) - int(time.Thursday) + 7) % 7
	start = start.AddDate(0, 0, -daysSinceThursday)
	if current.Before(start) {
		start = start.AddDate(0, 0, -7)
	}
	end = start.AddDate(0, 0, 7)
	return start, end, !current.Before(start) && current.Before(end)
}

func productionAcceptanceWindowKey(now time.Time) string {
	start, _, _ := productionAcceptanceWindow(now)
	return start.Format("2006-01-02")
}

func (r *productionAcceptanceReminder) prepareSend(now time.Time) (window string, shouldSend bool) {
	if r == nil {
		return "", false
	}
	start, _, active := productionAcceptanceWindow(now)
	if !active {
		return "", false
	}
	window = start.Format("2006-01-02")
	day := now.In(chinaLocation()).Format("2006-01-02")

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.window != window {
		r.window = window
		r.lastSentDay = ""
		r.stopped = false
		r.completed = false
		r.operator = ""
	}
	if r.stopped || r.lastSentDay == day {
		return window, false
	}
	r.lastSentDay = day
	return window, true
}

func (r *productionAcceptanceReminder) prepareForcedSend(now time.Time) (window string, shouldSend bool) {
	if r == nil {
		return "", false
	}
	start, _, active := productionAcceptanceWindow(now)
	if !active {
		start = nextThursdayAt0900(now)
	}
	window = start.Format("2006-01-02")
	day := now.In(chinaLocation()).Format("2006-01-02")

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.window != window {
		r.window = window
		r.lastSentDay = ""
		r.stopped = false
		r.completed = false
		r.operator = ""
	}
	if r.stopped {
		return window, false
	}
	r.lastSentDay = day
	return window, true
}

func (r *productionAcceptanceReminder) stop(now time.Time, window, openID string, completed bool) (bool, string) {
	if r == nil {
		return false, "生产验收提醒未启用"
	}
	if !r.isOwner(openID) {
		return false, "仅Dave或Eve可以停止本轮提醒"
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.window == "" || strings.TrimSpace(window) != r.window {
		return false, "本轮生产验收提醒已失效"
	}
	r.stopped = true
	r.completed = completed
	r.operator = openID
	return true, ""
}

func (r *productionAcceptanceReminder) isOwner(openID string) bool {
	for _, owner := range r.cfg.Owners {
		if owner.OpenID == openID {
			return true
		}
	}
	return false
}

func (r *productionAcceptanceReminder) snapshot(now time.Time) (window string, stopped, completed bool, operator string) {
	window = productionAcceptanceWindowKey(now)
	if r == nil {
		return window, false, false, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.window != window {
		return window, false, false, ""
	}
	return window, r.stopped, r.completed, r.operator
}

func (s *server) startProductionAcceptanceReminder() func() {
	if s == nil || s.productionAcceptanceReminder == nil || !s.feishuAppConfigured() {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			next := nextThursdayAt0900(time.Now())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case startedAt := <-timer.C:
				s.sendProductionAcceptanceReminder(startedAt)
				for day := 1; day < 7; day++ {
					daily := time.NewTimer(24 * time.Hour)
					select {
					case <-ctx.Done():
						daily.Stop()
						return
					case at := <-daily.C:
						s.sendProductionAcceptanceReminder(at)
					}
				}
			}
		}
	}()
	return cancel
}

func nextThursdayAt0900(now time.Time) time.Time {
	loc := chinaLocation()
	current := now.In(loc)
	candidate := time.Date(current.Year(), current.Month(), current.Day(), 9, 0, 0, 0, loc)
	days := (int(time.Thursday) - int(current.Weekday()) + 7) % 7
	candidate = candidate.AddDate(0, 0, days)
	if !candidate.After(current) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate
}

func (s *server) sendProductionAcceptanceReminder(now time.Time) {
	if s == nil || s.productionAcceptanceReminder == nil {
		return
	}
	window, shouldSend := s.productionAcceptanceReminder.prepareSend(now)
	if !shouldSend {
		return
	}
	card := buildProductionAcceptanceReminderCard(s.productionAcceptanceReminder.cfg, window, false, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.sendFeishuAppCardTo(ctx, s.productionAcceptanceReminder.cfg.ChatID, "chat_id", feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
		log.Printf("production acceptance reminder: send failed: %v", err)
		return
	}
	log.Printf("production acceptance reminder: sent window=%s chat=%s", window, s.productionAcceptanceReminder.cfg.ChatID)
}

func (s *server) sendProductionAcceptanceReminderNow(now time.Time) {
	if s == nil || s.productionAcceptanceReminder == nil {
		return
	}
	window, shouldSend := s.productionAcceptanceReminder.prepareForcedSend(now)
	if !shouldSend {
		return
	}
	card := buildProductionAcceptanceReminderCard(s.productionAcceptanceReminder.cfg, window, false, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.sendFeishuAppCardTo(ctx, s.productionAcceptanceReminder.cfg.ChatID, "chat_id", feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
		log.Printf("production acceptance reminder: manual send failed: %v", err)
		return
	}
	log.Printf("production acceptance reminder: manually sent window=%s chat=%s", window, s.productionAcceptanceReminder.cfg.ChatID)
}

func (s *server) handleProductionAcceptanceReminderSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && !s.validForwarderToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.productionAcceptanceReminder == nil || !s.feishuAppConfigured() {
		http.Error(w, "production acceptance reminder is not configured", http.StatusPreconditionFailed)
		return
	}
	s.sendProductionAcceptanceReminderNow(time.Now())
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"sent"}`))
}

func (s *server) handleProductionAcceptanceReminderMessageEvent(_ context.Context, raw json.RawMessage) error {
	if s == nil || s.productionAcceptanceReminder == nil || !s.feishuAppConfigured() {
		return nil
	}
	var event feishuMessageReceiveEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if event.Message.MessageType != "" && event.Message.MessageType != "text" {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(event.Message.ChatID), strings.TrimSpace(s.productionAcceptanceReminder.cfg.ChatID)) {
		return nil
	}
	text := normalizeDirtyWorkLaunchCommand(feishuMessageText(event.Message.Content))
	if text != "生产验收" && text != "/生产验收" {
		return nil
	}
	s.sendProductionAcceptanceReminderNow(time.Now())
	return nil
}

func (s *server) handleProductionAcceptanceReminderAction(_ context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	return s.handleProductionAcceptanceReminderActionAt(time.Now(), event)
}

func (s *server) handleProductionAcceptanceReminderActionAt(now time.Time, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	if s == nil || s.productionAcceptanceReminder == nil {
		return feishuCardCallbackResponse{Toast: &feishuCardToast{Type: "warning", Content: "生产验收提醒未启用"}}, nil
	}
	action := event.Action.Value["action"]
	completed := action == productionAcceptanceCompleteAction
	ok, reason := s.productionAcceptanceReminder.stop(now, event.Action.Value["window"], strings.TrimSpace(event.Operator.OpenID), completed)
	if !ok {
		return feishuCardCallbackResponse{Toast: &feishuCardToast{Type: "warning", Content: reason}}, nil
	}
	window, stopped, completed, operator := s.productionAcceptanceReminder.snapshot(now)
	card := buildProductionAcceptanceReminderCard(s.productionAcceptanceReminder.cfg, window, stopped, completed, operator)
	message := "本轮生产验收提醒已屏蔽"
	if completed {
		message = "生产验收已完成，本轮提醒已停止"
	}
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: message},
		Card:  rawFeishuCallbackCard(card),
	}, nil
}

func buildProductionAcceptanceReminderCard(cfg productionAcceptanceReminderConfig, window string, stopped, completed bool, operator string) feishuCard {
	owners := make([]string, 0, len(cfg.Owners))
	operatorName := ""
	for _, owner := range cfg.Owners {
		owners = append(owners, fmt.Sprintf("<at id=\"%s\"></at>", owner.OpenID))
		if owner.OpenID == operator {
			operatorName = owner.Name
		}
	}
	status := "待验收"
	if stopped && completed {
		status = "已完成"
	} else if stopped {
		status = "已屏蔽"
	}
	statusDetail := "**状态**：" + status
	if operatorName != "" {
		statusDetail += "（操作人：" + operatorName + "）"
	}
	actions := []map[string]any{}
	if !stopped {
		actions = []map[string]any{
			{
				"tag":   "button",
				"text":  map[string]string{"tag": "plain_text", "content": "生产验收已完成"},
				"type":  "primary",
				"value": map[string]string{"action": productionAcceptanceCompleteAction, "window": window},
			},
			{
				"tag":   "button",
				"text":  map[string]string{"tag": "plain_text", "content": "本轮无需生产验收提醒"},
				"type":  "default",
				"value": map[string]string{"action": productionAcceptanceSuppressAction, "window": window},
			},
		}
	}
	elements := []map[string]any{
		cardMarkdown(fmt.Sprintf("请 %s 完成生产验收。", strings.Join(owners, "、"))),
		cardMarkdown("**提醒窗口**：" + escapeLarkMarkdown(window) + " 09:00 起，持续 7 天\n" + statusDetail),
	}
	if len(actions) > 0 {
		elements = append(elements, map[string]any{"tag": "action", "actions": actions})
	}
	if !stopped {
		elements = append(elements, cardMarkdown("提醒窗口内每天最多提醒一次；任一指定负责人操作后，本轮将全局停止提醒。"))
	}
	return feishuCard{
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "orange",
			Title:    feishuCardText{Tag: "plain_text", Content: "生产验收提醒"},
		},
		Elements: elements,
	}
}
