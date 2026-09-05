package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIssueTrackerOpenDueFiltersStatuses(t *testing.T) {
	records := []issueTrackerRecord{
		{RecordID: "r1", Title: "待处理", Status: issueTrackerStatusOwnerPending},
		{RecordID: "r2", Title: "协助中", Status: issueTrackerStatusHelperPending},
		{RecordID: "r3", Title: "待验证", Status: issueTrackerStatusPendingVerify},
		{RecordID: "r4", Title: "已关闭", Status: issueTrackerStatusClosed},
		{RecordID: "r5", Title: "已拒绝", Status: issueTrackerStatusRejected},
		{RecordID: "r1", Title: "重复 id 应去重", Status: issueTrackerStatusOwnerPending},
		{RecordID: "r6", Title: "", Number: "", Status: issueTrackerStatusOwnerPending},
	}
	due := issueTrackerOpenDue(records, []string{
		issueTrackerStatusOwnerPending,
		issueTrackerStatusHelperPending,
		issueTrackerStatusPendingVerify,
	})
	if len(due) != 3 {
		t.Fatalf("expected 3 open records, got %d: %+v", len(due), due)
	}
	got := map[string]string{}
	for _, rec := range due {
		got[rec.RecordID] = rec.Status
	}
	if got["r1"] != issueTrackerStatusOwnerPending || got["r2"] != issueTrackerStatusHelperPending || got["r3"] != issueTrackerStatusPendingVerify {
		t.Fatalf("unexpected due set: %+v", got)
	}
}

func TestIssueTrackerMentionForStatusFallback(t *testing.T) {
	base := issueTrackerRecord{
		AssigneeOpenID: "ou_owner",
		AssigneeName:   "负责人甲",
		HelperOpenID:   "ou_helper",
		HelperName:     "协助乙",
		CreatorOpenID:  "ou_creator",
		CreatorName:    "创建丙",
	}

	cases := []struct {
		name   string
		status string
		helper string
		create string
		wantID string
		wantRole string
	}{
		{name: "owner pending", status: issueTrackerStatusOwnerPending, wantID: "ou_owner", wantRole: "负责人"},
		{name: "helper pending", status: issueTrackerStatusHelperPending, wantID: "ou_helper", wantRole: "协助人"},
		{name: "helper empty fallback", status: issueTrackerStatusHelperPending, helper: "-", wantID: "ou_owner", wantRole: "负责人"},
		{name: "verify creator", status: issueTrackerStatusPendingVerify, wantID: "ou_creator", wantRole: "创建人"},
		{name: "verify empty fallback", status: issueTrackerStatusPendingVerify, create: "-", wantID: "ou_owner", wantRole: "负责人"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			rec.Status = tc.status
			if tc.helper == "-" {
				rec.HelperOpenID = ""
				rec.HelperName = ""
			}
			if tc.create == "-" {
				rec.CreatorOpenID = ""
				rec.CreatorName = ""
			}
			openID, _, role := issueTrackerMentionForStatus(rec)
			if openID != tc.wantID || role != tc.wantRole {
				t.Fatalf("got openID=%s role=%s, want %s / %s", openID, role, tc.wantID, tc.wantRole)
			}
		})
	}
}

func TestIssueTrackerReminderSendsGroupCard(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	sentCh := make(chan map[string]any, 2)
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/bitable/v1/apps/app-issue/tables/tbl_issue/records":
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected bitable method: %s", r.Method)
			}
			fields := r.URL.Query().Get("field_names")
			for _, need := range []string{"问题标题", "问题状态", "负责人（问题源头负责人）", "协助人", "创建人"} {
				if !strings.Contains(fields, need) {
					t.Fatalf("missing field %q in field_names=%s", need, fields)
				}
			}
			resp := map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items": []map[string]any{
						{
							"record_id": "rec_owner",
							"fields": map[string]any{
								"问题编号":           "ISS-0002",
								"问题标题":           "需要产品确认范围",
								"问题状态":           issueTrackerStatusOwnerPending,
								"负责人（问题源头负责人）": []map[string]any{{"id": "ou_owner", "name": "负责人甲"}},
								"创建时间":           now.Add(-2 * time.Hour).UnixMilli(),
							},
						},
						{
							"record_id": "rec_helper",
							"fields": map[string]any{
								"问题编号": "ISS-0003",
								"问题标题": "需要协助补充信息",
								"问题状态": issueTrackerStatusHelperPending,
								"负责人（问题源头负责人）": []map[string]any{{"id": "ou_owner", "name": "负责人甲"}},
								"协助人": []map[string]any{{"id": "ou_helper", "name": "协助乙"}},
								"创建时间": now.Add(-90 * time.Minute).UnixMilli(),
							},
						},
						{
							"record_id": "rec_verify",
							"fields": map[string]any{
								"问题编号": "ISS-0004",
								"问题标题": "已解决待验证",
								"问题状态": issueTrackerStatusPendingVerify,
								"负责人（问题源头负责人）": []map[string]any{{"id": "ou_owner", "name": "负责人甲"}},
								"创建人": map[string]any{"id": "ou_creator", "name": "创建丙"},
								"创建时间": now.Add(-30 * time.Minute).UnixMilli(),
							},
						},
						{
							"record_id": "rec_closed",
							"fields": map[string]any{
								"问题编号": "ISS-0001",
								"问题标题": "已关闭不应提醒",
								"问题状态": issueTrackerStatusClosed,
								"负责人（问题源头负责人）": []map[string]any{{"id": "ou_owner", "name": "负责人甲"}},
								"创建时间": now.Add(-24 * time.Hour).UnixMilli(),
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
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_issue"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer feishu.Close()

	s := &server{
		feishuAppID:            "cli_test",
		feishuAppSecret:        "secret",
		feishuChatID:           "default-chat",
		feishuAPIBase:          feishu.URL,
		client:                 feishu.Client(),
		issueTrackerReminderAt: map[string]time.Time{},
	}
	s.issueTracker = &issueTrackerClient{
		server: s,
		cfg: issueTrackerConfig{
			Enabled:       true,
			AppToken:      "app-issue",
			TableID:       "tbl_issue",
			ChatID:        "oc_issue_chat",
			Interval:      time.Hour,
			Cooldown:      time.Hour,
			Title:         defaultIssueTrackerTitle,
			URL:           defaultIssueTrackerRecordURL,
			NumberField:   "问题编号",
			TitleField:    "问题标题",
			DescField:     "问题描述",
			StatusField:   "问题状态",
			AssigneeField: "负责人（问题源头负责人）",
			HelperField:   "协助人",
			CreatorField:  "创建人",
			CreatedField:  "创建时间",
			Statuses: []string{
				issueTrackerStatusOwnerPending,
				issueTrackerStatusHelperPending,
				issueTrackerStatusPendingVerify,
			},
		},
	}

	if err := s.runIssueTrackerReminderOnce(context.Background(), now); err != nil {
		t.Fatalf("runIssueTrackerReminderOnce: %v", err)
	}
	msg := findSentFeishuMessage(t, collectSentFeishuMessages(t, sentCh, 1), "chat_id", "oc_issue_chat")
	content := fmt.Sprint(msg["content"])
	if strings.Contains(content, `"schema":"2.0"`) {
		t.Fatalf("issue tracker card must use the compatible legacy schema: %s", content)
	}
	for _, want := range []string{"非缺陷问题", "ISS-0002", "需要产品确认范围", "ou_owner", "ou_helper", "ou_creator", "打开非缺陷问题表"} {
		if !strings.Contains(content, want) {
			t.Fatalf("issue tracker card missing %q: %s", want, content)
		}
	}
	for _, notWant := range []string{"已关闭不应提醒", "我已处理"} {
		if strings.Contains(content, notWant) {
			t.Fatalf("issue tracker card should not contain %q: %s", notWant, content)
		}
	}

	// cooldown should suppress immediate second send
	if err := s.runIssueTrackerReminderOnce(context.Background(), now.Add(10*time.Minute)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	select {
	case sent := <-sentCh:
		t.Fatalf("unexpected second send during cooldown: %+v", sent)
	case <-time.After(200 * time.Millisecond):
	}
}
