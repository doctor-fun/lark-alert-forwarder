package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// 项目管理提醒卡片的交互回调：状态按钮 / 表单提交后 toast + 刷新卡片。

func (s *server) handlePMTaskAction(_ context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	taskID := firstNonEmpty(event.Action.Value["task_id"], "pm-task")
	title := firstNonEmpty(event.Action.Value["task_title"], "未命名事项")
	due := firstNonEmpty(event.Action.Value["due"], "-")
	owners := firstNonEmpty(event.Action.Value["owners"], "-")
	status := firstNonEmpty(
		strings.TrimSpace(event.Action.Value["status"]),
		stringFromAny(event.Action.FormValue["pm_status"]),
		stringFromAny(event.Action.Option),
	)
	note := firstNonEmpty(
		stringFromAny(event.Action.FormValue["pm_note"]),
		event.Action.Value["note"],
	)
	status = normalizePMTaskStatus(status)
	if status == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "请先选择状态"},
		}, nil
	}
	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	card := buildPMTaskCard(pmTaskCardInput{
		TaskID:   taskID,
		Title:    title,
		Due:      due,
		Owners:   owners,
		Summary:  firstNonEmpty(event.Action.Value["summary"], ""),
		Status:   status,
		Note:     note,
		Operator: operator,
		Updated:  time.Now().In(chinaLocation()).Format("2006-01-02 15:04"),
	})
	log.Printf("pm-task updated task=%s status=%s operator=%s note=%q",
		taskID, status, event.Operator.OpenID, truncateMsg(note, 80))
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已更新为「" + status + "」"},
		Card: &feishuCallbackCard{
			Type: "raw",
			Data: card,
		},
	}, nil
}

func normalizePMTaskStatus(raw string) string {
	switch strings.TrimSpace(raw) {
	case "done", "已完成", "完成":
		return "已完成"
	case "doing", "进行中", "处理中":
		return "进行中"
	case "blocked", "有阻塞", "阻塞":
		return "有阻塞"
	case "acked", "已收到", "收到":
		return "已收到"
	default:
		return strings.TrimSpace(raw)
	}
}

type pmTaskCardInput struct {
	TaskID   string
	Title    string
	Due      string
	Owners   string // 已含 <at> 的 lark_md
	Summary  string
	Status   string
	Note     string
	Operator string
	Updated  string
}

func buildPMTaskCard(in pmTaskCardInput) feishuCard {
	template := "blue"
	switch in.Status {
	case "已完成":
		template = "green"
	case "有阻塞":
		template = "red"
	case "进行中":
		template = "orange"
	case "已收到":
		template = "wathet"
	}
	statusLine := "待确认"
	if strings.TrimSpace(in.Status) != "" {
		statusLine = in.Status
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = "请 owner 在卡片上更新状态；有阻塞请填备注。"
	}
	elements := []map[string]any{
		cardV2Markdown(summary),
		cardV2Markdown(fmt.Sprintf(
			"**事项**：%s\n**截止**：%s\n**Owner**：%s\n**当前状态**：%s",
			escapeLarkMarkdown(in.Title),
			escapeLarkMarkdown(in.Due),
			in.Owners,
			escapeLarkMarkdown(statusLine),
		)),
	}
	if strings.TrimSpace(in.Note) != "" || strings.TrimSpace(in.Operator) != "" {
		elements = append(elements, cardV2Markdown(fmt.Sprintf(
			"**最近更新**：%s by %s\n**备注**：%s",
			escapeLarkMarkdown(firstNonEmpty(in.Updated, "-")),
			firstNonEmpty(in.Operator, "-"),
			escapeLarkMarkdown(firstNonEmpty(in.Note, "-")),
		)))
	}
	elements = append(elements, map[string]any{
		"tag":              "form",
		"name":             "pm_task_form_" + in.TaskID,
		"direction":        "vertical",
		"vertical_spacing": "8px",
		"elements": []map[string]any{
			cardV2Markdown("**更新状态**"),
			{
				"tag":  "select_static",
				"name": "pm_status",
				"placeholder": map[string]string{
					"tag":     "plain_text",
					"content": "选择状态",
				},
				"options": []map[string]any{
					{"text": map[string]string{"tag": "plain_text", "content": "已收到"}, "value": "acked"},
					{"text": map[string]string{"tag": "plain_text", "content": "进行中"}, "value": "doing"},
					{"text": map[string]string{"tag": "plain_text", "content": "已完成"}, "value": "done"},
					{"text": map[string]string{"tag": "plain_text", "content": "有阻塞"}, "value": "blocked"},
				},
			},
			{
				"tag":      "input",
				"name":     "pm_note",
				"required": false,
				"width":    "fill",
				"placeholder": map[string]string{
					"tag":     "plain_text",
					"content": "可选：进度说明 / 阻塞原因",
				},
			},
			{
				"tag":              "column_set",
				"horizontal_align": "left",
				"columns": []map[string]any{
					pmTaskSubmitButton("提交更新", "primary_filled", in, ""),
					pmTaskSubmitButton("已完成", "primary", in, "done"),
					pmTaskSubmitButton("有阻塞", "danger", in, "blocked"),
				},
			},
		},
	})
	return feishuCard{
		Schema: "2.0",
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: template,
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: "项目管理 · " + in.Title,
			},
		},
		Body: map[string]any{
			"elements": elements,
		},
	}
}

func pmTaskSubmitButton(label, btnType string, in pmTaskCardInput, fixedStatus string) map[string]any {
	value := map[string]string{
		"action":     "pm_task_update",
		"task_id":    in.TaskID,
		"task_title": in.Title,
		"due":        in.Due,
		"owners":     in.Owners,
		"summary":    in.Summary,
	}
	if fixedStatus != "" {
		value["status"] = fixedStatus
	}
	return map[string]any{
		"tag":   "column",
		"width": "auto",
		"elements": []map[string]any{
			{
				"tag":  "button",
				"name": "pm_submit_" + firstNonEmpty(fixedStatus, "form"),
				"text": map[string]string{
					"tag":     "plain_text",
					"content": label,
				},
				"type":             btnType,
				"form_action_type": "submit",
				"behaviors": []map[string]any{
					{
						"type":  "callback",
						"value": value,
					},
				},
			},
		},
	}
}

func chinaLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// 生产镜像可能没有 tzdata；不能退回 UTC，否则周五 14:00 会被错当成
		// 14:00 UTC（北京时间 22:00）。中国标准时间没有夏令时，固定 +08:00
		// 能保证周期任务仍按业务约定的北京时间运行。
		return time.FixedZone("CST", 8*60*60)
	}
	return loc
}
