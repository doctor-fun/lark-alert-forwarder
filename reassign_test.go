package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ====== handleReassignNotify HTTP 层测试 ======

func TestHandleReassignNotify_TokenMissingRejects(t *testing.T) {
	s := &server{token: "secret"}
	body, _ := json.Marshal(internalReassignEvent{IncidentID: 1, NewAssigneeID: "ou_a"})
	req := httptest.NewRequest(http.MethodPost, "/internal/incident_reassigned", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleReassignNotify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when token missing", rec.Code)
	}
}

func TestHandleReassignNotify_BearerOK(t *testing.T) {
	s := &server{token: "secret"}
	body, _ := json.Marshal(internalReassignEvent{IncidentID: 1, NewAssigneeID: "ou_a"})
	req := httptest.NewRequest(http.MethodPost, "/internal/incident_reassigned", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleReassignNotify(rec, req)
	// feishu app 未配（appID 空），返 202 + skipped；说明鉴权过了。
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s, want 202", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "skipped") {
		t.Fatalf("body = %q, want 'skipped' (feishu app not configured)", rec.Body.String())
	}
}

func TestHandleReassignNotify_XForwarderTokenOK(t *testing.T) {
	s := &server{token: "secret"}
	body, _ := json.Marshal(internalReassignEvent{IncidentID: 1, NewAssigneeID: "ou_a"})
	req := httptest.NewRequest(http.MethodPost, "/internal/incident_reassigned", bytes.NewReader(body))
	req.Header.Set("X-Forwarder-Token", "secret")
	rec := httptest.NewRecorder()
	s.handleReassignNotify(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestHandleReassignNotify_WrongMethodRejects(t *testing.T) {
	s := &server{}
	req := httptest.NewRequest(http.MethodGet, "/internal/incident_reassigned", nil)
	rec := httptest.NewRecorder()
	s.handleReassignNotify(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleReassignNotify_BadJSONRejects(t *testing.T) {
	s := &server{}
	req := httptest.NewRequest(http.MethodPost, "/internal/incident_reassigned", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	s.handleReassignNotify(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleReassignNotify_MissingFieldsRejected(t *testing.T) {
	s := &server{}
	cases := []internalReassignEvent{
		{NewAssigneeID: "ou_a"},                // incidentId 缺
		{IncidentID: 1},                        // newAssigneeOpenId 缺
		{IncidentID: 1, NewAssigneeID: "   "},  // newAssigneeOpenId 全空格
	}
	for i, c := range cases {
		body, _ := json.Marshal(c)
		req := httptest.NewRequest(http.MethodPost, "/internal/incident_reassigned", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleReassignNotify(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("case %d: status = %d, want 400 for missing fields", i, rec.Code)
		}
	}
}

// ====== buildReassignDMCard 测试：新接手人卡片 ======

func TestBuildReassignDMCard_AdminOverrideHasFiveButtons(t *testing.T) {
	ev := internalReassignEvent{
		IncidentID:    42,
		AlertName:     "matrix-api-5xx",
		Service:       "matrix-api",
		Env:           "prod",
		Severity:      "p1",
		Summary:       "p99 latency",
		Description:   "....",
		DashboardURL:  "https://grafana/x",
		NewAssigneeID: "ou_new",
		Reason:        "admin_override",
	}
	card := buildReassignDMCard(ev)
	if !strings.Contains(card.Header.Title.Content, "你被指派了告警 #42") {
		t.Fatalf("title = %q, want '你被指派了告警 #42'", card.Header.Title.Content)
	}
	if card.Header.Template != "blue" {
		t.Fatalf("template = %q, want blue", card.Header.Template)
	}
	actions := findActionsBlock(t, card)
	wantActionKeys := map[string]bool{"claim": false, "resolve": false, "discard": false, "copilot_analyze": false}
	wantURLButton := false
	for _, a := range actions {
		if tag, _ := a["tag"].(string); tag != "button" {
			continue
		}
		if url, ok := a["url"].(string); ok && url == "https://grafana/x" {
			wantURLButton = true
			continue
		}
		if v, ok := a["value"].(map[string]string); ok {
			if _, exists := wantActionKeys[v["action"]]; exists {
				wantActionKeys[v["action"]] = true
				if v["incident_id"] != "42" {
					t.Fatalf("action %q missing incident_id=42, got %q", v["action"], v["incident_id"])
				}
			}
		}
	}
	for k, hit := range wantActionKeys {
		if !hit {
			t.Fatalf("missing callback button action=%q in DM card", k)
		}
	}
	if !wantURLButton {
		t.Fatal("missing '打开大盘' URL button")
	}
}

// TestBuildReassignDMCard_ClaimReasonHasDifferentHeadline 抢单（claim）和被指派
// （admin_override）文案应不同，避免让"我自己点的"用户误解为别人改派给他。
func TestBuildReassignDMCard_ClaimReasonHasDifferentHeadline(t *testing.T) {
	ev := internalReassignEvent{IncidentID: 7, AlertName: "x", NewAssigneeID: "ou_self", Reason: "claim"}
	card := buildReassignDMCard(ev)
	if !strings.Contains(card.Header.Title.Content, "你已接走告警") {
		t.Fatalf("title = %q, want '你已接走告警 ...'", card.Header.Title.Content)
	}
}

// TestBuildReassignDMCard_NoDashboardSkipsURLButton 没 link 时不渲染 url 按钮，
// 避免出现"按了没反应"的死按钮。
func TestBuildReassignDMCard_NoDashboardSkipsURLButton(t *testing.T) {
	ev := internalReassignEvent{IncidentID: 1, AlertName: "x", NewAssigneeID: "ou_a"}
	card := buildReassignDMCard(ev)
	actions := findActionsBlock(t, card)
	for _, a := range actions {
		if _, ok := a["url"]; ok {
			t.Fatalf("unexpected URL button when DashboardURL empty: %+v", a)
		}
	}
}

// ====== buildHandoffDMCard 测试：原责任人卡片 ======

func TestBuildHandoffDMCard_NoActionButtons(t *testing.T) {
	ev := internalReassignEvent{
		IncidentID:     17,
		AlertName:      "x",
		Service:        "matrix-api",
		Severity:       "p1",
		PrevAssigneeID: "ou_prev",
		NewAssigneeID:  "ou_new",
		Reason:         "admin_override",
	}
	card := buildHandoffDMCard(ev)
	if card.Header.Template != "grey" {
		t.Fatalf("template = %q, want grey", card.Header.Template)
	}
	for _, el := range card.Elements {
		if tag, _ := el["tag"].(string); tag != "action" {
			continue
		}
		acts, _ := el["actions"].([]map[string]any)
		for _, a := range acts {
			if _, hasCallback := a["value"]; hasCallback {
				t.Fatalf("handoff card should not have callback buttons, got %+v", a)
			}
		}
	}
}

func TestBuildHandoffDMCard_ClaimWording(t *testing.T) {
	ev := internalReassignEvent{IncidentID: 1, AlertName: "MatrixAPI5xx", Reason: "claim"}
	card := buildHandoffDMCard(ev)
	text := elementMarkdownString(card.Elements)
	if !strings.Contains(text, "已被同事在群里抢单") {
		t.Fatalf("claim handoff text should mention 抢单, got: %q", text)
	}
}

func TestBuildHandoffDMCard_AdminWording(t *testing.T) {
	ev := internalReassignEvent{IncidentID: 1, AlertName: "MatrixAPI5xx", Reason: "admin_override"}
	card := buildHandoffDMCard(ev)
	text := elementMarkdownString(card.Elements)
	if !strings.Contains(text, "已由管理员转给") {
		t.Fatalf("admin handoff text should mention 管理员, got: %q", text)
	}
}

// ====== buildReassignThreadText 测试 ======

func TestBuildReassignThreadText_ClaimsMentionBoth(t *testing.T) {
	ev := internalReassignEvent{
		IncidentID: 9, Reason: "claim",
		PrevAssigneeID: "ou_prev", NewAssigneeID: "ou_new",
	}
	text := buildReassignThreadText(ev)
	if !strings.Contains(text, "#9") {
		t.Fatalf("text should contain '#9': %s", text)
	}
	if !strings.Contains(text, `user_id="ou_new"`) {
		t.Fatalf("text should @ new assignee: %s", text)
	}
	if !strings.Contains(text, `user_id="ou_prev"`) {
		t.Fatalf("text should reference prev assignee: %s", text)
	}
	if !strings.Contains(text, "抢单接走") {
		t.Fatalf("claim wording missing: %s", text)
	}
}

func TestBuildReassignThreadText_AdminOverride(t *testing.T) {
	ev := internalReassignEvent{
		IncidentID: 9, Reason: "admin_override",
		PrevAssigneeID: "ou_prev", NewAssigneeID: "ou_new",
	}
	text := buildReassignThreadText(ev)
	if !strings.Contains(text, "管理员转派") {
		t.Fatalf("admin wording missing: %s", text)
	}
}

func TestBuildReassignThreadText_NoPrevAssigneeOK(t *testing.T) {
	ev := internalReassignEvent{IncidentID: 1, Reason: "reassign", NewAssigneeID: "ou_new"}
	text := buildReassignThreadText(ev)
	if strings.Contains(text, "原") {
		t.Fatalf("should not contain '原 ...' when prev empty, got: %s", text)
	}
	if !strings.Contains(text, `user_id="ou_new"`) {
		t.Fatalf("must @ new assignee: %s", text)
	}
}

// ====== dispatchReassignNotify 集成测试 ======
//
// 用 httptest 起 fake 飞书 server，server 把 sendFeishuAppCardTo / replyFeishuMessage
// 的请求都打过来；通过计数 + 解析 URL 路径来断言"3 路都发了 / 跳过的真没发"。
//
// 这里直接 instantiate `server` 让 feishuAppConfigured() 返回 true，并把 feishuAPIBase
// 指向 fake server，token cache 用 fakeTokenServer 实现。

func TestDispatchReassignNotify_AllThreeRoutes(t *testing.T) {
	var (
		dmCount    int32
		replyCount int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/open-apis/auth/v3/tenant_access_token/internal"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t","expire":3600}`)
		case strings.Contains(r.URL.Path, "/im/v1/messages") && strings.HasSuffix(r.URL.Path, "/reply"):
			atomic.AddInt32(&replyCount, 1)
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok"}`)
		case strings.Contains(r.URL.Path, "/im/v1/messages"):
			atomic.AddInt32(&dmCount, 1)
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"message_id":"om_xx"}}`)
		default:
			t.Logf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &server{
		feishuAppID:     "cli_x",
		feishuAppSecret: "sec",
		feishuChatID:    "oc_x",
		feishuAPIBase:   srv.URL,
		client:          srv.Client(),
	}

	ev := internalReassignEvent{
		IncidentID:     42,
		AlertName:      "x",
		FeishuMsgID:    "om_orig",
		PrevAssigneeID: "ou_prev",
		NewAssigneeID:  "ou_new",
		Reason:         "admin_override",
	}
	s.dispatchReassignNotify(ev)

	if got := atomic.LoadInt32(&dmCount); got != 2 {
		t.Fatalf("DM sent = %d, want 2 (new + prev)", got)
	}
	if got := atomic.LoadInt32(&replyCount); got != 1 {
		t.Fatalf("group reply = %d, want 1", got)
	}
}

func TestDispatchReassignNotify_SkipsPrevDMWhenEmpty(t *testing.T) {
	var dmCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal") {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"t","expire":3600}`)
			return
		}
		if strings.Contains(r.URL.Path, "/im/v1/messages") && !strings.HasSuffix(r.URL.Path, "/reply") {
			atomic.AddInt32(&dmCount, 1)
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"message_id":"om_xx"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s := &server{
		feishuAppID:     "cli_x",
		feishuAppSecret: "sec",
		feishuChatID:    "oc_x",
		feishuAPIBase:   srv.URL,
		client:          srv.Client(),
	}
	s.dispatchReassignNotify(internalReassignEvent{
		IncidentID:    42,
		AlertName:     "x",
		NewAssigneeID: "ou_new",
		// 注意：PrevAssigneeID + FeishuMsgID 都是空
		Reason: "claim",
	})

	if got := atomic.LoadInt32(&dmCount); got != 1 {
		t.Fatalf("DM sent = %d, want 1 (only new assignee, prev/group skipped)", got)
	}
}

// TestHandleReassignNotify_ReturnsQuicklyEvenIfFeishuSlow 验证 HTTP 立即返回 202，
// 不等飞书 API。
func TestHandleReassignNotify_ReturnsQuicklyEvenIfFeishuSlow(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second) // 故意慢
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()
	s := &server{
		feishuAppID:     "cli_x",
		feishuAppSecret: "sec",
		feishuChatID:    "oc_x",
		feishuAPIBase:   slow.URL,
		client:          slow.Client(),
	}
	body, _ := json.Marshal(internalReassignEvent{IncidentID: 1, AlertName: "x", NewAssigneeID: "ou_a"})
	req := httptest.NewRequest(http.MethodPost, "/internal/incident_reassigned", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	start := time.Now()
	s.handleReassignNotify(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("HTTP handler blocked %v on slow feishu, want < 500ms", elapsed)
	}
}

// ====== 辅助 ======

func findActionsBlock(t *testing.T, card feishuCard) []map[string]any {
	t.Helper()
	for _, el := range card.Elements {
		if tag, _ := el["tag"].(string); tag == "action" {
			if as, ok := el["actions"].([]map[string]any); ok {
				return as
			}
		}
	}
	t.Fatalf("no action block in card: %+v", card)
	return nil
}

// elementMarkdownString 把 div+lark_md 里的 content 拼起来，方便单测做"包含"断言。
// cardMarkdown 的 text 子字段是 map[string]string；旧代码可能塞 map[string]any，所以
// 双类型断言都做一遍。
func elementMarkdownString(elems []map[string]any) string {
	var sb strings.Builder
	for _, el := range elems {
		if c, ok := el["content"].(string); ok {
			sb.WriteString(c)
			sb.WriteString(" ")
		}
		switch text := el["text"].(type) {
		case map[string]any:
			if c, ok := text["content"].(string); ok {
				sb.WriteString(c)
				sb.WriteString(" ")
			}
		case map[string]string:
			sb.WriteString(text["content"])
			sb.WriteString(" ")
		}
	}
	return sb.String()
}
