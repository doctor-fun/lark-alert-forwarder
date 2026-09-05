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

func TestParseTestCaseReminderOwners(t *testing.T) {
	owners := parseTestCaseReminderOwners("Frank|ou_gao, Grace|ou_wang, skip")
	if len(owners) != 2 || owners[0].Name != "Frank" || owners[1].OpenID != "ou_wang" {
		t.Fatalf("owners=%+v", owners)
	}
	if parseTestCaseReminderOwners("") != nil {
		t.Fatal("expected empty env to yield no owners")
	}
}

func TestNextFridayAt1400(t *testing.T) {
	loc := chinaLocation()
	tests := []struct {
		now  time.Time
		want string
	}{
		{now: time.Date(2026, 8, 14, 13, 0, 0, 0, loc), want: "2026-08-14 14:00"},
		{now: time.Date(2026, 8, 14, 14, 0, 0, 0, loc), want: "2026-08-21 14:00"},
		{now: time.Date(2026, 8, 13, 18, 0, 0, 0, loc), want: "2026-08-14 14:00"},
	}
	for _, tt := range tests {
		if got := nextFridayAt1400(tt.now).Format("2006-01-02 15:04"); got != tt.want {
			t.Fatalf("nextFridayAt1400(%s)=%s, want %s", tt.now, got, tt.want)
		}
	}
}

func TestTestCaseReminderConfirmationRequiresBothOwners(t *testing.T) {
	r := &testCaseReminder{
		cfg: testCaseReminderConfig{
			TableURL: "https://wishhudong.feishu.cn/base/BIe6b6kA0ay6qVsoXEnc5p3dnTb",
			Owners: []testCaseReminderOwner{
				{Name: "Frank", OpenID: "ou_gao"},
				{Name: "Grace", OpenID: "ou_wang"},
			},
		},
		confirmed:  map[string]time.Time{},
		suppressed: map[string]time.Time{},
	}
	now := time.Date(2026, 8, 14, 14, 0, 0, 0, chinaLocation())
	r.begin(now)
	week, active, _, _, _ := r.snapshot()
	if !active {
		t.Fatal("reminder should be active after weekly trigger")
	}
	if ok, _ := r.confirm(now, week, "ou_other"); ok {
		t.Fatal("non-owner must not confirm")
	}
	if ok, _ := r.confirm(now, week, "ou_gao"); !ok {
		t.Fatal("first owner should confirm")
	}
	_, active, confirmed, _, _ := r.snapshot()
	if !active || len(confirmed) != 1 {
		t.Fatalf("reminder must continue until both owners confirm: active=%v confirmed=%v", active, confirmed)
	}
	if ok, _ := r.confirm(now, week, "ou_wang"); !ok {
		t.Fatal("second owner should confirm")
	}
	_, active, _, _, _ = r.snapshot()
	if active {
		t.Fatal("reminder must stop after both owners confirm")
	}
}

func TestTestCaseReminderSuppressesOneOrAll(t *testing.T) {
	newReminder := func() *testCaseReminder {
		return &testCaseReminder{
			cfg: testCaseReminderConfig{Owners: []testCaseReminderOwner{
				{Name: "Frank", OpenID: "ou_gao"},
				{Name: "Grace", OpenID: "ou_wang"},
			}},
			confirmed:  map[string]time.Time{},
			suppressed: map[string]time.Time{},
		}
	}
	now := time.Date(2026, 8, 14, 14, 0, 0, 0, chinaLocation())

	r := newReminder()
	r.begin(now)
	week, _, _, _, _ := r.snapshot()
	if ok, _ := r.suppress(now, week, "ou_gao", false); !ok {
		t.Fatal("owner should be able to suppress personal reminder")
	}
	_, active, _, suppressed, all := r.snapshot()
	if !active || all || len(suppressed) != 1 {
		t.Fatalf("personal suppression must retain the other owner: active=%v all=%v suppressed=%v", active, all, suppressed)
	}

	r = newReminder()
	r.begin(now)
	week, _, _, _, _ = r.snapshot()
	if ok, _ := r.suppress(now, week, "ou_gao", true); !ok {
		t.Fatal("owner should be able to suppress the weekly reminder for all")
	}
	_, active, _, _, all = r.snapshot()
	if active || !all {
		t.Fatalf("global suppression must stop the weekly reminder: active=%v all=%v", active, all)
	}
}

func TestTestCaseReminderActionUpdatesCompatibleCard(t *testing.T) {
	r := &testCaseReminder{
		cfg: testCaseReminderConfig{
			TableURL: "https://wishhudong.feishu.cn/base/BIe6b6kA0ay6qVsoXEnc5p3dnTb",
			Owners: []testCaseReminderOwner{
				{Name: "Frank", OpenID: "ou_gao"},
				{Name: "Grace", OpenID: "ou_wang"},
			},
		},
		confirmed:  map[string]time.Time{},
		suppressed: map[string]time.Time{},
	}
	r.begin(time.Date(2026, 8, 14, 14, 0, 0, 0, chinaLocation()))
	week, _, _, _, _ := r.snapshot()
	s := &server{testCaseReminder: r}
	event := feishuCardActionEvent{}
	event.Action.Value = map[string]string{"week": week}
	event.Operator.OpenID = "ou_gao"
	reply, err := s.handleTestCaseReminderAction(context.Background(), event)
	if err != nil {
		t.Fatalf("handle action: %v", err)
	}
	if reply.Card == nil || reply.Card.Data.Schema != "" {
		t.Fatalf("callback should return a compatible legacy card: %+v", reply.Card)
	}
	raw, err := json.Marshal(reply.Card.Data)
	if err != nil {
		t.Fatalf("marshal updated card: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "我已提交测试用例") {
		t.Fatalf("updated card missing submit button: %s", content)
	}
	if !strings.Contains(content, "提交/上传测试用例") {
		t.Fatalf("updated card missing Bitable entry: %s", content)
	}
	if !strings.Contains(content, "[打开测试用例提交表](https://wishhudong.feishu.cn/base/BIe6b6kA0ay6qVsoXEnc5p3dnTb)") {
		t.Fatalf("updated card missing visible Bitable link: %s", content)
	}
}

func TestTestCaseReminderMessageCommandSendsCard(t *testing.T) {
	sent := make(chan map[string]any, 1)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/im/v1/messages":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode sent card: %v", err)
			}
			sent <- body
			_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"om_test_case"}}`))
		default:
			t.Fatalf("unexpected Feishu API path: %s", r.URL.Path)
		}
	}))
	defer feishu.Close()

	reminder := &testCaseReminder{
		cfg: testCaseReminderConfig{
			ChatID:   "oc_test_case",
			TableURL: "https://wishhudong.feishu.cn/base/example",
			Owners:   []testCaseReminderOwner{{Name: "Frank", OpenID: "ou_gao"}},
		},
		confirmed:  map[string]time.Time{},
		suppressed: map[string]time.Time{},
	}
	s := &server{
		feishuAppID:      "app-id",
		feishuAppSecret:  "app-secret",
		feishuChatID:     "oc_default",
		feishuAPIBase:    feishu.URL,
		client:           feishu.Client(),
		testCaseReminder: reminder,
	}
	event := json.RawMessage(`{
		"sender":{"sender_id":{"open_id":"ou_operator"}},
		"message":{"chat_id":"oc_test_case","chat_type":"group","message_type":"text","content":"{\"text\":\"@告警机器人 用例\"}"}
	}`)
	if err := s.handleTestCaseReminderMessageEvent(context.Background(), event); err != nil {
		t.Fatalf("handle message command: %v", err)
	}
	msg := <-sent
	if got := msg["receive_id"]; got != "oc_test_case" {
		t.Fatalf("receive_id=%v, want test chat", got)
	}
	if content := msg["content"].(string); !strings.Contains(content, "提交/上传测试用例") {
		t.Fatalf("sent card missing Bitable entry: %s", content)
	}
	if _, active, _, _, _ := reminder.snapshot(); !active {
		t.Fatal("message command should activate this week's reminder")
	}
}
