package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// dataWorksPayload 兼容 DataWorks 自定义 Webhook 告警的常见 body 形态。
// DataWorks 各版本/各告警类型推送的字段名并不统一，这里把可能承载“告警正文”
// 的字段都收一遍，取第一个非空的作为描述；其余路由维度（service/severity/
// alertname/env）一律以 URL 查询参数为准，正文里取不到就用兜底值。
type dataWorksPayload struct {
	Content   string `json:"content"`
	Message   string `json:"message"`
	Text      string `json:"text"`
	Body      string `json:"body"`
	Title     string `json:"title"`
	Subject   string `json:"subject"`
	AlertName string `json:"alertName"`
	Alertname string `json:"alertname"`
	Severity  string `json:"severity"`
	Level     string `json:"level"`
	Service   string `json:"service"`
	Status    string `json:"status"`
	Instance  string `json:"instanceId"`
}

// handleDataWorksAlert 是数据团队 DataWorks 告警的接入入口。它把 DataWorks
// 的 webhook body + URL 查询参数适配成内部 grafanaWebhook，再复用 processAlert
// 的主链路（silence / 入库 / 分人 / 飞书卡片 / 电话升级）。
//
// 鉴权：与 /grafana/feishu 共用 GRAFANA_FORWARDER_TOKEN；DataWorks 不能加请求头，
// 所以走 ?token= 查询参数（validForwarderToken 已支持）。
//
// 路由维度全部来自 URL 查询参数，DataWorks 侧只需在告警 webhook 地址上拼好：
//
//	?token=xxx&service=data-platform&severity=p1&alertname=xxx&env=prod
//
// service 命中 SERVICE_CHAT_ROUTES 即进数据团队自己的飞书群；severity 命中
// SERVICE_VOICE_SEVERITIES（或全局 ALERT_VOICE_SEVERITIES）才会电话升级。
func (s *server) handleDataWorksAlert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && !s.validForwarderToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var body dataWorksPayload
	// DataWorks body 不保证是合法 JSON（可能直接推纯文本），解析失败不报错，
	// 退化成把整段原文当正文。
	if len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &body)
	}

	q := r.URL.Query()
	payload := s.dataWorksToGrafana(body, q, string(raw))
	log.Printf("dataworks alert in: service=%s severity=%s alertname=%s status=%s",
		payload.CommonLabels["service"], payload.CommonLabels["severity"],
		payload.CommonLabels["alertname"], payload.Status)
	s.processAlert(w, r.Context(), payload)
}

// dataWorksToGrafana 把 DataWorks body + 查询参数映射成 grafanaWebhook。
// 查询参数优先级最高，其次 body 字段，最后兜底默认值。rawBody 用于 body 解析
// 不出任何正文时的最终兜底。
func (s *server) dataWorksToGrafana(body dataWorksPayload, q queryGetter, rawBody string) grafanaWebhook {
	alertName := firstNonEmpty(
		q.Get("alertname"), q.Get("alert"),
		body.AlertName, body.Alertname, body.Title, body.Subject,
		"DataWorks告警",
	)
	service := strings.TrimSpace(firstNonEmpty(q.Get("service"), body.Service, "dataworks"))
	severity := strings.TrimSpace(firstNonEmpty(q.Get("severity"), q.Get("level"), body.Severity, body.Level, "P1"))
	env := strings.TrimSpace(firstNonEmpty(q.Get("env"), "prod"))

	status := strings.ToLower(strings.TrimSpace(firstNonEmpty(q.Get("status"), body.Status, "firing")))
	if status != "resolved" {
		status = "firing"
	}

	text := firstNonEmpty(body.Content, body.Message, body.Text, body.Body, body.Title, body.Subject)
	if strings.TrimSpace(text) == "" {
		text = strings.TrimSpace(rawBody)
	}
	if strings.TrimSpace(text) == "" {
		text = alertName
	}

	labels := map[string]string{
		"alertname": alertName,
		"service":   service,
		"severity":  severity,
		"env":       env,
		"source":    "dataworks",
	}
	if body.Instance != "" {
		labels["instance"] = body.Instance
	}
	annotations := map[string]string{
		"summary":     alertName,
		"description": text,
	}

	now := time.Now()
	return grafanaWebhook{
		Status:            status,
		Title:             alertName,
		Message:           text,
		CommonLabels:      labels,
		CommonAnnotations: annotations,
		Alerts: []grafanaAlert{{
			Status:      status,
			Labels:      labels,
			Annotations: annotations,
			StartsAt:    now,
		}},
	}
}

// queryGetter 抽出 url.Values.Get 这一个方法，方便单测直接传 fake，不必构造
// 完整 *http.Request。
type queryGetter interface {
	Get(string) string
}
