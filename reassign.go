package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// internalReassignEvent 与 backend `biz.ReassignNotifyPayload` 字段一一对应。
// 字段全小驼峰；reason 由 backend 给出（"reassign" | "admin_override" | "claim"），
// forwarder 用它决定文案（不要在此重写映射，否则 backend / forwarder 容易飘）。
type internalReassignEvent struct {
	IncidentID     int64  `json:"incidentId"`
	AlertName      string `json:"alertName"`
	Service        string `json:"service"`
	Env            string `json:"env"`
	Severity       string `json:"severity"`
	Summary        string `json:"summary"`
	Description    string `json:"description"`
	DashboardURL   string `json:"dashboardUrl"`
	FeishuMsgID    string `json:"feishuMsgId"`
	FeishuChatID   string `json:"feishuChatId"`
	PrevAssigneeID string `json:"prevAssigneeOpenId"`
	NewAssigneeID  string `json:"newAssigneeOpenId"`
	OperatorOpenID string `json:"operatorOpenId"`
	Reason         string `json:"reason"`
}

// handleReassignNotify 接收 backend `POST /internal/incident_reassigned`：
//   - 鉴权同 grafana webhook（GRAFANA_FORWARDER_TOKEN）；
//   - 立即异步并发投递三路通知，HTTP 1 秒内返回 202；
//   - 任何一路失败只 log，**不重试**——backend 的通知本身是辅助提示，不重试可
//     避免飞书反复打扰用户。
//
// 入参校验失败回 400；token 不对回 401；解析失败回 400。
func (s *server) handleReassignNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && !s.validForwarderToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	var ev internalReassignEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if ev.IncidentID <= 0 {
		http.Error(w, "incidentId required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(ev.NewAssigneeID) == "" {
		http.Error(w, "newAssigneeOpenId required", http.StatusBadRequest)
		return
	}
	if !s.feishuAppConfigured() {
		// Feishu app 没接，DM 发不了；只 log 一行让 ops 注意。
		log.Printf("reassign-notify: feishu app not configured, skipping incident=%d", ev.IncidentID)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"skipped","reason":"feishu_app_not_configured"}`))
		return
	}

	// 异步触发；HTTP 立刻返回，backend 调用方不被飞书 API 抖动卡住。
	go s.dispatchReassignNotify(ev)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"queued"}`))
}

// dispatchReassignNotify 并发投递 3 路；每路独立 5s 超时。
//
// 设计：用 WaitGroup 是为了**让单测可以等到所有路径跑完**；生产路径里 main 不
// 等 wg，靠 ctx timeout 自动收尾，不会泄漏 goroutine。
func (s *server) dispatchReassignNotify(ev internalReassignEvent) {
	var wg sync.WaitGroup

	// 1) DM 新接手人：5 按钮卡（我来处理 / 已修复 / 标为误报 / 打开详情 / AI 归因）
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		card := buildReassignDMCard(ev)
		if _, err := s.sendFeishuAppCardTo(ctx, ev.NewAssigneeID, "open_id",
			feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
			log.Printf("reassign-notify: DM new assignee failed incident=%d open_id=%s: %v",
				ev.IncidentID, ev.NewAssigneeID, err)
			return
		}
		log.Printf("reassign-notify: DM new assignee ok incident=%d open_id=%s reason=%s",
			ev.IncidentID, ev.NewAssigneeID, ev.Reason)
	}()

	// 2) DM 原责任人：简版「单已转出」卡；prev 为空（首次抢单 / picker 没人）就跳过。
	if strings.TrimSpace(ev.PrevAssigneeID) != "" && ev.PrevAssigneeID != ev.NewAssigneeID {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			card := buildHandoffDMCard(ev)
			if _, err := s.sendFeishuAppCardTo(ctx, ev.PrevAssigneeID, "open_id",
				feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
				log.Printf("reassign-notify: DM prev assignee failed incident=%d open_id=%s: %v",
					ev.IncidentID, ev.PrevAssigneeID, err)
				return
			}
			log.Printf("reassign-notify: DM prev assignee ok incident=%d open_id=%s",
				ev.IncidentID, ev.PrevAssigneeID)
		}()
	}

	// 3) 群里 thread reply：审计透明度。FeishuMsgID 为空（创建期间还没 BindFeishuMessage）就跳过。
	if strings.TrimSpace(ev.FeishuMsgID) != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			text := buildReassignThreadText(ev)
			if err := s.replyFeishuMessage(ctx, ev.FeishuMsgID, text); err != nil {
				log.Printf("reassign-notify: group thread reply failed incident=%d msg=%s: %v",
					ev.IncidentID, ev.FeishuMsgID, err)
				return
			}
			log.Printf("reassign-notify: group thread reply ok incident=%d msg=%s",
				ev.IncidentID, ev.FeishuMsgID)
		}()
	}

	wg.Wait()
}

// buildReassignDMCard 给新接手人发的卡片：**完整 5 按钮**，与新告警 DM 体验一致，
// 只是头部文案告诉他"是被转过来的"。
//
// 设计：仍然用 callback button（action=claim/resolve/discard/copilot_analyze），
// 这样 handleCardAction 不用改一行，所有交互沿用现有路径。
func buildReassignDMCard(ev internalReassignEvent) feishuCard {
	headline := fmt.Sprintf("📥 你被指派了告警 #%d：%s", ev.IncidentID, ev.AlertName)
	desc := "管理员把这条告警转给你了。可以直接在卡片上点 \"我来处理\" 接走，避免重复跟进。"
	if ev.Reason == "claim" {
		// "claim" 表示别人在群里点了"我来处理"——本意是抢单，目标人会被换。这种
		// 改派对新接手人是"主动接走"，DM 的文案换成确认对话。
		headline = fmt.Sprintf("✅ 你已接走告警 #%d：%s", ev.IncidentID, ev.AlertName)
		desc = "你已在群里抢单，原责任人会同步收到通知。可直接在 DM 内继续操作。"
	}
	idStr := strconv.FormatInt(ev.IncidentID, 10)

	card := feishuCard{
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "blue",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: headline,
			},
		},
		Elements: []map[string]any{
			cardMarkdown(desc),
			{
				"tag": "div",
				"fields": []map[string]any{
					cardField("服务", firstNonEmpty(ev.Service, "-")),
					cardField("级别", firstNonEmpty(ev.Severity, "-")),
					cardField("环境", firstNonEmpty(ev.Env, "-")),
					cardField("Incident", "#"+idStr),
				},
			},
		},
	}
	if strings.TrimSpace(ev.Summary) != "" {
		card.Elements = append(card.Elements,
			cardMarkdown(fmt.Sprintf("**摘要**：%s", escapeLarkMarkdown(ev.Summary))))
	}

	// 5 按钮（claim / resolve / discard / 打开详情 / AI 归因）。
	actions := []map[string]any{
		cardCallbackButton("我来处理", map[string]string{
			"action":      "claim",
			"alertname":   ev.AlertName,
			"incident_id": idStr,
		}),
		cardCallbackButton("已修复", map[string]string{
			"action":      "resolve",
			"alertname":   ev.AlertName,
			"incident_id": idStr,
		}),
		cardCallbackButton("标为误报", map[string]string{
			"action":      "discard",
			"alertname":   ev.AlertName,
			"incident_id": idStr,
		}),
	}
	if link := strings.TrimSpace(ev.DashboardURL); link != "" {
		actions = append(actions, cardURLButton("打开大盘", link))
	}
	actions = append(actions, cardCallbackButton("AI 归因", map[string]string{
		"action":      "copilot_analyze",
		"alertname":   ev.AlertName,
		"service":     ev.Service,
		"env":         ev.Env,
		"severity":    ev.Severity,
		"summary":     ev.Summary,
		"description": ev.Description,
		"link":        ev.DashboardURL,
		"incident_id": idStr,
	}))

	card.Elements = append(card.Elements, map[string]any{
		"tag":     "action",
		"actions": actions,
	})
	card.Elements = append(card.Elements, map[string]any{
		"tag": "note",
		"elements": []map[string]any{
			{
				"tag":     "lark_md",
				"content": fmt.Sprintf("incident #%s · 这条 DM 由值班调度自动发送", idStr),
			},
		},
	})
	return card
}

// buildHandoffDMCard 给原责任人的"已转出"通知；不带操作按钮，避免他再去 ACK
// 已经不属于他的工单引起混乱。仅一个"打开详情"链接（如果有 dashboard）。
func buildHandoffDMCard(ev internalReassignEvent) feishuCard {
	headline := fmt.Sprintf("ℹ️ 告警 #%d 已转出", ev.IncidentID)
	body := fmt.Sprintf("「%s」已不再由你负责，无需继续处理。", escapeLarkMarkdown(ev.AlertName))
	switch ev.Reason {
	case "claim":
		body = fmt.Sprintf("「%s」已被同事在群里抢单接走，无需继续处理。", escapeLarkMarkdown(ev.AlertName))
	case "admin_override":
		body = fmt.Sprintf("「%s」已由管理员转给其他同事，无需继续处理。", escapeLarkMarkdown(ev.AlertName))
	}
	idStr := strconv.FormatInt(ev.IncidentID, 10)

	card := feishuCard{
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "grey",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: headline,
			},
		},
		Elements: []map[string]any{
			cardMarkdown(body),
			{
				"tag": "div",
				"fields": []map[string]any{
					cardField("服务", firstNonEmpty(ev.Service, "-")),
					cardField("级别", firstNonEmpty(ev.Severity, "-")),
					cardField("Incident", "#"+idStr),
				},
			},
		},
	}
	if link := strings.TrimSpace(ev.DashboardURL); link != "" {
		card.Elements = append(card.Elements, map[string]any{
			"tag": "action",
			"actions": []map[string]any{
				cardURLButton("打开大盘", link),
			},
		})
	}
	return card
}

// buildReassignThreadText 群里 thread reply 的纯文本（卡片群发会刷屏，群里要"克
// 制"）。文案直白：「告警 #N 已从 @A 转给 @B」，不带表情、不打 markdown。
//
// at 标签：Feishu IM 的 text 消息支持 `<at user_id="ou_xxx">@xxx</at>` 内联 @；
// 我们这里不知道用户的"@姓名"（admin 没把 name 带过来），用 user_id 形式让飞书
// 自动渲染。
func buildReassignThreadText(ev internalReassignEvent) string {
	var sb strings.Builder
	sb.WriteString("告警 #")
	sb.WriteString(strconv.FormatInt(ev.IncidentID, 10))
	switch ev.Reason {
	case "claim":
		sb.WriteString(" 已由 ")
		sb.WriteString(mentionTag(ev.NewAssigneeID))
		sb.WriteString(" 抢单接走")
		if strings.TrimSpace(ev.PrevAssigneeID) != "" {
			sb.WriteString("（原责任人 ")
			sb.WriteString(mentionTag(ev.PrevAssigneeID))
			sb.WriteString(" 已收到 DM 通知）")
		}
	case "admin_override":
		sb.WriteString(" 由管理员转派给 ")
		sb.WriteString(mentionTag(ev.NewAssigneeID))
		if strings.TrimSpace(ev.PrevAssigneeID) != "" {
			sb.WriteString("（原 ")
			sb.WriteString(mentionTag(ev.PrevAssigneeID))
			sb.WriteString("）")
		}
	default:
		sb.WriteString(" 已转派给 ")
		sb.WriteString(mentionTag(ev.NewAssigneeID))
		if strings.TrimSpace(ev.PrevAssigneeID) != "" {
			sb.WriteString("（原 ")
			sb.WriteString(mentionTag(ev.PrevAssigneeID))
			sb.WriteString("）")
		}
	}
	return sb.String()
}

// mentionTag 把 open_id 包成飞书 text 消息可识别的 @ 标签。
// 飞书 SDK 不会自动 escape，但 open_id 本身只含字母数字，不需要 escape。
func mentionTag(openID string) string {
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return "<未知>"
	}
	return fmt.Sprintf(`<at user_id="%s"></at>`, openID)
}
