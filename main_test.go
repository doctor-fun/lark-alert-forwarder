package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFeishuChatIDForPayload_ServiceRoute(t *testing.T) {
	s := &server{
		feishuChatID:      "oc_default",
		feishuP1ChatID:    "oc_p1",
		attributionChatID: "oc_attribution",
		serviceChatRoutes: parseServiceChatRoutes("data-platform=oc_data;matrix-bi=oc_bi"),
	}
	// service 路由优先于 severity
	if got := s.feishuChatIDForPayload(grafanaWebhook{CommonLabels: map[string]string{"service": "data-platform", "severity": "P1"}}); got != "oc_data" {
		t.Fatalf("data-platform should route to oc_data, got %s", got)
	}
	// service 大小写不敏感
	if got := s.feishuChatIDForPayload(grafanaWebhook{CommonLabels: map[string]string{"service": "Matrix-BI"}}); got != "oc_bi" {
		t.Fatalf("Matrix-BI should route to oc_bi, got %s", got)
	}
	for _, service := range []string{"attribution-service", "Attribution-Worker"} {
		if got := s.feishuChatIDForPayload(grafanaWebhook{CommonLabels: map[string]string{"service": service, "severity": "P1"}}); got != "oc_attribution" {
			t.Fatalf("%s should route to oc_attribution, got %s", service, got)
		}
	}
	// 未命中 service → 回退 severity 路由
	if got := s.feishuChatIDForPayload(grafanaWebhook{CommonLabels: map[string]string{"service": "matrix-api", "severity": "P1"}}); got != "oc_p1" {
		t.Fatalf("matrix-api P1 should fall back to oc_p1, got %s", got)
	}
	// 普通 → 默认群
	if got := s.feishuChatIDForPayload(grafanaWebhook{CommonLabels: map[string]string{"service": "matrix-api", "severity": "P3"}}); got != "oc_default" {
		t.Fatalf("default should be oc_default, got %s", got)
	}
}

func TestDataAlertMentionOverrideUsesOnlyDesignatedResponder(t *testing.T) {
	s := &server{
		feishuChatID:           "oc_default",
		dataAlertChatID:        "oc_data",
		dataAlertMentionOpenID: "ou_sunbin",
		serviceChatRoutes:      parseServiceChatRoutes("data-platform=oc_data"),
	}
	ctxInfo := cardContext{
		AssigneeOpenID:     "ou_other",
		FireMentionOpenIDs: []string{"ou_a", "ou_b"},
	}
	payload := grafanaWebhook{
		Status:       "firing",
		CommonLabels: map[string]string{"service": "data-platform", "severity": "P2"},
	}
	s.applyDataAlertMentionOverride(payload, &ctxInfo)
	if ctxInfo.AssigneeOpenID != "ou_sunbin" || len(ctxInfo.FireMentionOpenIDs) != 0 {
		t.Fatalf("data alert mentions = assignee:%s cc:%v, want only ou_sunbin", ctxInfo.AssigneeOpenID, ctxInfo.FireMentionOpenIDs)
	}
}

func TestParseServiceChatRoutes(t *testing.T) {
	if parseServiceChatRoutes("") != nil {
		t.Fatal("empty should be nil")
	}
	m := parseServiceChatRoutes("a=oc_1 ; b=oc_2\nc=oc_3")
	if len(m) != 3 || m["a"] != "oc_1" || m["b"] != "oc_2" || m["c"] != "oc_3" {
		t.Fatalf("parse failed: %#v", m)
	}
}

func TestBuildAssigneeDMCard(t *testing.T) {
	payload := grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "MatrixAPIHighP99Latency",
			"service":   "matrix-api",
			"env":       "prod",
			"severity":  "P1",
		},
		CommonAnnotations: map[string]string{
			"summary": "P99 延迟超过阈值",
		},
		ExternalURL: "https://grafana.example.com",
	}
	card := buildAssigneeDMCard(payload, cardContext{IncidentID: 42, AssigneeOpenID: "ou_test_42"})

	if card.Header.Template != "red" {
		t.Fatalf("header template = %q, want red", card.Header.Template)
	}
	if !strings.Contains(card.Header.Title.Content, "MatrixAPIHighP99Latency") {
		t.Fatalf("DM title missing alertname: %q", card.Header.Title.Content)
	}
	// 找一下 callback button 的 incident_id，确保 ACK 回调能反查 incident
	found := false
	for _, el := range card.Elements {
		actions, ok := el["actions"].([]map[string]any)
		if !ok {
			continue
		}
		for _, a := range actions {
			v, _ := a["value"].(map[string]string)
			if v["action"] == "claim" && v["incident_id"] == "42" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("DM card missing claim callback button with incident_id=42, elements=%+v", card.Elements)
	}
}

func TestBuildAssigneeDMCard_NoIncidentID(t *testing.T) {
	// 兜底场景：backend 完全不可用 / 没拿到 incident_id 时，DM 也不能崩，
	// 至少能让责任人看到"哪条告警"。callback button 应该被省略——避免点了
	// 之后 backend 反查失败 toast。
	card := buildAssigneeDMCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "Foo",
		},
	}, cardContext{IncidentID: 0, AssigneeOpenID: "ou_x"})

	for _, el := range card.Elements {
		if actions, ok := el["actions"].([]map[string]any); ok {
			for _, a := range actions {
				if v, _ := a["value"].(map[string]string); v["action"] == "claim" {
					t.Fatalf("DM should not have claim button without incident_id, got %+v", v)
				}
			}
		}
	}
}

func TestBuildFeishuCard(t *testing.T) {
	startsAt := time.Date(2026, 4, 27, 16, 0, 0, 0, time.UTC)
	msg := buildFeishuCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "MatrixAPIHighP99Latency",
			"service":   "matrix-api",
			"env":       "prod",
			"severity":  "P1",
		},
		CommonAnnotations: map[string]string{
			"summary":     "P99 延迟超过阈值",
			"description": "matrix-api P99 > 1s 持续 5 分钟",
		},
		ExternalURL: "https://grafana.example.com",
		Alerts: []grafanaAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "MatrixAPIHighP99Latency",
				},
				StartsAt:     startsAt,
				PanelURL:     "https://grafana.example.com/panel",
				GeneratorURL: "https://grafana.example.com/rule",
				Values: map[string]any{
					"B": 1.37,
				},
			},
		},
	}, true, cardContext{})

	if msg.MsgType != "interactive" {
		t.Fatalf("MsgType = %q, want interactive", msg.MsgType)
	}
	if msg.Card.Header.Template != "red" {
		t.Fatalf("header template = %q, want red", msg.Card.Header.Template)
	}
	if msg.Card.Header.Title.Content != "[Grafana告警] FIRING MatrixAPIHighP99Latency" {
		t.Fatalf("title = %q", msg.Card.Header.Title.Content)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"[Grafana告警] FIRING MatrixAPIHighP99Latency",
		"matrix-api",
		"prod",
		"P1",
		"P99 延迟超过阈值",
		"持续 5 分钟",
		"2026-04-28 00:00:00",
		"我来处理",
		"https://grafana.example.com/panel",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("card does not contain %q:\n%s", want, body)
		}
	}
}

func TestBuildFeishuCardIncludesAlertContext(t *testing.T) {
	msg := buildFeishuCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "MatrixAPIDownstreamErrorRateP2",
			"service":   "matrix-api",
			"env":       "prod",
			"severity":  "P2",
			"category":  "dependency",
		},
		CommonAnnotations: map[string]string{
			"summary":     "下游错误率超过阈值",
			"description": "target 维度错误率 > 5%",
		},
		Alerts: []grafanaAlert{{
			Labels: map[string]string{
				"alertname": "MatrixAPIDownstreamErrorRateP2",
				"target":    "order-srv",
			},
			PanelURL: "https://grafana.example.com/panel-31",
			Values: map[string]any{
				"B": 0.08,
			},
		}},
	}, true, cardContext{})

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"dependency",
		"order-srv",
		"当前值",
		"0.08",
		"category",
		"target",
		"current_value",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("card does not contain %q:\n%s", want, body)
		}
	}
}

// TestBuildFeishuCard_DetailsShowInstanceLabels 防止回归：
// 按 target / error_type 分组的规则一次带多个实例，各实例 alertname 完全相同，
// 告警明细必须打印实例独有标签，否则就是 N 行重复噪音，oncall 看不出哪个下游红了。
func TestBuildFeishuCard_DetailsShowInstanceLabels(t *testing.T) {
	alert := func(target, errorType string) grafanaAlert {
		return grafanaAlert{
			Status: "firing",
			Labels: map[string]string{
				"alertname":      "MatrixAPIDownstreamInfraErrors",
				"service":        "matrix-api",
				"env":            "prod",
				"grafana_folder": "服务巡检大盘",
				"target":         target,
				"error_type":     errorType,
			},
		}
	}
	msg := buildFeishuCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "MatrixAPIDownstreamInfraErrors",
			"service":   "matrix-api",
			"env":       "prod",
			"severity":  "P1",
		},
		CommonAnnotations: map[string]string{
			"summary":     "[需介入, 下游问题] matrix-api 调下游基础设施 / 三方接口异常",
			"description": "规则: 按 target + error_type 分组, 5m 速率 > 0.05/s",
		},
		Alerts: []grafanaAlert{
			alert("matrix-backend", "Canceled"),
			alert("media-srv", "Canceled"),
		},
	}, true, cardContext{})

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"error_type=Canceled, target=matrix-backend",
		"error_type=Canceled, target=media-srv",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("card does not contain %q:\n%s", want, body)
		}
	}
	// commonLabels 已渲染在卡片顶部，不该在明细里重复。
	if strings.Contains(body, "service=matrix-api") || strings.Contains(body, "grafana_folder=") {
		t.Fatalf("common label repeated in details:\n%s", body)
	}
}

// TestInstanceLabelSummary_NoDistinctLabels 保证单实例、无独有标签的老告警
// 仍回退到 alertname，不会退化成空行。
func TestInstanceLabelSummary_NoDistinctLabels(t *testing.T) {
	got := instanceLabelSummary(grafanaAlert{
		Status: "firing",
		Labels: map[string]string{
			"alertname":      "MatrixAPIHighP99Latency",
			"grafana_folder": "服务巡检大盘",
			"service":        "matrix-api",
			"empty":          "  ",
		},
	}, map[string]string{"alertname": "MatrixAPIHighP99Latency", "service": "matrix-api"})
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

// TestBuildFeishuCard_AssigneeMentionUsesCardSyntax 防止回归：
// 互动卡片 lark_md 内 @ 人必须用 <at id="ou_..."></at>，而不是 <at user_id="...">。
// 后者是 msg_type=text 的语法，飞书互动卡片会静默吞掉，导致"责任人"那行 @ 不出来。
// 真实事故：incident #5/#13 卡片"责任人："为空，但 backend 已分人。
func TestBuildFeishuCard_AssigneeMentionUsesCardSyntax(t *testing.T) {
	msg := buildFeishuCard(grafanaWebhook{
		Status:       "firing",
		CommonLabels: map[string]string{"alertname": "Foo", "service": "matrix-api", "env": "prod", "severity": "P1"},
		Alerts:       []grafanaAlert{{Labels: map[string]string{"alertname": "Foo"}}},
	}, true, cardContext{
		IncidentID:     42,
		AssigneeOpenID: "ou_assignee_xyz",
	})
	// 走 element.text.content 的字面字符串，绕开 JSON 转义带来的歧义。
	for _, el := range msg.Card.Elements {
		var c string
		switch txt := el["text"].(type) {
		case map[string]any:
			c, _ = txt["content"].(string)
		case map[string]string:
			c = txt["content"]
		}
		if !strings.Contains(c, "责任人") {
			continue
		}
		if !strings.Contains(c, `<at id="ou_assignee_xyz"></at>`) {
			t.Fatalf("card must @ assignee with <at id=...> (card syntax). got: %q", c)
		}
		if strings.Contains(c, "<at user_id=") {
			t.Fatalf("card must NOT use <at user_id=...> (text syntax, silently dropped). got: %q", c)
		}
		return
	}
	t.Fatalf("card has no 责任人 element; ctxInfo.AssigneeOpenID was %q", "ou_assignee_xyz")
}

func TestBuildFeishuCardUsesURLButtonWithoutCallback(t *testing.T) {
	msg := buildFeishuCard(grafanaWebhook{
		Status:       "firing",
		CommonLabels: map[string]string{"alertname": "MatrixAPIHighP99Latency"},
		Alerts: []grafanaAlert{{
			Labels:   map[string]string{"alertname": "MatrixAPIHighP99Latency"},
			PanelURL: "https://grafana.example.com/panel",
		}},
	}, false, cardContext{})
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"url":"https://grafana.example.com/panel"`) {
		t.Fatalf("fallback card should use url button: %s", body)
	}
	if strings.Contains(body, `"value"`) {
		t.Fatalf("fallback card should not contain callback value: %s", body)
	}
}

func TestBuildFeishuCardUsesFallbackDashboardWithCallback(t *testing.T) {
	msg := buildFeishuCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "MatrixAPIContactPointConfigured",
			"service":   "matrix-api",
			"env":       "prod",
		},
	}, true, cardContext{})
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "我来处理") {
		t.Fatalf("card should contain claim button: %s", body)
	}
	if !strings.Contains(body, "AI 归因") || !strings.Contains(body, "copilot_analyze") {
		t.Fatalf("card should contain copilot callback: %s", body)
	}
	if !strings.Contains(body, "打开大盘") || !strings.Contains(body, matrixAPIDashboardURL) {
		t.Fatalf("card should contain fallback dashboard link: %s", body)
	}
}

func TestHandleGrafanaWebhookForwardsToFeishuWebhook(t *testing.T) {
	var got feishuCardMessage
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode feishu body: %v", err)
		}
		_, _ = w.Write([]byte(`{"StatusCode":0,"StatusMessage":"success","code":0,"data":{},"msg":"success"}`))
	}))
	defer feishu.Close()

	s := &server{
		feishuWebhook: feishu.URL,
		token:         "secret",
		client:        feishu.Client(),
	}

	payload := `{
	  "status": "firing",
	  "commonLabels": {"alertname": "MatrixAPIErrorLogsDetected", "service": "matrix-api", "env": "prod"},
	  "commonAnnotations": {"summary": "ERROR 日志出现", "description": "最近 5 分钟 ERROR > 0"},
	  "alerts": [{"status": "firing", "labels": {"alertname": "MatrixAPIErrorLogsDetected"}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	req.Header.Set("X-Forwarder-Token", "secret")
	rec := httptest.NewRecorder()

	s.handleGrafanaWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got.MsgType != "interactive" {
		t.Fatalf("forwarded msg type = %q, want interactive", got.MsgType)
	}
	if !strings.Contains(got.Card.Header.Title.Content, "MatrixAPIErrorLogsDetected") {
		t.Fatalf("forwarded card missing alert name: %#v", got.Card.Header.Title)
	}
}

func TestHandleGrafanaWebhookForwardsToFeishuApp(t *testing.T) {
	var sent map[string]string
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			if r.URL.Query().Get("receive_id_type") != "chat_id" {
				t.Fatalf("receive_id_type = %q", r.URL.Query().Get("receive_id_type"))
			}
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Fatalf("authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_123"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:     "app-id",
		feishuAppSecret: "app-secret",
		feishuChatID:    "chat-id",
		feishuAPIBase:   feishu.URL,
		token:           "secret",
		client:          feishu.Client(),
	}

	payload := `{
	  "status": "firing",
	  "commonLabels": {"alertname": "MatrixAPIErrorLogsDetected", "service": "matrix-api", "env": "prod"},
	  "commonAnnotations": {"summary": "ERROR 日志出现", "description": "最近 5 分钟 ERROR > 0"},
	  "alerts": [{"status": "firing", "labels": {"alertname": "MatrixAPIErrorLogsDetected"}, "panelURL":"https://grafana.example.com/panel"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	req.Header.Set("X-Forwarder-Token", "secret")
	rec := httptest.NewRecorder()

	s.handleGrafanaWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if sent["receive_id"] != "chat-id" || sent["msg_type"] != "interactive" {
		t.Fatalf("unexpected send body: %#v", sent)
	}
	if !strings.Contains(sent["content"], `"value"`) {
		t.Fatalf("app card should contain callback value: %s", sent["content"])
	}
}

func TestHandleGrafanaWebhookRoutesP1ToDedicatedChat(t *testing.T) {
	var sent []map[string]string
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent = append(sent, body)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_123"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:     "app-id",
		feishuAppSecret: "app-secret",
		feishuChatID:    "default-chat",
		feishuP1ChatID:  "p1-chat",
		feishuAPIBase:   feishu.URL,
		token:           "secret",
		client:          feishu.Client(),
	}

	for _, tc := range []struct {
		name      string
		severity  string
		wantChat  string
		alertName string
	}{
		{name: "p1", severity: "P1", wantChat: "p1-chat", alertName: "MatrixAPI5xxDetected"},
		{name: "p0", severity: "P0", wantChat: "default-chat", alertName: "MatrixAPICriticalIncident"},
		{name: "p2", severity: "P2", wantChat: "p1-chat", alertName: "MatrixAPIOrderFailureRateP2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{
			  "status": "firing",
			  "commonLabels": {"alertname": %q, "service": "matrix-api", "env": "prod", "severity": %q},
			  "commonAnnotations": {"summary": "demo", "description": "demo"},
			  "alerts": [{"status": "firing", "labels": {"alertname": %q}, "panelURL":"https://grafana.example.com/panel"}]
			}`, tc.alertName, tc.severity, tc.alertName)
			req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
			req.Header.Set("X-Forwarder-Token", "secret")
			rec := httptest.NewRecorder()

			before := len(sent)
			s.handleGrafanaWebhook(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if len(sent) != before+1 {
				t.Fatalf("send count = %d, want %d", len(sent), before+1)
			}
			if sent[len(sent)-1]["receive_id"] != tc.wantChat {
				t.Fatalf("receive_id = %q, want %q", sent[len(sent)-1]["receive_id"], tc.wantChat)
			}
		})
	}
}

func TestHandleGrafanaWebhookDedupMissingFeishuMessageSendsThrottledRepeat(t *testing.T) {
	var sendCount int
	var lastBody string
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sendCount++
		body, _ := io.ReadAll(r.Body)
		lastBody = string(body)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer feishu.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/alert/v1/silences/") {
			_, _ = w.Write([]byte(`{"active":false}`))
			return
		}
		if r.URL.Path != "/alert/v1/incidents:upsert" {
			t.Fatalf("unexpected backend path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"incident":{
				"id":"5",
				"fingerprint":"fp-5xx",
				"alertname":"MatrixAPI5xxDetected",
				"service":"matrix-api",
				"env":"prod",
				"severity":"P1",
				"fireCount":274,
				"lastFiredAt":"2026-05-18T18:27:10+08:00"
			},
			"dedup":true
		}`))
	}))
	defer backend.Close()

	s := &server{
		feishuWebhook: feishu.URL,
		client:        feishu.Client(),
		backend: &IncidentBackend{
			BaseURL:    backend.URL,
			HTTPClient: backend.Client(),
			Timeout:    time.Second,
		},
		dedupNotifyInterval: time.Hour,
	}

	payload := `{
	  "status": "firing",
	  "commonLabels": {"alertname": "MatrixAPI5xxDetected", "service": "matrix-api", "env": "prod", "severity": "P1"},
	  "commonAnnotations": {"summary": "matrix-api 出现 5xx", "description": "最近 5 分钟 5xx > 0"},
	  "alerts": [{"status": "firing", "labels": {"alertname": "MatrixAPI5xxDetected", "route": "/v1/callback/apply/pay/notification", "status": "500"}}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleGrafanaWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if sendCount != 1 {
		t.Fatalf("dedup with missing feishu msg should send repeat card once, got %d sends", sendCount)
	}
	if !strings.Contains(lastBody, "重复触发") || !strings.Contains(lastBody, "累计触发 274 次") {
		t.Fatalf("repeat card should explain dedup state, body=%s", lastBody)
	}

	req = httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	rec = httptest.NewRecorder()
	s.handleGrafanaWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if sendCount != 1 {
		t.Fatalf("second dedup inside interval should be throttled, got %d sends", sendCount)
	}
	if !strings.Contains(rec.Body.String(), "deduped") {
		t.Fatalf("second response should be deduped, body=%s", rec.Body.String())
	}
}

func TestHandleGrafanaWebhookResolvedReleasesBackendDedup(t *testing.T) {
	resolveCalls := 0
	matchCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/alert/v1/silences/") {
			matchCalls++
			_, _ = w.Write([]byte(`{"active":false}`))
			return
		}
		if r.URL.Path != "/alert/v1/incidents:resolve_by_fingerprint" {
			t.Fatalf("resolved webhook must release dedup by fingerprint, got %s", r.URL.Path)
		}
		resolveCalls++
		var got map[string]string
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		wantFP := computeFingerprint("UserSrvVerifyTokenHighErrorRate", "user-srv", "prod", map[string]string{
			"alertname": "UserSrvVerifyTokenHighErrorRate",
			"service":   "user-srv",
			"env":       "prod",
			"severity":  "P0",
		})
		if got["fingerprint"] != wantFP {
			t.Fatalf("fingerprint = %q, want %q", got["fingerprint"], wantFP)
		}
		_, _ = w.Write([]byte(`{"incident":{"id":"30","fingerprint":"` + wantFP + `"},"resolved":true}`))
	}))
	defer backend.Close()

	var sentBody string
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sentBody = string(body)
		_, _ = w.Write([]byte(`{"StatusCode":0,"StatusMessage":"success","code":0,"data":{},"msg":"success"}`))
	}))
	defer feishu.Close()

	s := &server{
		feishuWebhook: feishu.URL,
		client:        feishu.Client(),
		backend: &IncidentBackend{
			BaseURL:    backend.URL,
			HTTPClient: backend.Client(),
			Timeout:    time.Second,
		},
		emergencyAssignees: []string{"ou_fallback"},
	}

	payload := `{
	  "status": "resolved",
	  "commonLabels": {"alertname": "UserSrvVerifyTokenHighErrorRate", "service": "user-srv", "env": "prod", "severity": "P0"},
	  "commonAnnotations": {"summary": "VerifyToken recovered", "description": "恢复通知不应计入触发次数"},
	  "alerts": [{"status": "resolved", "labels": {"alertname": "UserSrvVerifyTokenHighErrorRate"}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	s.handleGrafanaWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if resolveCalls != 1 || matchCalls != 1 {
		t.Fatalf("backend resolve/match calls = %d/%d, want 1/1", resolveCalls, matchCalls)
	}
	if strings.Contains(sentBody, "重复触发") || strings.Contains(sentBody, "incident #") || strings.Contains(sentBody, "ou_fallback") {
		t.Fatalf("resolved card should not look like incident repeat/assignment card: %s", sentBody)
	}
}

func TestCardActionsResolvedDoesNotExposeWriteCallbacks(t *testing.T) {
	msg := buildFeishuCard(grafanaWebhook{
		Status:       "resolved",
		CommonLabels: map[string]string{"alertname": "UserSrvVerifyTokenHighErrorRate", "service": "user-srv", "env": "prod", "severity": "P0"},
		Alerts: []grafanaAlert{{
			Status:   "resolved",
			Labels:   map[string]string{"alertname": "UserSrvVerifyTokenHighErrorRate"},
			PanelURL: "https://grafana.example.com/panel",
		}},
	}, true, cardContext{IncidentID: 28, AssigneeOpenID: "ou_a"})
	body, _ := json.Marshal(msg)
	for _, forbidden := range []string{`"action":"claim"`, `"action":"resolve"`, `"action":"discard"`, `"action":"copilot_analyze"`} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("resolved card should not include write callback %s: %s", forbidden, string(body))
		}
	}
	if !strings.Contains(string(body), "打开大盘") {
		t.Fatalf("resolved card should keep dashboard link: %s", string(body))
	}
}

func TestCardActionsActiveIncludesSilenceCallback(t *testing.T) {
	msg := buildFeishuCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "UserSrvVerifyTokenHighErrorRate",
			"service":   "user-srv",
			"env":       "prod",
			"severity":  "P0",
		},
		Alerts: []grafanaAlert{{
			Status: "firing",
			Labels: map[string]string{"alertname": "UserSrvVerifyTokenHighErrorRate"},
		}},
	}, true, cardContext{IncidentID: 30, AssigneeOpenID: "ou_a"})
	body, _ := json.Marshal(msg)
	for _, want := range []string{
		"select_static", "选择屏蔽时长",
		"屏蔽10分钟", `"value":"10m"`,
		"屏蔽30分钟", `"value":"30m"`,
		"屏蔽4小时", `"value":"4h"`,
		"屏蔽6小时", `"value":"6h"`,
		"屏蔽12小时", `"value":"12h"`,
		"屏蔽24小时", `"value":"24h"`,
		"silence_alert", "fingerprint",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("active card missing silence marker %q: %s", want, string(body))
		}
	}
	if strings.Count(string(body), `"action":"silence_alert"`) != 1 {
		t.Fatalf("silence action should be represented by one dropdown callback: %s", string(body))
	}
}

func TestCardActionsActiveIncludesLongSilenceOptionsForNonP0(t *testing.T) {
	msg := buildFeishuCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "MatrixAPI5xxDetected",
			"service":   "matrix-api",
			"env":       "prod",
			"severity":  "P1",
		},
		Alerts: []grafanaAlert{{
			Status: "firing",
			Labels: map[string]string{"alertname": "MatrixAPI5xxDetected"},
		}},
	}, true, cardContext{IncidentID: 42, AssigneeOpenID: "ou_a"})
	body, _ := json.Marshal(msg)
	for _, want := range []string{
		"select_static", "选择屏蔽时长",
		"屏蔽30分钟", `"value":"30m"`,
		"屏蔽2小时", `"value":"2h"`,
		"屏蔽4小时", `"value":"4h"`,
		"屏蔽12小时", `"value":"12h"`,
		"屏蔽24小时", `"value":"24h"`,
		"屏蔽2天", `"value":"48h"`,
		"屏蔽7天", `"value":"168h"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("active card missing non-P0 silence marker %q: %s", want, string(body))
		}
	}
	if strings.Count(string(body), `"action":"silence_alert"`) != 1 {
		t.Fatalf("silence action should be represented by one dropdown callback: %s", string(body))
	}
}

func TestHandleGrafanaWebhookRejectsBadToken(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	s.handleGrafanaWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleGrafanaWebhookBackendSilenceSkipsSendAndUpsert(t *testing.T) {
	payload := `{
	  "status": "firing",
	  "commonLabels": {"alertname": "UserSrvVerifyTokenHighErrorRate", "service": "user-srv", "env": "prod", "severity": "P0"},
	  "commonAnnotations": {"summary": "VerifyToken high error rate", "description": "too noisy"},
	  "alerts": [{"status": "firing", "labels": {"alertname": "UserSrvVerifyTokenHighErrorRate"}}]
	}`
	var parsed grafanaWebhook
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	match := alertSilenceMatchFromPayload(parsed)

	sendCount := 0
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sendCount++
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer feishu.Close()
	backendCalls := 0
	expiresAt := time.Now().Add(time.Hour).UTC()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/alert/v1/silences/"+match.Fingerprint+":match" {
			t.Fatalf("unexpected backend request method=%s path=%s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"active":true,
			"silence":{
				"fingerprint":"` + match.Fingerprint + `",
				"alertname":"` + match.AlertName + `",
				"service":"` + match.Service + `",
				"env":"` + match.Env + `",
				"severity":"` + match.Severity + `",
				"operatorOpenId":"ou_operator",
				"expiresAt":"` + expiresAt.Format(time.RFC3339) + `"
			}
		}`))
	}))
	defer backend.Close()

	s := &server{
		feishuWebhook: feishu.URL,
		client:        feishu.Client(),
		backend: &IncidentBackend{
			BaseURL:    backend.URL,
			HTTPClient: backend.Client(),
			Timeout:    time.Second,
		},
		alertSilences: newAlertSilenceStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleGrafanaWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"silenced"`) {
		t.Fatalf("expected silenced response, got %s", rec.Body.String())
	}
	if sendCount != 0 || backendCalls != 1 {
		t.Fatalf("sendCount=%d backendCalls=%d, want 0/1", sendCount, backendCalls)
	}
}

func TestHandleGrafanaWebhookSilencedResolvedSkipsSendButResolvesBackend(t *testing.T) {
	payload := `{
	  "status": "resolved",
	  "commonLabels": {"alertname": "UserSrvVerifyTokenHighErrorRate", "service": "user-srv", "env": "prod", "severity": "P0"},
	  "commonAnnotations": {"summary": "VerifyToken high error rate", "description": "recovered"},
	  "alerts": [{"status": "resolved", "labels": {"alertname": "UserSrvVerifyTokenHighErrorRate", "service": "user-srv", "env": "prod", "severity": "P0"}}]
	}`
	var parsed grafanaWebhook
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	match := alertSilenceMatchFromPayload(parsed)

	sendCount := 0
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sendCount++
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer feishu.Close()
	backendCalls := 0
	expiresAt := time.Now().Add(time.Hour).UTC()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alert/v1/incidents:resolve_by_fingerprint":
			_, _ = w.Write([]byte(`{"incident":{"id":"42","fingerprint":"` + match.Fingerprint + `"},"resolved":true}`))
		case "/alert/v1/silences/" + match.Fingerprint + ":match":
			_, _ = w.Write([]byte(`{
				"active":true,
				"silence":{
					"fingerprint":"` + match.Fingerprint + `",
					"alertname":"` + match.AlertName + `",
					"service":"` + match.Service + `",
					"env":"` + match.Env + `",
					"severity":"` + match.Severity + `",
					"operatorOpenId":"ou_operator",
					"expiresAt":"` + expiresAt.Format(time.RFC3339) + `"
				}
			}`))
		default:
			t.Fatalf("unexpected backend path=%s", r.URL.Path)
		}
	}))
	defer backend.Close()

	s := &server{
		feishuWebhook: feishu.URL,
		client:        feishu.Client(),
		backend: &IncidentBackend{
			BaseURL:    backend.URL,
			HTTPClient: backend.Client(),
			Timeout:    time.Second,
		},
		alertSilences: newAlertSilenceStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleGrafanaWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"silenced"`) {
		t.Fatalf("expected silenced response, got %s", rec.Body.String())
	}
	if sendCount != 0 || backendCalls != 2 {
		t.Fatalf("sendCount=%d backendCalls=%d, want 0/2", sendCount, backendCalls)
	}
}

func TestHandleGrafanaWebhookExpiredSilenceFallsThrough(t *testing.T) {
	payload := `{
	  "status": "firing",
	  "commonLabels": {"alertname": "MatrixAPI5xxDetected", "service": "matrix-api", "env": "prod", "severity": "P1"},
	  "commonAnnotations": {"summary": "5xx", "description": "server errors"},
	  "alerts": [{"status": "firing", "labels": {"alertname": "MatrixAPI5xxDetected"}}]
	}`
	var parsed grafanaWebhook
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	match := alertSilenceMatchFromPayload(parsed)

	now := time.Date(2026, 5, 25, 14, 0, 0, 0, time.Local)
	store := newAlertSilenceStore()
	store.now = func() time.Time { return now }
	if _, err := store.Put(alertSilence{
		Fingerprint: match.Fingerprint,
		AlertName:   match.AlertName,
		Service:     match.Service,
		Env:         match.Env,
		Severity:    match.Severity,
		ExpiresAt:   now.Add(30 * time.Minute),
	}); err != nil {
		t.Fatalf("put silence: %v", err)
	}
	now = now.Add(31 * time.Minute)

	sendCount := 0
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sendCount++
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer feishu.Close()
	s := &server{
		feishuWebhook: feishu.URL,
		client:        feishu.Client(),
		alertSilences: store,
	}

	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleGrafanaWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) || sendCount != 1 {
		t.Fatalf("expired silence should fall through, body=%s sendCount=%d", rec.Body.String(), sendCount)
	}
}

func TestValidForwarderTokenAcceptsBearer(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/grafana/feishu", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")

	if !s.validForwarderToken(req) {
		t.Fatal("bearer token should be accepted")
	}
}

func TestHandleFeishuEventsURLVerification(t *testing.T) {
	s := &server{feishuVerificationToken: "verify-token"}
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(`{
	  "challenge": "challenge-value",
	  "token": "verify-token",
	  "type": "url_verification"
	}`))
	rec := httptest.NewRecorder()

	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"challenge":"challenge-value"`) {
		t.Fatalf("unexpected challenge response: %s", rec.Body.String())
	}
}

func TestHandleFeishuEventsEncryptedURLVerification(t *testing.T) {
	encryptKey := "test-encrypt-key"
	plain := []byte(`{"challenge":"encrypted-challenge","token":"verify-token","type":"url_verification"}`)
	encrypted := encryptFeishuEventForTest(t, plain, encryptKey)

	s := &server{
		feishuVerificationToken: "verify-token",
		feishuEncryptKey:        encryptKey,
	}
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(fmt.Sprintf(`{"encrypt":%q}`, encrypted)))
	rec := httptest.NewRecorder()

	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"challenge":"encrypted-challenge"`) {
		t.Fatalf("unexpected challenge response: %s", rec.Body.String())
	}
}

func TestHandleFeishuEventsCardActionClaimReplies(t *testing.T) {
	var reply map[string]any
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages/om_123/reply":
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Fatalf("authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				t.Fatalf("decode reply body: %v", err)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuChatID:            "chat-id",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_123","open_id":"ou_123"},
	    "context":{"open_message_id":"om_123","open_chat_id":"oc_123"},
	    "action":{"tag":"button","value":{"action":"claim","alertname":"MatrixAPIErrorLogsDetected","link":"https://grafana.example.com/panel"}}
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"已记录认领"`) {
		t.Fatalf("unexpected callback response: %s", rec.Body.String())
	}
	if reply["msg_type"] != "text" || reply["reply_in_thread"] != true {
		t.Fatalf("unexpected reply body: %#v", reply)
	}
	replyContent := fmt.Sprint(reply["content"])
	if !strings.Contains(replyContent, "ou_123") || !strings.Contains(replyContent, "已认领处理") {
		t.Fatalf("reply missing operator: %#v", reply)
	}
}

func TestHandleDirtyWorkLaunchSendsInputCard(t *testing.T) {
	var sent map[string]any
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			if got := r.URL.Query().Get("receive_id_type"); got != "chat_id" {
				t.Fatalf("receive_id_type=%q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_launcher"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:        "app-id",
		feishuAppSecret:    "app-secret",
		feishuChatID:       "default-chat",
		feishuAPIBase:      feishu.URL,
		client:             feishu.Client(),
		token:              "secret",
		dirtyWorkRecordURL: "https://example.feishu.cn/base/dirty-work-records",
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/dirty_work/launcher", strings.NewReader(`{
	  "chat_id":"oc_dirty",
	  "candidates":[{"name":"Bob","open_id":"ou_liu"}]
	}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleDirtyWorkLaunch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if sent["receive_id"] != "oc_dirty" || sent["msg_type"] != "interactive" {
		t.Fatalf("unexpected send body: %#v", sent)
	}
	content := fmt.Sprint(sent["content"])
	for _, want := range []string{
		"后端待处理问题", "dirty_work_task", "轮询分配", "Bob", "ou_liu",
		"assignee_open_id", "dirty_work_assign_ou_liu", "点候选人直接指定",
		"查看分配记录", "https://example.feishu.cn/base/dirty-work-records",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("launcher card missing %q: %s", want, content)
		}
	}
}

func TestHandleFeishuMessageDirtyWorkCommandSendsLauncher(t *testing.T) {
	sentCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			if got := r.URL.Query().Get("receive_id_type"); got != "chat_id" {
				t.Fatalf("receive_id_type=%q", got)
			}
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = r.URL.Query().Get("receive_id_type")
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_launcher"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	t.Setenv("DIRTY_WORK_CANDIDATES", "Alice|ou_alice,Bob|ou_bob,Carol|ou_carol")
	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
		dirtyWorkRecordURL:      "https://example.feishu.cn/base/dirty-work-records",
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"im.message.receive_v1","token":"verify-token"},
	  "event":{
	    "sender":{"sender_id":{"open_id":"ou_operator","user_id":"user_123"}},
	    "message":{"chat_id":"oc_dirty","chat_type":"group","message_type":"text","content":"{\"text\":\"@_user_1 /杂活\"}"}
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sent := findSentFeishuMessage(t, collectSentFeishuMessages(t, sentCh, 1), "chat_id", "oc_dirty")
	if sent["receive_id"] != "oc_dirty" || sent["msg_type"] != "interactive" {
		t.Fatalf("unexpected send body: %#v", sent)
	}
	content := fmt.Sprint(sent["content"])
	for _, want := range []string{
		"后端待处理问题", "dirty_work_task", "轮询分配", "点候选人直接指定",
		"Alice", "Bob", "Carol", "查看分配记录", "https://example.feishu.cn/base/dirty-work-records",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("launcher card missing %q: %s", want, content)
		}
	}
}

func TestHandleFeishuMessageDirtyWorkCommandDedupesRetry(t *testing.T) {
	sentCh := make(chan map[string]any, 2)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = r.URL.Query().Get("receive_id_type")
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_launcher"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
		feishuEventDedupTTL:     30 * time.Minute,
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_id":"evt_dirty_1","event_type":"im.message.receive_v1","token":"verify-token"},
	  "event":{
	    "sender":{"sender_id":{"open_id":"ou_operator","user_id":"user_123"}},
	    "message":{"message_id":"om_cmd_1","chat_id":"oc_dirty","chat_type":"group","message_type":"text","content":"{\"text\":\"@_user_1 /杂活\"}"}
	  }
	}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
		rec := httptest.NewRecorder()
		s.handleFeishuEvents(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body = %s", i+1, rec.Code, rec.Body.String())
		}
	}

	_ = findSentFeishuMessage(t, collectSentFeishuMessages(t, sentCh, 1), "chat_id", "oc_dirty")
	select {
	case extra := <-sentCh:
		t.Fatalf("duplicate event sent another launcher: %#v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHandleUserFeedbackOncallMessageRepliesWithDailyAssignee(t *testing.T) {
	var reply map[string]any
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages/om_feedback/reply":
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Fatalf("authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				t.Fatalf("decode reply body: %v", err)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:                   "app-id",
		feishuAppSecret:               "app-secret",
		feishuAPIBase:                 feishu.URL,
		client:                        feishu.Client(),
		userFeedbackOncallChatID:      defaultUserFeedbackOncallChatID,
		userFeedbackOncallCandidates:  []string{"ou_a", "ou_b", "ou_c"},
		userFeedbackOncallReplyPrefix: "收到新的用户反馈，请跟进",
	}
	event := json.RawMessage(`{
	  "sender":{"sender_id":{"open_id":"ou_b","user_id":"user_123"}},
	  "message":{"message_id":"om_feedback","chat_id":"oc_example_feedback","chat_type":"group","message_type":"text","content":"{\"text\":\"用户反馈支付失败\"}"}
	}`)
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))

	if err := s.handleUserFeedbackOncallMessageEvent(context.Background(), event, now); err != nil {
		t.Fatalf("handle user feedback oncall: %v", err)
	}
	if reply["msg_type"] != "text" || reply["reply_in_thread"] != true {
		t.Fatalf("unexpected reply body: %#v", reply)
	}
	wantAssignee := pickUserFeedbackOncallAssignee(s.userFeedbackOncallCandidates, now)
	var replyContent struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(fmt.Sprint(reply["content"])), &replyContent); err != nil {
		t.Fatalf("decode reply content: %v", err)
	}
	for _, want := range []string{"收到新的用户反馈，请跟进", "值班同学：后端值班"} {
		if !strings.Contains(replyContent.Text, want) {
			t.Fatalf("reply missing %q: %#v", want, reply)
		}
	}
	if strings.Contains(replyContent.Text, "<at ") || strings.Contains(replyContent.Text, wantAssignee) {
		t.Fatalf("reply should not directly at users or expose open_id: %s", replyContent.Text)
	}
}

func TestHandleUserFeedbackOncallMessageMentionsThreadOnce(t *testing.T) {
	replyCount := 0
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages/om_feedback_1/reply":
			replyCount++
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:                   "app-id",
		feishuAppSecret:               "app-secret",
		feishuAPIBase:                 feishu.URL,
		client:                        feishu.Client(),
		userFeedbackOncallChatID:      defaultUserFeedbackOncallChatID,
		userFeedbackOncallCandidates:  []string{"ou_oncall"},
		userFeedbackOncallReplyPrefix: "收到新的用户反馈，请跟进",
		userFeedbackOncallMentionTTL:  24 * time.Hour,
		userFeedbackOncallMentionAt:   map[string]time.Time{},
	}
	first := json.RawMessage(`{
	  "sender":{"sender_id":{"open_id":"ou_user"}},
	  "message":{"message_id":"om_feedback_1","thread_id":"omt_card_thread","chat_id":"oc_example_feedback","chat_type":"group","message_type":"text","content":"{\"text\":\"第一条\"}"}
	}`)
	second := json.RawMessage(`{
	  "sender":{"sender_id":{"open_id":"ou_user"}},
	  "message":{"message_id":"om_feedback_2","root_id":"om_feedback_1","chat_id":"oc_example_feedback","chat_type":"group","message_type":"text","content":"{\"text\":\"第二条\"}"}
	}`)
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if err := s.handleUserFeedbackOncallMessageEvent(context.Background(), first, now); err != nil {
		t.Fatalf("first oncall event: %v", err)
	}
	if err := s.handleUserFeedbackOncallMessageEvent(context.Background(), second, now.Add(time.Minute)); err != nil {
		t.Fatalf("second oncall event: %v", err)
	}
	if replyCount != 1 {
		t.Fatalf("reply count = %d, want 1", replyCount)
	}
}

func TestHandleUserFeedbackOncallMessageIgnoresOtherChat(t *testing.T) {
	requests := 0
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Fatalf("unexpected feishu request: %s", r.URL.String())
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:                   "app-id",
		feishuAppSecret:               "app-secret",
		feishuAPIBase:                 feishu.URL,
		client:                        feishu.Client(),
		userFeedbackOncallChatID:      defaultUserFeedbackOncallChatID,
		userFeedbackOncallCandidates:  []string{"ou_oncall"},
		userFeedbackOncallReplyPrefix: "收到新的用户反馈，请跟进",
	}
	otherChat := json.RawMessage(`{
	  "sender":{"sender_id":{"open_id":"ou_user"}},
	  "message":{"message_id":"om_other","chat_id":"oc_other","chat_type":"group","message_type":"text","content":"{\"text\":\"hello\"}"}
	}`)
	if err := s.handleUserFeedbackOncallMessageEvent(context.Background(), otherChat, time.Now()); err != nil {
		t.Fatalf("other chat should be ignored: %v", err)
	}
	if requests != 0 {
		t.Fatalf("unexpected feishu requests: %d", requests)
	}
}

func TestHandleFeishuMessageDirtyWorkCommandLoadsBitableCandidates(t *testing.T) {
	sentCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/bitable/v1/apps/app-token/tables/tbl_candidates/records":
			if got := r.URL.Query().Get("user_id_type"); got != "open_id" {
				t.Fatalf("user_id_type=%q", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"items":[
				{"record_id":"rec_1","fields":{"姓名":"表格候选人","open_id":"ou_bitable","启用":true,"权重":3}},
				{"record_id":"rec_2","fields":{"姓名":"停用候选人","open_id":"ou_disabled","启用":false}}
			]}}`))
		case "/open-apis/im/v1/messages":
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = r.URL.Query().Get("receive_id_type")
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_launcher"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
	}
	s.dirtyWorkBitable = &dirtyWorkBitableClient{
		server: s,
		cfg: dirtyWorkBitableConfig{
			AppToken:                  "app-token",
			CandidateTableID:          "tbl_candidates",
			CandidateNameField:        "姓名",
			CandidateOpenIDField:      "open_id",
			CandidateEnabledField:     "启用",
			CandidateWeightField:      "权重",
			RecordTaskField:           "任务内容",
			RecordOperatorField:       "发起人",
			RecordOperatorOpenIDField: "发起人OpenID",
			RecordAssigneeField:       "负责人",
			RecordAssigneeOpenIDField: "负责人OpenID",
		},
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"im.message.receive_v1","token":"verify-token"},
	  "event":{
	    "sender":{"sender_id":{"open_id":"ou_operator","user_id":"user_123"}},
	    "message":{"chat_id":"oc_dirty","chat_type":"group","message_type":"text","content":"{\"text\":\"@_user_1 /杂活\"}"}
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sent := findSentFeishuMessage(t, collectSentFeishuMessages(t, sentCh, 1), "chat_id", "oc_dirty")
	content := fmt.Sprint(sent["content"])
	for _, want := range []string{"表格候选人", "ou_bitable", "表格候选人|ou_bitable|3"} {
		if !strings.Contains(content, want) {
			t.Fatalf("launcher card missing %q: %s", want, content)
		}
	}
	if strings.Contains(content, "ou_disabled") {
		t.Fatalf("launcher card should not include disabled candidate: %s", content)
	}
}

func TestIsDirtyWorkLaunchCommandStripsFeishuMentions(t *testing.T) {
	for _, text := range []string{
		"/杂",
		"杂",
		"/杂活",
		"杂活",
		"/待处理",
		"待处理",
		"@_user_1 /杂活",
		"@告警机器人 /杂",
		"@告警机器人 /杂活",
		"@告警机器人/杂活",
		`<at id="ou_bot"></at> 杂活分配`,
		"/后端待处理问题",
		"@告警机器人 后端待处理问题",
	} {
		if !isDirtyWorkLaunchCommand(text) {
			t.Fatalf("isDirtyWorkLaunchCommand(%q) = false, want true", text)
		}
	}
}

func TestDirtyWorkTaskPreviewTruncatesAndCollapsesWhitespace(t *testing.T) {
	longTask := "第一行需要先排查接口异常\n\n第二行补充：" + strings.Repeat("需要同步确认订单、用户、媒体三侧日志，并在群里回填处理结论。", 4)
	preview := dirtyWorkTaskPreview(longTask)
	if strings.Contains(preview, "\n") || strings.Contains(preview, "  ") {
		t.Fatalf("preview should collapse whitespace: %q", preview)
	}
	if !strings.HasSuffix(preview, "…") {
		t.Fatalf("preview should be truncated with ellipsis: %q", preview)
	}
	if len([]rune(strings.TrimSuffix(preview, "…"))) != dirtyWorkTaskPreviewLimit {
		t.Fatalf("preview length = %d, want %d: %q", len([]rune(strings.TrimSuffix(preview, "…"))), dirtyWorkTaskPreviewLimit, preview)
	}
}

func TestPickDirtyWorkCandidateRoundRobinUsesMemoryCursor(t *testing.T) {
	s := &server{}
	candidates := []dirtyWorkCandidate{
		{Name: "Alice", OpenID: "ou_fang"},
		{Name: "Bob", OpenID: "ou_liu"},
		{Name: "Carol", OpenID: "ou_jiaqiang"},
	}
	for _, want := range []string{"ou_fang", "ou_liu", "ou_jiaqiang", "ou_fang"} {
		got, err := s.pickDirtyWorkCandidate(context.Background(), candidates)
		if err != nil {
			t.Fatalf("pick dirty work candidate: %v", err)
		}
		if got.OpenID != want {
			t.Fatalf("picked %s, want %s", got.OpenID, want)
		}
	}
}

func TestNextDirtyWorkCandidateAfterWrapsToNextPerson(t *testing.T) {
	candidates := []dirtyWorkCandidate{
		{Name: "Alice", OpenID: "ou_fang"},
		{Name: "Bob", OpenID: "ou_liu"},
		{Name: "Carol", OpenID: "ou_jiaqiang"},
	}
	got, err := nextDirtyWorkCandidateAfter(candidates, "ou_liu")
	if err != nil {
		t.Fatalf("next dirty work candidate: %v", err)
	}
	if got.OpenID != "ou_jiaqiang" {
		t.Fatalf("picked %s, want ou_jiaqiang", got.OpenID)
	}
	got, err = nextDirtyWorkCandidateAfter(candidates, "ou_jiaqiang")
	if err != nil {
		t.Fatalf("next dirty work candidate wrap: %v", err)
	}
	if got.OpenID != "ou_fang" {
		t.Fatalf("picked %s, want ou_fang", got.OpenID)
	}
}

func TestDirtyWorkRotationCursorRestoresFromLatestRecord(t *testing.T) {
	now := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records":
			resp := map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items": []map[string]any{
						{
							"record_id": "rec_old",
							"fields": map[string]any{
								"任务内容": "旧任务",
								"负责人":  []map[string]any{{"id": "ou_fang", "name": "Alice"}},
								"创建时间": now.Add(-2 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_latest",
							"fields": map[string]any{
								"任务内容": "新任务",
								"负责人":  []map[string]any{{"id": "ou_liu", "name": "Bob"}},
								"创建时间": now.Add(-1 * time.Hour).UnixMilli(),
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:     "app-id",
		feishuAppSecret: "app-secret",
		feishuAPIBase:   feishu.URL,
		client:          feishu.Client(),
	}
	s.dirtyWorkBitable = &dirtyWorkBitableClient{
		server: s,
		cfg: dirtyWorkBitableConfig{
			AppToken:             "app-token",
			RecordTableID:        "tbl_records",
			RecordTaskField:      "任务内容",
			RecordAssigneeField:  "负责人",
			RecordCreatedAtField: "创建时间",
		},
	}
	got, err := s.pickDirtyWorkCandidate(context.Background(), []dirtyWorkCandidate{
		{Name: "Alice", OpenID: "ou_fang"},
		{Name: "Bob", OpenID: "ou_liu"},
		{Name: "Carol", OpenID: "ou_jiaqiang"},
	})
	if err != nil {
		t.Fatalf("pick dirty work candidate: %v", err)
	}
	if got.OpenID != "ou_jiaqiang" {
		t.Fatalf("picked %s, want ou_jiaqiang", got.OpenID)
	}
}

func TestHandleFeishuEventsDirtyWorkPickSendsResultCard(t *testing.T) {
	sentCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			receiveIDType := r.URL.Query().Get("receive_id_type")
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = receiveIDType
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_result"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuChatID:            "chat-id",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
		dirtyWorkRecordURL:      "https://example.feishu.cn/base/dirty-work-records",
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_123","open_id":"ou_operator"},
	    "context":{"open_message_id":"om_launcher","open_chat_id":"oc_dirty"},
	    "action":{
	      "tag":"button",
	      "form_value":{"dirty_work_task":"整理发布遗留事项"},
	      "value":{"action":"dirty_work_pick","candidates":"Bob|ou_liu"}
	    }
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "已分配给Bob") {
		t.Fatalf("unexpected callback response: %s", rec.Body.String())
	}
	for _, want := range []string{`"card":{"type":"raw","data":{"schema":"2.0"`, `"body":{"elements"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("callback response missing %q: %s", want, rec.Body.String())
		}
	}
	messages := collectSentFeishuMessages(t, sentCh, 1)
	dmMsg := findSentFeishuMessage(t, messages, "open_id", "ou_liu")
	content := rec.Body.String()
	for _, want := range []string{"任务预览", "整理发布遗留事项", "Bob", "ou_liu", "ou_operator", "查看分配记录", "https://example.feishu.cn/base/dirty-work-records"} {
		if !strings.Contains(content, want) {
			t.Fatalf("result card missing %q: %s", want, content)
		}
	}
	dmContent := fmt.Sprint(dmMsg["content"])
	for _, want := range []string{"后端待处理问题提醒", "任务预览", "整理发布遗留事项", "当前状态", "已分配", "我来处理", "已处理", "dirty_work_status", "ou_operator", "查看分配记录", "https://example.feishu.cn/base/dirty-work-records"} {
		if !strings.Contains(dmContent, want) {
			t.Fatalf("assignee dm missing %q: %s", want, dmContent)
		}
	}
}

func TestHandleFeishuEventsDirtyWorkPickDirectAssignee(t *testing.T) {
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_direct"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_123","open_id":"ou_operator"},
	    "context":{"open_message_id":"om_launcher","open_chat_id":"oc_dirty"},
	    "action":{
	      "tag":"button",
	      "form_value":{"dirty_work_task":"指定分给Carol"},
	      "value":{
	        "action":"dirty_work_pick",
	        "candidates":"Alice|ou_fang,Bob|ou_liu,Carol|ou_jiaqiang",
	        "assignee_open_id":"ou_jiaqiang",
	        "assignee_name":"Carol"
	      }
	    }
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "已分配给Carol") {
		t.Fatalf("unexpected callback response: %s", body)
	}
	if !strings.Contains(body, "ou_jiaqiang") {
		t.Fatalf("result card missing direct assignee: %s", body)
	}
	// 轮询若误用会分给首位Alice；直接点选必须落到Carol。
	if strings.Contains(body, "已分配给Alice") {
		t.Fatalf("direct pick fell back to round-robin: %s", body)
	}
}

func TestHandleFeishuEventsDirtyWorkPickCreatesBitableRecord(t *testing.T) {
	sentCh := make(chan map[string]any, 1)
	recordCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = r.URL.Query().Get("receive_id_type")
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_result"}}`))
		case "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode record body: %v", err)
			}
			recordCh <- body
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"record":{"record_id":"rec_created"}}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuChatID:            "chat-id",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
	}
	s.dirtyWorkBitable = &dirtyWorkBitableClient{
		server: s,
		cfg: dirtyWorkBitableConfig{
			AppToken:                  "app-token",
			RecordTableID:             "tbl_records",
			RecordTaskField:           "任务内容",
			RecordOperatorField:       "发起人",
			RecordOperatorOpenIDField: "发起人OpenID",
			RecordAssigneeField:       "负责人",
			RecordAssigneeOpenIDField: "负责人OpenID",
			RecordPreviousField:       "原负责人",
			RecordPreviousOpenIDField: "原负责人OpenID",
			RecordStatusField:         "状态",
			RecordCreatedAtField:      "创建时间",
		},
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_123","open_id":"ou_operator"},
	    "context":{"open_message_id":"om_launcher","open_chat_id":"oc_dirty"},
	    "action":{
	      "tag":"button",
	      "form_value":{"dirty_work_task":"整理发布遗留事项"},
	      "value":{"action":"dirty_work_pick","candidates":"Bob|ou_liu"}
	    }
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	messages := collectSentFeishuMessages(t, sentCh, 1)
	_ = findSentFeishuMessage(t, messages, "open_id", "ou_liu")
	var record map[string]any
	select {
	case record = <-recordCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bitable record")
	}
	fields, ok := record["fields"].(map[string]any)
	if !ok {
		t.Fatalf("record fields missing: %#v", record)
	}
	for key, want := range map[string]string{
		"任务内容": "整理发布遗留事项",
		"状态":   "已分配",
	} {
		if got := fmt.Sprint(fields[key]); got != want {
			t.Fatalf("record field %s=%q, want %q; fields=%#v", key, got, want, fields)
		}
	}
	if _, ok := fields["操作类型"]; ok {
		t.Fatalf("record should not write 操作类型 by default: %#v", fields)
	}
	if got := bitableUserFieldID(t, fields["发起人"]); got != "ou_operator" {
		t.Fatalf("发起人 user id=%q, want ou_operator; fields=%#v", got, fields)
	}
	if got := bitableUserFieldID(t, fields["负责人"]); got != "ou_liu" {
		t.Fatalf("负责人 user id=%q, want ou_liu; fields=%#v", got, fields)
	}
	if _, ok := fields["创建时间"]; !ok {
		t.Fatalf("record missing 创建时间: %#v", fields)
	}
}

func TestDirtyWorkTimeoutReminderSendsGroupCard(t *testing.T) {
	now := time.Date(2026, 6, 14, 23, 0, 0, 0, time.UTC)
	sentCh := make(chan map[string]any, 2)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records":
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected bitable method: %s", r.Method)
			}
			resp := map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items": []map[string]any{
						{
							"record_id": "rec_old_open",
							"fields": map[string]any{
								"任务内容": "旧任务需要同步",
								"状态":   "已分配",
								"负责人":  []map[string]any{{"id": "ou_liu", "name": "Bob"}},
								"创建时间": now.Add(-72 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_recent",
							"fields": map[string]any{
								"任务内容": "新任务不用提醒",
								"状态":   "已分配",
								"负责人":  []map[string]any{{"id": "ou_jiaqiang", "name": "Carol"}},
								"创建时间": now.Add(-24 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_done_old",
							"fields": map[string]any{
								"任务内容": "已经处理的任务",
								"状态":   "已分配",
								"负责人":  []map[string]any{{"id": "ou_done", "name": "已处理人"}},
								"创建时间": now.Add(-72 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_done_latest",
							"fields": map[string]any{
								"任务内容": "已经处理的任务",
								"状态":   "已处理",
								"负责人":  []map[string]any{{"id": "ou_done", "name": "已处理人"}},
								"创建时间": now.Add(-1 * time.Hour).UnixMilli(),
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/open-apis/im/v1/messages":
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = r.URL.Query().Get("receive_id_type")
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_timeout"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:        "app-id",
		feishuAppSecret:    "app-secret",
		feishuChatID:       "default-chat",
		feishuAPIBase:      feishu.URL,
		client:             feishu.Client(),
		dirtyWorkRecordURL: "https://example.feishu.cn/base/dirty-work-records",
		dirtyWorkReminder: dirtyWorkTimeoutReminderConfig{
			ChatID:   "oc_timeout",
			After:    48 * time.Hour,
			Interval: 10 * time.Minute,
			Cooldown: 6 * time.Hour,
		},
		dirtyWorkReminderAt: map[string]time.Time{},
	}
	s.dirtyWorkBitable = &dirtyWorkBitableClient{
		server: s,
		cfg: dirtyWorkBitableConfig{
			AppToken:             "app-token",
			RecordTableID:        "tbl_records",
			RecordTaskField:      "任务内容",
			RecordAssigneeField:  "负责人",
			RecordStatusField:    "状态",
			RecordCreatedAtField: "创建时间",
		},
	}
	if err := s.runDirtyWorkTimeoutReminderOnce(context.Background(), now); err != nil {
		t.Fatalf("run reminder: %v", err)
	}
	msg := findSentFeishuMessage(t, collectSentFeishuMessages(t, sentCh, 1), "chat_id", "oc_timeout")
	content := fmt.Sprint(msg["content"])
	for _, want := range []string{"后端待处理问题超时提醒", "旧任务需要同步", "Bob", "ou_liu", "查看分配记录"} {
		if !strings.Contains(content, want) {
			t.Fatalf("reminder card missing %q: %s", want, content)
		}
	}
	for _, notWant := range []string{"新任务不用提醒", "已经处理的任务"} {
		if strings.Contains(content, notWant) {
			t.Fatalf("reminder card should not contain %q: %s", notWant, content)
		}
	}

	if err := s.runDirtyWorkTimeoutReminderOnce(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatalf("run reminder again: %v", err)
	}
	select {
	case msg := <-sentCh:
		t.Fatalf("unexpected duplicate reminder: %#v", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDirtyWorkTopicReminderFiltersIssueAndSendsGroupCard(t *testing.T) {
	now := time.Date(2026, 6, 25, 11, 30, 0, 0, time.UTC)
	sentCh := make(chan map[string]any, 2)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records":
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected bitable method: %s", r.Method)
			}
			if fields := r.URL.Query().Get("field_names"); !strings.Contains(fields, "议题") {
				t.Fatalf("topic reminder should request issue/topic field, field_names=%s", fields)
			}
			resp := map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items": []map[string]any{
						{
							"record_id": "rec_topic_open",
							"fields": map[string]any{
								"议题":   "发布议题",
								"任务内容": "上线清单需要确认",
								"状态":   "已分配",
								"负责人":  []map[string]any{{"id": "ou_liu", "name": "Bob"}},
								"创建时间": now.Add(-3 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_topic_processing",
							"fields": map[string]any{
								"议题":   "发布议题",
								"任务内容": "回归问题需要同步",
								"状态":   "处理中",
								"负责人":  []map[string]any{{"id": "ou_jiaqiang", "name": "Carol"}},
								"创建时间": now.Add(-2 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_other_topic",
							"fields": map[string]any{
								"议题":   "其他议题",
								"任务内容": "其他议题不应提醒",
								"状态":   "已分配",
								"负责人":  []map[string]any{{"id": "ou_other", "name": "其他人"}},
								"创建时间": now.Add(-3 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_topic_done",
							"fields": map[string]any{
								"议题":   "发布议题",
								"任务内容": "已完成事项",
								"状态":   "已处理",
								"负责人":  []map[string]any{{"id": "ou_done", "name": "已处理人"}},
								"创建时间": now.Add(-3 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_topic_blocked",
							"fields": map[string]any{
								"议题":   "发布议题",
								"任务内容": "非开放状态不应提醒",
								"状态":   "暂缓",
								"负责人":  []map[string]any{{"id": "ou_blocked", "name": "暂缓人"}},
								"创建时间": now.Add(-3 * time.Hour).UnixMilli(),
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/open-apis/im/v1/messages":
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = r.URL.Query().Get("receive_id_type")
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_topic"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:        "app-id",
		feishuAppSecret:    "app-secret",
		feishuChatID:       "default-chat",
		feishuAPIBase:      feishu.URL,
		client:             feishu.Client(),
		dirtyWorkRecordURL: "https://example.feishu.cn/base/dirty-work-records",
		dirtyWorkTopicReminder: dirtyWorkTopicReminderConfig{
			Enabled:    true,
			ChatID:     "oc_topic",
			Interval:   time.Hour,
			Cooldown:   2 * time.Hour,
			TopicField: "议题",
			TopicValue: "发布议题",
			Statuses:   []string{"已分配", "处理中"},
			Title:      "发布议题定时提醒",
			MentionAll: true,
		},
		dirtyWorkTopicReminderAt: map[string]time.Time{},
	}
	s.dirtyWorkBitable = &dirtyWorkBitableClient{
		server: s,
		cfg: dirtyWorkBitableConfig{
			AppToken:             "app-token",
			RecordTableID:        "tbl_records",
			RecordTaskField:      "任务内容",
			RecordAssigneeField:  "负责人",
			RecordStatusField:    "状态",
			RecordCreatedAtField: "创建时间",
			RecordTopicField:     "议题",
		},
	}
	if err := s.runDirtyWorkTopicReminderOnce(context.Background(), now); err != nil {
		t.Fatalf("run topic reminder: %v", err)
	}
	msg := findSentFeishuMessage(t, collectSentFeishuMessages(t, sentCh, 1), "chat_id", "oc_topic")
	content := fmt.Sprint(msg["content"])
	for _, want := range []string{"发布议题定时提醒", "发布议题", "上线清单需要确认", "回归问题需要同步", "Bob", "ou_liu", "Carol", "ou_jiaqiang", "我已处理", "dirty_work_topic_done", "rec_topic_open", `id=\"all\"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("topic reminder card missing %q: %s", want, content)
		}
	}
	for _, notWant := range []string{"其他议题不应提醒", "已完成事项", "非开放状态不应提醒", "查看多维表格"} {
		if strings.Contains(content, notWant) {
			t.Fatalf("topic reminder card should not contain %q: %s", notWant, content)
		}
	}

	if err := s.runDirtyWorkTopicReminderOnce(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatalf("run topic reminder again: %v", err)
	}
	select {
	case msg := <-sentCh:
		t.Fatalf("unexpected duplicate topic reminder: %#v", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleDirtyWorkTopicDonePatchesBitableRecord(t *testing.T) {
	patchCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case r.Method == http.MethodPut && r.URL.Path == "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records/rec_topic_open":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			body["user_id_type"] = r.URL.Query().Get("user_id_type")
			patchCh <- body
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:     "app-id",
		feishuAppSecret: "app-secret",
		feishuAPIBase:   feishu.URL,
		client:          feishu.Client(),
	}
	s.dirtyWorkBitable = &dirtyWorkBitableClient{
		server: s,
		cfg: dirtyWorkBitableConfig{
			AppToken:                  "app-token",
			RecordTableID:             "tbl_records",
			RecordStatusField:         "状态",
			RecordActionField:         "动作",
			RecordOperatorField:       "处理人",
			RecordOperatorOpenIDField: "处理人 open_id",
		},
	}

	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_liu","open_id":"ou_liu"},
	  "context":{"open_message_id":"om_topic","open_chat_id":"oc_topic"},
	  "action":{
	    "tag":"button",
	    "value":{
	      "action":"dirty_work_topic_done",
	      "record_id":"rec_topic_open",
	      "task":"上线清单需要确认",
	      "assignee_open_id":"ou_liu",
	      "assignee_name":"Bob",
	      "status":"已处理"
	    }
	  }
	}`))
	if err != nil {
		t.Fatalf("handle topic done: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "已确认处理") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
	patch := <-patchCh
	fields, ok := patch["fields"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected patch fields: %#v", patch)
	}
	if got := fmt.Sprint(fields["状态"]); got != "已处理" {
		t.Fatalf("状态 = %q, want 已处理; patch=%#v", got, patch)
	}
	if got := fmt.Sprint(fields["动作"]); got != "卡片确认处理" {
		t.Fatalf("动作 = %q, want 卡片确认处理; patch=%#v", got, patch)
	}
	if got := fmt.Sprint(fields["处理人 open_id"]); got != "ou_liu" {
		t.Fatalf("处理人 open_id = %q, want ou_liu; patch=%#v", got, patch)
	}
	if got := patch["user_id_type"]; got != "open_id" {
		t.Fatalf("user_id_type = %q, want open_id", got)
	}
}

func TestHandleDirtyWorkTopicDoneRejectsNonAssignee(t *testing.T) {
	s := &server{}
	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_other","open_id":"ou_other"},
	  "context":{"open_message_id":"om_topic","open_chat_id":"oc_topic"},
	  "action":{
	    "tag":"button",
	    "value":{
	      "action":"dirty_work_topic_done",
	      "record_id":"rec_topic_open",
	      "task":"上线清单需要确认",
	      "assignee_open_id":"ou_liu",
	      "assignee_name":"Bob"
	    }
	  }
	}`))
	if err != nil {
		t.Fatalf("handle topic done: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "只有当前负责人") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
}

func TestDirtyWorkTopicReminderFromEnvCompactSpec(t *testing.T) {
	t.Setenv("DIRTY_WORK_TOPIC_REMINDER", "发布议题|oc_topic|30m|2h|true")

	cfg := dirtyWorkTopicReminderFromEnv()
	if !cfg.Enabled {
		t.Fatalf("compact topic reminder should enable config: %#v", cfg)
	}
	if cfg.TopicField != "议题" {
		t.Fatalf("TopicField = %q, want 议题", cfg.TopicField)
	}
	if cfg.TopicValue != "发布议题" {
		t.Fatalf("TopicValue = %q, want 发布议题", cfg.TopicValue)
	}
	if cfg.ChatID != "oc_topic" {
		t.Fatalf("ChatID = %q, want oc_topic", cfg.ChatID)
	}
	if cfg.Interval != 30*time.Minute {
		t.Fatalf("Interval = %s, want 30m", cfg.Interval)
	}
	if cfg.Cooldown != 2*time.Hour {
		t.Fatalf("Cooldown = %s, want 2h", cfg.Cooldown)
	}
	if !cfg.MentionAll {
		t.Fatalf("MentionAll = false, want true")
	}
	if got, want := strings.Join(cfg.Statuses, ","), "已分配,处理中"; got != want {
		t.Fatalf("Statuses = %q, want %q", got, want)
	}
	if cfg.Title != "发布议题定时提醒" {
		t.Fatalf("Title = %q, want 发布议题定时提醒", cfg.Title)
	}
}

func TestDirtyWorkTopicReminderFromEnvAdvancedOverridesCompactSpec(t *testing.T) {
	t.Setenv("DIRTY_WORK_TOPIC_REMINDER", "发布议题|oc_topic|30m")
	t.Setenv("DIRTY_WORK_TOPIC_REMINDER_FIELD", "模块")
	t.Setenv("DIRTY_WORK_TOPIC_REMINDER_VALUE", "支付模块")
	t.Setenv("DIRTY_WORK_TOPIC_REMINDER_CHAT_ID", "oc_override")
	t.Setenv("DIRTY_WORK_TOPIC_REMINDER_STATUSES", "待处理，处理中")
	t.Setenv("DIRTY_WORK_TOPIC_REMINDER_TITLE", "支付模块提醒")

	cfg := dirtyWorkTopicReminderFromEnv()
	if !cfg.Enabled {
		t.Fatalf("compact topic reminder should enable config: %#v", cfg)
	}
	if cfg.TopicField != "模块" || cfg.TopicValue != "支付模块" || cfg.ChatID != "oc_override" {
		t.Fatalf("overrides not applied: %#v", cfg)
	}
	if got, want := strings.Join(cfg.Statuses, ","), "待处理,处理中"; got != want {
		t.Fatalf("Statuses = %q, want %q", got, want)
	}
	if cfg.Title != "支付模块提醒" {
		t.Fatalf("Title = %q, want 支付模块提醒", cfg.Title)
	}
}

func bitableUserFieldID(t *testing.T, value any) string {
	t.Helper()
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("unexpected user field value: %#v", value)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected user field item: %#v", items[0])
	}
	return fmt.Sprint(first["id"])
}

func collectSentFeishuMessages(t *testing.T, ch <-chan map[string]any, n int) []map[string]any {
	t.Helper()
	messages := make([]map[string]any, 0, n)
	deadline := time.After(2 * time.Second)
	for len(messages) < n {
		select {
		case msg := <-ch:
			messages = append(messages, msg)
		case <-deadline:
			t.Fatalf("timed out waiting for %d feishu messages, got %d: %#v", n, len(messages), messages)
		}
	}
	return messages
}

func findSentFeishuMessage(t *testing.T, messages []map[string]any, receiveIDType, receiveID string) map[string]any {
	t.Helper()
	for _, msg := range messages {
		if fmt.Sprint(msg["receive_id_type"]) == receiveIDType && fmt.Sprint(msg["receive_id"]) == receiveID {
			return msg
		}
	}
	t.Fatalf("missing feishu message receive_id_type=%s receive_id=%s in %#v", receiveIDType, receiveID, messages)
	return nil
}

func TestHandleFeishuEventsDirtyWorkRepickSendsNextResultCard(t *testing.T) {
	sentCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			receiveIDType := r.URL.Query().Get("receive_id_type")
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode send body: %v", err)
			}
			sent["receive_id_type"] = receiveIDType
			sentCh <- sent
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_dirty_repick"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuChatID:            "chat-id",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_jiaqiang","open_id":"ou_jiaqiang"},
	    "context":{"open_message_id":"om_result","open_chat_id":"oc_dirty"},
	    "action":{
	      "tag":"button",
	      "value":{
	        "action":"dirty_work_repick",
	        "task":"吃饭",
	        "candidates":"Carol|ou_jiaqiang,Bob|ou_liu",
	        "assignee_open_id":"ou_jiaqiang",
	        "assignee_name":"Carol"
	      }
	    }
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "已重新分配给Bob") {
		t.Fatalf("unexpected callback response: %s", rec.Body.String())
	}
	for _, want := range []string{`"card":{"type":"raw","data":{"schema":"2.0"`, `"body":{"elements"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("callback response missing %q: %s", want, rec.Body.String())
		}
	}
	messages := collectSentFeishuMessages(t, sentCh, 1)
	dmMsg := findSentFeishuMessage(t, messages, "open_id", "ou_liu")
	content := rec.Body.String()
	for _, want := range []string{"任务预览", "吃饭", "Bob", "ou_liu", "Carol 没时间", "dirty_work_repick"} {
		if !strings.Contains(content, want) {
			t.Fatalf("repick result card missing %q: %s", want, content)
		}
	}
	dmContent := fmt.Sprint(dmMsg["content"])
	for _, want := range []string{"后端待处理问题提醒", "吃饭", "Carol 没时间", "ou_jiaqiang", "我来处理", "已处理", "没时间"} {
		if !strings.Contains(dmContent, want) {
			t.Fatalf("repick assignee dm missing %q: %s", want, dmContent)
		}
	}
}

func TestHandleDirtyWorkRepickRejectsNonAssignee(t *testing.T) {
	s := &server{}
	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_other","open_id":"ou_other"},
	  "context":{"open_message_id":"om_result","open_chat_id":"oc_dirty"},
	  "action":{
	    "tag":"button",
	    "value":{
	      "action":"dirty_work_repick",
	      "task":"吃饭",
	      "candidates":"Carol|ou_jiaqiang,Bob|ou_liu",
	      "assignee_open_id":"ou_jiaqiang",
	      "assignee_name":"Carol"
	    }
	  }
	}`))
	if err != nil {
		t.Fatalf("handle card action: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "只有当前负责人") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
}

func TestHandleDirtyWorkStatusUpdatesAssigneeDMCard(t *testing.T) {
	s := &server{dirtyWorkRecordURL: "https://example.feishu.cn/base/dirty-work-records"}
	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_liu","open_id":"ou_liu"},
	  "context":{"open_message_id":"om_dm","open_chat_id":"oc_dm"},
	  "action":{
	    "tag":"button",
	    "value":{
	      "action":"dirty_work_status",
	      "task":"整理发布遗留事项",
	      "status":"处理中",
	      "assignee_open_id":"ou_liu",
	      "assignee_name":"Bob",
	      "requester":"<at id=\"ou_operator\"></at>",
	      "candidates":"Bob|ou_liu,Carol|ou_jiaqiang",
	      "record_url":"https://example.feishu.cn/base/dirty-work-records",
	      "source_chat_id":"oc_dirty",
	      "source_message_id":"om_launcher"
	    }
	  }
	}`))
	if err != nil {
		t.Fatalf("handle card action: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "已更新为处理中") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	content := string(body)
	for _, want := range []string{`"card":{"type":"raw","data":{"schema":"2.0"`, "当前状态", "处理中", "已处理", "没时间", "dirty_work_repick", "整理发布遗留事项"} {
		if !strings.Contains(content, want) {
			t.Fatalf("status response missing %q: %s", want, content)
		}
	}
	if strings.Contains(content, "我来处理") {
		t.Fatalf("processing card should not keep 我来处理 button: %s", content)
	}
}

func TestHandleDirtyWorkStatusPatchesExistingBitableRecord(t *testing.T) {
	patchCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case r.Method == http.MethodGet && r.URL.Path == "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records":
			resp := map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items": []map[string]any{
						{
							"record_id": "rec_existing",
							"fields": map[string]any{
								"任务内容":      "整理发布遗留事项",
								"状态":        "处理中",
								"负责人":       []map[string]any{{"id": "ou_liu", "name": "Bob"}},
								"ChatID":    "oc_dirty",
								"MessageID": "om_launcher",
								"创建时间":      time.Date(2026, 6, 16, 13, 0, 0, 0, time.UTC).UnixMilli(),
							},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPut && r.URL.Path == "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records/rec_existing":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			patchCh <- body
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/bitable/v1/apps/app-token/tables/tbl_records/records":
			t.Fatal("status update must not create a new bitable record")
		default:
			t.Fatalf("unexpected path: %s %s", r.Method, r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:     "app-id",
		feishuAppSecret: "app-secret",
		feishuAPIBase:   feishu.URL,
		client:          feishu.Client(),
	}
	s.dirtyWorkBitable = &dirtyWorkBitableClient{
		server: s,
		cfg: dirtyWorkBitableConfig{
			AppToken:             "app-token",
			RecordTableID:        "tbl_records",
			RecordTaskField:      "任务内容",
			RecordAssigneeField:  "负责人",
			RecordStatusField:    "状态",
			RecordChatIDField:    "ChatID",
			RecordMessageIDField: "MessageID",
			RecordCreatedAtField: "创建时间",
		},
	}
	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_liu","open_id":"ou_liu"},
	  "context":{"open_message_id":"om_dm","open_chat_id":"oc_dm"},
	  "action":{
	    "tag":"button",
	    "value":{
	      "action":"dirty_work_status",
	      "task":"整理发布遗留事项",
	      "status":"已处理",
	      "assignee_open_id":"ou_liu",
	      "assignee_name":"Bob",
	      "source_chat_id":"oc_dirty",
	      "source_message_id":"om_launcher"
	    }
	  }
	}`))
	if err != nil {
		t.Fatalf("handle card action: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "已更新为已处理") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
	var patch map[string]any
	select {
	case patch = <-patchCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bitable patch")
	}
	fields, ok := patch["fields"].(map[string]any)
	if !ok {
		t.Fatalf("patch fields missing: %#v", patch)
	}
	if got := fmt.Sprint(fields["状态"]); got != "已处理" {
		t.Fatalf("patched 状态=%q, want 已处理; fields=%#v", got, fields)
	}
}

func TestHandleDirtyWorkStatusRejectsNonAssignee(t *testing.T) {
	s := &server{}
	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_other","open_id":"ou_other"},
	  "action":{
	    "tag":"button",
	    "value":{
	      "action":"dirty_work_status",
	      "task":"整理发布遗留事项",
	      "status":"已处理",
	      "assignee_open_id":"ou_liu",
	      "assignee_name":"Bob"
	    }
	  }
	}`))
	if err != nil {
		t.Fatalf("handle card action: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "只有当前负责人") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
}

func TestHandleDirtyWorkRepickMissingCurrentAssignee(t *testing.T) {
	s := &server{}
	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_other","open_id":"ou_other"},
	  "context":{"open_message_id":"om_result","open_chat_id":"oc_dirty"},
	  "action":{
	    "tag":"button",
	    "value":{
	      "action":"dirty_work_repick",
	      "task":"吃饭",
	      "candidates":"Carol|ou_jiaqiang,Bob|ou_liu"
	    }
	  }
	}`))
	if err != nil {
		t.Fatalf("handle card action: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "缺少当前负责人") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
}

func TestHandleFeishuEventsCardActionSilenceCreatesRule(t *testing.T) {
	var reply map[string]any
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages/om_123/reply":
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				t.Fatalf("decode reply body: %v", err)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	now := time.Date(2026, 5, 25, 14, 0, 0, 0, time.Local)
	store := newAlertSilenceStore()
	store.now = func() time.Time { return now }
	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuChatID:            "chat-id",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
		alertSilences:           store,
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_123","open_id":"ou_123"},
	    "context":{"open_message_id":"om_123","open_chat_id":"oc_123"},
	    "action":{"tag":"button","value":{"action":"silence_alert","fingerprint":"fp_silence","alertname":"MatrixAPI5xxDetected","service":"matrix-api","env":"prod","severity":"P1","duration":"2h","incident_id":"42"}}
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "已屏蔽2小时") {
		t.Fatalf("unexpected callback response: %s", rec.Body.String())
	}
	item, ok := store.Match("fp_silence")
	if !ok {
		t.Fatal("silence rule was not stored")
	}
	if item.OperatorOpenID != "ou_123" || item.AlertName != "MatrixAPI5xxDetected" {
		t.Fatalf("unexpected silence item: %#v", item)
	}
	if got := item.ExpiresAt.Sub(now); got != 2*time.Hour {
		t.Fatalf("duration = %s, want 2h", got)
	}
	replyContent := fmt.Sprint(reply["content"])
	if !strings.Contains(replyContent, "已屏蔽告警 2小时") || !strings.Contains(replyContent, "MatrixAPI5xxDetected") {
		t.Fatalf("reply missing audit text: %#v", reply)
	}
}

func TestHandleCardActionSilenceSelectUsesOptionDuration(t *testing.T) {
	now := time.Date(2026, 5, 25, 14, 0, 0, 0, time.Local)
	store := newAlertSilenceStore()
	store.now = func() time.Time { return now }
	s := &server{alertSilences: store}

	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_123","open_id":"ou_123"},
	  "action":{
	    "tag":"select_static",
	    "option":"12h",
	    "value":{"action":"silence_alert","fingerprint":"fp_select","alertname":"UserSrvVerifyTokenHighErrorRate","service":"user-srv","env":"prod","severity":"P0","incident_id":"30"}
	  }
	}`))
	if err != nil {
		t.Fatalf("handle card action: %v", err)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "已屏蔽12小时") {
		t.Fatalf("unexpected callback response: %#v", resp)
	}
	item, ok := store.Match("fp_select")
	if !ok {
		t.Fatal("silence rule was not stored")
	}
	if got := item.ExpiresAt.Sub(now); got != 12*time.Hour {
		t.Fatalf("duration = %s, want 12h", got)
	}
}

func TestHandleCardActionSilencePersistsBackendAndCachesLocal(t *testing.T) {
	now := time.Date(2026, 5, 25, 14, 0, 0, 0, time.UTC)
	store := newAlertSilenceStore()
	store.now = func() time.Time { return now }
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/alert/v1/silences" {
			t.Fatalf("unexpected backend request method=%s path=%s", r.Method, r.URL.Path)
		}
		backendCalled = true
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend body: %v", err)
		}
		if body["fingerprint"] != "fp_persist" || body["operatorOpenId"] != "ou_123" {
			t.Fatalf("unexpected backend body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"silence":{"id":"9","fingerprint":"fp_persist","alertname":"MatrixAPI5xxDetected","service":"matrix-api","env":"prod","severity":"P1","operatorOpenId":"ou_123","reason":"飞书卡片手动屏蔽","expiresAt":"` + now.Add(2*time.Hour).Format(time.RFC3339) + `"}}`))
	}))
	defer backend.Close()

	s := &server{
		backend:       &IncidentBackend{BaseURL: backend.URL, HTTPClient: backend.Client(), Timeout: time.Second},
		alertSilences: store,
	}
	resp, err := s.handleCardAction(context.Background(), json.RawMessage(`{
	  "operator":{"user_id":"user_123","open_id":"ou_123"},
	  "action":{
	    "tag":"select_static",
	    "option":"2h",
	    "value":{"action":"silence_alert","fingerprint":"fp_persist","alertname":"MatrixAPI5xxDetected","service":"matrix-api","env":"prod","severity":"P1","incident_id":"42"}
	  }
	}`))
	if err != nil {
		t.Fatalf("handle card action: %v", err)
	}
	if resp.Toast == nil || resp.Toast.Type != "success" || !strings.Contains(resp.Toast.Content, "已屏蔽2小时") {
		t.Fatalf("unexpected toast: %#v", resp.Toast)
	}
	if !backendCalled {
		t.Fatal("backend create silence was not called")
	}
	item, ok := store.Match("fp_persist")
	if !ok {
		t.Fatal("silence rule was not cached locally")
	}
	if item.OperatorOpenID != "ou_123" {
		t.Fatalf("unexpected local silence: %#v", item)
	}
}

func TestHandleFeishuEventsCardActionCopilotReplies(t *testing.T) {
	replyCh := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages/om_123/reply":
			var reply map[string]any
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				t.Fatalf("decode reply body: %v", err)
			}
			replyCh <- reply
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:             "app-id",
		feishuAppSecret:         "app-secret",
		feishuChatID:            "chat-id",
		feishuAPIBase:           feishu.URL,
		feishuVerificationToken: "verify-token",
		client:                  feishu.Client(),
		analyzer:                stubAnalyzer{},
		analysisTimeout:         time.Second,
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_123","open_id":"ou_123"},
	    "context":{"open_message_id":"om_123","open_chat_id":"oc_123"},
	    "action":{"tag":"button","value":{"action":"copilot_analyze","alertname":"MatrixAPI5xxDetected","service":"matrix-api","env":"prod","severity":"P1","status":"firing","summary":"5xx detected","description":"matrix-api 5xx > 0","link":"https://grafana.example.com/panel","starts_at":"2026-04-29T00:00:00Z"}}
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AI 归因已开始") {
		t.Fatalf("unexpected callback response: %s", rec.Body.String())
	}
	select {
	case reply := <-replyCh:
		if reply["msg_type"] != "interactive" || reply["reply_in_thread"] != true {
			t.Fatalf("unexpected reply body: %#v", reply)
		}
		replyContent := fmt.Sprint(reply["content"])
		// The card json embeds operator at-mention, the structured sections
		// the stub analyzer produced, and the alert metadata line.
		for _, want := range []string{
			"ou_123",
			"告警 Copilot 只读归因",
			"MatrixAPI5xxDetected",
			"matrix-api",
			"plain_text",
			"lark_md",
		} {
			if !strings.Contains(replyContent, want) {
				t.Fatalf("reply missing %q: %s", want, replyContent)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for copilot reply")
	}
}

func TestHandleCopilotAnalyzeActionWithoutMessageID(t *testing.T) {
	s := &server{}
	result := s.handleCopilotAnalyzeAction(feishuCardActionEvent{})

	if result.Toast == nil || result.Toast.Type != "warning" || !strings.Contains(result.Toast.Content, "没有找到原消息 ID") {
		t.Fatalf("unexpected toast: %#v", result.Toast)
	}
}

func TestHandleFeishuEventsRefactorEnqueueActionPostsMetricTrigger(t *testing.T) {
	gotCh := make(chan refactorMetricTrigger, 1)
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refactor/triggers/metric" {
			t.Fatalf("unexpected path: %s", r.URL.String())
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		var got refactorMetricTrigger
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode trigger: %v", err)
		}
		gotCh <- got
		_, _ = w.Write([]byte(`{"item":{"id":"wi_123"},"duplicate":false}`))
	}))
	defer orchestrator.Close()

	s := &server{
		feishuVerificationToken: "verify-token",
		refactorOrchestrator: &RefactorOrchestratorClient{
			BaseURL:    orchestrator.URL,
			HTTPClient: orchestrator.Client(),
		},
		refactorDefaultRepo: "matrix-api",
	}
	payload := `{
	  "schema":"2.0",
	  "header":{"event_type":"card.action.trigger","token":"verify-token"},
	  "event":{
	    "operator":{"user_id":"user_123","open_id":"ou_123"},
	    "context":{"open_message_id":"om_123","open_chat_id":"oc_123"},
	    "action":{"tag":"button","value":{"action":"refactor_enqueue","repo":"order-srv","alertname":"OrderErrorSpike","service":"order-srv","env":"prod","severity":"P1","status":"firing","summary":"order error spike","description":"order-srv error > 10","link":"https://grafana.example.com/panel","starts_at":"2026-04-29T00:00:00Z","current_value":"12"}}
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/feishu/events", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	s.handleFeishuEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "已加入自动重构队列") {
		t.Fatalf("unexpected callback response: %s", rec.Body.String())
	}
	select {
	case got := <-gotCh:
		if got.Repo != "order-srv" || got.Service != "order-srv" {
			t.Fatalf("unexpected trigger repo/service: %#v", got)
		}
		if got.Labels["alertname"] != "OrderErrorSpike" || got.Labels["severity"] != "P1" {
			t.Fatalf("unexpected labels: %#v", got.Labels)
		}
		if got.Annotations["summary"] != "order error spike" || got.Annotations["operator"] == "" {
			t.Fatalf("unexpected annotations: %#v", got.Annotations)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refactor trigger")
	}
}

func TestRuleBasedAnalyzerFormats5xxReport(t *testing.T) {
	report, err := RuleBasedAnalyzer{}.Analyze(context.Background(), AnalysisRequest{
		AlertName: "MatrixAPI5xxDetected",
		Service:   "matrix-api",
		Env:       "prod",
		Severity:  "P1",
		Status:    "firing",
		Summary:   "5xx detected",
		Link:      "https://grafana.example.com/panel",
	})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	text := report.FormatText()
	for _, want := range []string{"告警 Copilot 只读归因", "MatrixAPI5xxDetected", "服务端错误", "SLS", "https://grafana.example.com/panel"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}

func TestCommandAnalyzerParsesJSONReport(t *testing.T) {
	script := writeTestScript(t, `#!/bin/sh
input=$(cat)
case "$input" in
  *MatrixAPI5xxDetected*) ;;
  *) echo "missing alert payload" >&2; exit 2 ;;
esac
printf '%s' '{"title":"外部归因","facts":["查到 SLS 错误"],"judgement":["下游错误"],"next_steps":["继续查 order-srv"]}'
`)

	report, err := CommandAnalyzer{
		Command: "sh " + shellQuote(script),
		Timeout: time.Second,
	}.Analyze(context.Background(), AnalysisRequest{AlertName: "MatrixAPI5xxDetected"})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	text := report.FormatText()
	for _, want := range []string{"外部归因", "查到 SLS 错误", "继续查 order-srv"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}

func TestCommandAnalyzerAcceptsPlainText(t *testing.T) {
	script := writeTestScript(t, `#!/bin/sh
cat >/dev/null
printf '%s' '外部 Cursor CLI 归因结果'
`)

	report, err := CommandAnalyzer{
		Command: "sh " + shellQuote(script),
		Timeout: time.Second,
	}.Analyze(context.Background(), AnalysisRequest{AlertName: "MatrixAPI5xxDetected"})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if got := report.FormatText(); got != "外部 Cursor CLI 归因结果" {
		t.Fatalf("report = %q", got)
	}
}

func TestFallbackAnalyzerUsesRuleBasedOnCommandFailure(t *testing.T) {
	analyzer := FallbackAnalyzer{
		Primary: CommandAnalyzer{
			Command: "sh -c 'echo runner failed >&2; exit 1'",
			Timeout: time.Second,
		},
		Fallback: RuleBasedAnalyzer{},
	}

	report, err := analyzer.Analyze(context.Background(), AnalysisRequest{
		AlertName: "MatrixAPI5xxDetected",
		Service:   "matrix-api",
		Env:       "prod",
		Status:    "firing",
	})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	text := report.FormatText()
	for _, want := range []string{"规则兜底", "AI runner 未产出有效归因", "runner failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback report missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "规则型") && !strings.Contains(text, "服务端错误") {
		t.Fatalf("fallback report not used:\n%s", text)
	}
}

func TestCommandAnalyzerTimeout(t *testing.T) {
	_, err := CommandAnalyzer{
		Command: "sleep 1",
		Timeout: 10 * time.Millisecond,
	}.Analyze(context.Background(), AnalysisRequest{AlertName: "SlowAlert"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

type stubAnalyzer struct{}

func (stubAnalyzer) Analyze(_ context.Context, req AnalysisRequest) (AnalysisReport, error) {
	return AnalysisReport{
		Title: "告警 Copilot 只读归因",
		Facts: []string{
			"告警：" + req.AlertName,
			"服务/环境：" + req.Service + " / " + req.Env,
		},
		Judgement: []string{"测试归因"},
		NextSteps: []string{"测试下一步"},
	}, nil
}

func writeTestScript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.sh")
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func encryptFeishuEventForTest(t *testing.T, plain []byte, encryptKey string) string {
	t.Helper()

	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}

	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append([]byte(nil), plain...)
	padded = append(padded, bytes.Repeat([]byte{byte(padding)}, padding)...)

	iv := []byte("0123456789abcdef")
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	return base64.StdEncoding.EncodeToString(append(iv, ciphertext...))
}
