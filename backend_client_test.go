package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestComputeFingerprintIncludesRouteAndStatus(t *testing.T) {
	base := computeFingerprint("MatrixAPI5xxDetected", "matrix-api", "prod", map[string]string{
		"severity": "P1",
		"route":    "/v1/callback/apply/pay/notification",
		"status":   "500",
	})
	sameWithNoise := computeFingerprint("MatrixAPI5xxDetected", "matrix-api", "prod", map[string]string{
		"severity":           "P1",
		"route":              "/v1/callback/apply/pay/notification",
		"status":             "500",
		"__alert_rule_uid__": "grafana-noise",
	})
	if base != sameWithNoise {
		t.Fatal("grafana noise labels should not affect fingerprint")
	}
	otherRoute := computeFingerprint("MatrixAPI5xxDetected", "matrix-api", "prod", map[string]string{
		"severity": "P1",
		"route":    "/v1/media/search",
		"status":   "500",
	})
	if base == otherRoute {
		t.Fatal("different routes must create different fingerprints")
	}
	otherStatus := computeFingerprint("MatrixAPI5xxDetected", "matrix-api", "prod", map[string]string{
		"severity": "P1",
		"route":    "/v1/callback/apply/pay/notification",
		"status":   "502",
	})
	if base == otherStatus {
		t.Fatal("different status codes must create different fingerprints")
	}
}

// TestUpsertOnFire_ProtoJSONInt64 防止回归：
//
// matrix-backend 用 Kratos protojson 序列化，proto3 int64 会输出成 JSON string
// （避免 JS 大数精度丢失），所以 incidentInfoDTO.ID 必须能同时吃 string 和 number
// 两种格式。任何改动如果让 ID 又退回成 plain int64，这个 test 会立刻挂掉，避免
// 像 phase B cut3 smoke 时那样跑到生产才发现卡片少了 incident #N 标签。
func TestUpsertOnFire_ProtoJSONInt64(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{
			name: "id_as_string_protojson_default",
			body: `{"incident":{"id":"42","fingerprint":"fp1","alertname":"x"},"dedup":false}`,
			want: 42,
		},
		{
			name: "id_as_number_classic_json",
			body: `{"incident":{"id":42,"fingerprint":"fp1","alertname":"x"},"dedup":false}`,
			want: 42,
		},
		{
			name: "id_missing_or_empty",
			body: `{"incident":{"fingerprint":"fp1","alertname":"x"},"dedup":false}`,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			b := &IncidentBackend{
				BaseURL:    srv.URL,
				HTTPClient: &http.Client{Timeout: 2 * time.Second},
				Timeout:    1 * time.Second,
			}
			incident, _, _, err := b.UpsertOnFire(context.Background(), grafanaWebhook{
				Status: "firing",
				CommonLabels: map[string]string{
					"alertname": "x",
					"service":   "y",
				},
				Alerts: []grafanaAlert{{Status: "firing"}},
			})
			if err != nil {
				t.Fatalf("UpsertOnFire: %v", err)
			}
			if incident == nil {
				t.Fatal("incident is nil")
			}
			if got := incident.idAsInt64(); got != tc.want {
				t.Fatalf("idAsInt64 = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEscalationCandidate_IDProtoJSON 同上，但作用于升级候选 list。
// 注意 fixture 使用 camelCase 与 protojson 实际输出对齐。
func TestEscalationCandidate_IDProtoJSON(t *testing.T) {
	raw := `{"needL1":[{"id":"7","alertname":"x","service":"y","feishuMsgId":"om_1"}],"needL2":[]}`
	var reply listEscalationReply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(reply.NeedL1) != 1 {
		t.Fatalf("NeedL1 len=%d, want 1", len(reply.NeedL1))
	}
	if got := reply.NeedL1[0].idAsInt64(); got != 7 {
		t.Fatalf("idAsInt64=%d, want 7", got)
	}
	if reply.NeedL1[0].FeishuMsgID != "om_1" {
		t.Fatalf("FeishuMsgID=%q, want om_1", reply.NeedL1[0].FeishuMsgID)
	}
}

// TestResolveMinutes_ProtoJSON 防止 resolveIncidentReply 回归。
func TestResolveMinutes_ProtoJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"incident":{"id":"3"},"resolveMinutes":"125"}`))
	}))
	defer srv.Close()
	b := &IncidentBackend{
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	_, mins, err := b.Resolve(context.Background(), 3, "ou_x", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if mins != 125 {
		t.Fatalf("mins=%d, want 125", mins)
	}
}

// TestIncidentInfo_CamelCaseMultiword 是 phase B 灰度回归记一笔：
//
// matrix-backend 用 Kratos protojson 序列化，proto3 多词字段（assignee_open_id /
// feishu_msg_id / dashboard_url 等）默认输出 **lowerCamelCase**。曾经把 forwarder
// DTO 写成 snake_case，跑通了链路但卡片永远不渲染"责任人"行（assignee 空），
// escalator 也永远 skip 升级（feishuMsgId 空）。这个 test 用真实 protojson 形态
// 的响应做反序列化校验：
//
//   - assigneeOpenId 必须解码进 AssigneeOpenID
//   - feishuMsgId    必须解码进 FeishuMsgID
//   - assignedAt 等多词时间字段同理
func TestIncidentInfo_CamelCaseMultiword(t *testing.T) {
	raw := `{
		"incident":{
			"id":"42",
			"fingerprint":"fp1",
			"alertname":"x",
			"service":"matrix-api",
			"severity":"P1",
			"status":1,
			"dashboardUrl":"https://g/d",
			"feishuMsgId":"om_abc",
			"feishuChatId":"oc_abc",
			"assigneeOpenId":"ou_bob",
			"assignedAt":"2026-05-12T01:15:34Z",
			"fireCount":1,
			"firstFiredAt":"2026-05-12T01:15:31Z",
			"lastFiredAt":"2026-05-12T01:15:31Z"
		},
		"dedup":false
	}`
	var reply upsertIncidentReply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Incident == nil {
		t.Fatal("incident is nil")
	}
	got := reply.Incident
	if got.AssigneeOpenID != "ou_bob" {
		t.Fatalf("AssigneeOpenID=%q, want ou_bob", got.AssigneeOpenID)
	}
	if got.FeishuMsgID != "om_abc" {
		t.Fatalf("FeishuMsgID=%q, want om_abc", got.FeishuMsgID)
	}
	if got.FeishuChatID != "oc_abc" {
		t.Fatalf("FeishuChatID=%q, want oc_abc", got.FeishuChatID)
	}
	if got.DashboardURL != "https://g/d" {
		t.Fatalf("DashboardURL=%q, want https://g/d", got.DashboardURL)
	}
	if got.AssignedAt == "" {
		t.Fatalf("AssignedAt should be parsed, got empty")
	}
	if got.FireCount != 1 {
		t.Fatalf("FireCount=%d, want 1", got.FireCount)
	}
}

// TestUpsertOnFire_FireMentions 验证 backend 的 fireMentionOpenIds 能透传到 forwarder。
//
// 这是 phase D 的核心契约：admin 在 oncall 配置里把当前 severity 标记为"首次告警就
// @ 全员"时，backend 给出去 OOO/去 assignee/去停用之后的列表，forwarder 必须把它
// 原样塞进卡片 cc 行；任何反序列化字段名漂移（snake_case / 首字母大小写）都会让
// "fire-time @ 全员"功能静默失效——线上发出去的卡片只 @ assignee，运营在群里再喊一遍
// 才知道。本测用真实 protojson 形态做回归。
func TestUpsertOnFire_FireMentions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"incident":{"id":"99","fingerprint":"fp1","alertname":"x","assigneeOpenId":"ou_a"},
			"dedup":false,
			"fireMentionOpenIds":["ou_b","ou_c"]
		}`))
	}))
	defer srv.Close()
	b := &IncidentBackend{
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		Timeout:    1 * time.Second,
	}
	_, _, fireMentions, err := b.UpsertOnFire(context.Background(), grafanaWebhook{
		Status:       "firing",
		CommonLabels: map[string]string{"alertname": "x", "service": "y"},
		Alerts:       []grafanaAlert{{Status: "firing"}},
	})
	if err != nil {
		t.Fatalf("UpsertOnFire: %v", err)
	}
	if len(fireMentions) != 2 || fireMentions[0] != "ou_b" || fireMentions[1] != "ou_c" {
		t.Fatalf("fireMentions = %v, want [ou_b ou_c]", fireMentions)
	}
}

func TestCreateSilenceAndMatchSilence(t *testing.T) {
	expiresAt := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	var sawCreate bool
	var sawMatch bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/alert/v1/silences":
			sawCreate = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if body["operatorOpenId"] != "ou_operator" || body["expiresAt"] == "" {
				t.Fatalf("unexpected create body: %#v", body)
			}
			_, _ = w.Write([]byte(`{"silence":{"id":"7","fingerprint":"fp1","alertname":"HighError","service":"matrix-api","env":"prod","severity":"P1","operatorOpenId":"ou_operator","reason":"manual","expiresAt":"` + expiresAt.Format(time.RFC3339) + `","createdAt":"2026-06-12T08:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/alert/v1/silences/fp1:match":
			sawMatch = true
			_, _ = w.Write([]byte(`{"active":true,"silence":{"id":"7","fingerprint":"fp1","alertname":"HighError","service":"matrix-api","env":"prod","severity":"P1","operatorOpenId":"ou_operator","reason":"manual","expiresAt":"` + expiresAt.Format(time.RFC3339) + `"}}`))
		default:
			t.Fatalf("unexpected request method=%s path=%s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := &IncidentBackend{BaseURL: srv.URL, HTTPClient: srv.Client(), Timeout: time.Second}
	created, err := b.CreateSilence(context.Background(), alertSilence{
		Fingerprint:    "fp1",
		AlertName:      "HighError",
		Service:        "matrix-api",
		Env:            "prod",
		Severity:       "P1",
		OperatorOpenID: "ou_operator",
		Reason:         "manual",
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateSilence: %v", err)
	}
	if created.Fingerprint != "fp1" || !created.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("unexpected created silence: %#v", created)
	}

	matched, ok, err := b.MatchSilence(context.Background(), "fp1")
	if err != nil {
		t.Fatalf("MatchSilence: %v", err)
	}
	if !ok || matched.OperatorOpenID != "ou_operator" {
		t.Fatalf("unexpected matched silence ok=%v item=%#v", ok, matched)
	}
	if !sawCreate || !sawMatch {
		t.Fatalf("sawCreate=%v sawMatch=%v, want true/true", sawCreate, sawMatch)
	}
}
